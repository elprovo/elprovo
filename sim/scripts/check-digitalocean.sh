#!/bin/sh
# Checks the DigitalOcean simulation against real clients. It needs Docker
# and Go, and OpenTofu (the repository's pinned tofu) for the terraform check. Run it through mise from the
# repository root:
#
#   mise exec -- sim/scripts/check-digitalocean.sh agent
#   mise exec -- sim/scripts/check-digitalocean.sh terraform
#   mise exec -- sim/scripts/check-digitalocean.sh image
#
# agent runs the agent's DigitalOcean provider test
# (internal/agent/provider/digitalocean, build tag simdocker) in a container
# on the simulation's private and public Docker networks, because droplet
# addresses are reachable only from there.
#
# image builds images/digitalocean with Packer against the simulation, which
# snapshots the build droplet, then runs the agent check on a droplet booted
# from that snapshot.
#
# terraform applies sim/internal/digitalocean/testdata/terraform with the
# official provider through OpenTofu, checks that the wildcard DNS record
# points at the reserved IP and that a second plan has no changes, destroys
# it, and checks that no machine or domain is left.
#
# Each run uses its own data directory, Docker networks and port, and
# removes them when it ends.
set -eu

check=${1:-}
case "$check" in
agent | terraform | image) ;;
*)
	echo "usage: $0 agent|terraform|image" >&2
	exit 2
	;;
esac

repo=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
network="stagely-sim-check-$$"
port=${STAGELY_SIM_CHECK_PORT:-9390}
token="dop_v1_check_$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
sim_pid=""

account() {
	sed -n 's/.*"uuid": "\([^"]*\)".*/\1/p' "$work/data/digitalocean/state.json" 2>/dev/null | head -n 1
}

machines_left() {
	docker ps --all --quiet --filter "label=stagely-sim.account=$(account)" | wc -l | tr -d ' '
}

cleanup() {
	status=$?
	if [ -n "$sim_pid" ]; then
		kill "$sim_pid" 2>/dev/null || true
		wait "$sim_pid" 2>/dev/null || true
	fi
	docker rm --force "$network-agent" "$network-packer" >/dev/null 2>&1 || true
	if [ -n "$(account)" ]; then
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^stagely-sim-do-snapshot:$(account | cut -c1-8)-" | xargs docker rmi --force >/dev/null 2>&1 || true
	fi
	if [ -n "$(account)" ]; then
		docker ps --all --quiet --filter "label=stagely-sim.account=$(account)" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
	fi
	docker network rm "$network" "$network-public" >/dev/null 2>&1 || true
	if [ "$status" -ne 0 ] && [ -f "$work/sim.log" ]; then
		echo "--- simulation log" >&2
		tail -n 50 "$work/sim.log" >&2
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

start_simulation() {
	(cd "$repo/sim" && go build -o "$work/stagely-sim" ./cmd/stagely-sim)
	"$work/stagely-sim" -digitalocean -data-dir "$work/data" -do-network "$network" \
		-do-addr ":$port" -do-token "$token" >"$work/sim.log" 2>&1 &
	sim_pid=$!
	tries=0
	until curl -fs -o /dev/null -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v2/account"; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "the simulation did not start" >&2
			exit 1
		fi
		sleep 1
	done
}

check_agent() {
	arch=$(docker version --format '{{.Server.Arch}}')
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags simdocker \
		-o "$work/provider.test" ./internal/agent/provider/digitalocean)
	docker create --name "$network-agent" --network "$network" --network "$network-public" --add-host stagely-sim-host:host-gateway \
		-e STAGELY_SIM_DO_URL="http://stagely-sim-host:$port" -e STAGELY_SIM_DO_TOKEN="$token" -e STAGELY_SIM_DO_GOLDEN_IMAGE="${golden_image:-}" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its droplet" >&2
		exit 1
	fi
	echo "agent check passed"
}

check_terraform() {
	cp -R "$repo/sim/internal/digitalocean/testdata/terraform" "$work/tf"
	ssh-keygen -q -t ed25519 -N '' -C stagely-sim-check -f "$work/key"
	TF_VAR_ssh_public_key=$(cat "$work/key.pub")
	export TF_IN_AUTOMATION=1 TF_INPUT=0 TF_VAR_api_endpoint="http://127.0.0.1:$port" TF_VAR_token="$token" TF_VAR_ssh_public_key
	cd "$work/tf"
	tofu init -no-color >/dev/null
	tofu apply -auto-approve -no-color
	if [ "$(machines_left)" != 2 ]; then
		echo "expected 2 machines after apply, found $(machines_left)" >&2
		exit 1
	fi
	records="http://127.0.0.1:$port/v2/domains/stagely-sim-check.test/records?type=A&name=*.stagelets.stagely-sim-check.test"
	if ! curl -fsS -H "Authorization: Bearer $token" "$records" | grep -q "\"data\":\"$(tofu output -raw reserved_ip)\""; then
		echo "the wildcard record does not point at the reserved IP" >&2
		exit 1
	fi
	tofu plan -detailed-exitcode -no-color
	tofu destroy -auto-approve -no-color
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after destroy" >&2
		exit 1
	fi
	if curl -fs -o /dev/null -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v2/domains/stagely-sim-check.test"; then
		echo "the domain is left after destroy" >&2
		exit 1
	fi
	echo "terraform check passed"
}

check_image() {
	docker create --name "$network-packer" --network "$network" --network "$network-public" --add-host stagely-sim-host:host-gateway \
		-e DIGITALOCEAN_TOKEN="$token" -w /images --entrypoint sh hashicorp/packer:1.16.1 \
		-c "packer init . && packer build -color=false -var region=nyc3 -var api_url=http://stagely-sim-host:$port -var snapshot_name=stagely-image-check ." >/dev/null
	docker cp "$repo/images/digitalocean/." "$network-packer:/images/"
	docker start --attach "$network-packer"
	golden_image=$(curl -fs -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v2/images?private=true" |
		python3 -c 'import json, sys; print(next(i["id"] for i in json.load(sys.stdin)["images"] if i["name"] == "stagely-image-check"))')
	if [ "$(machines_left)" != 0 ]; then
		echo "Packer left its build droplet" >&2
		exit 1
	fi
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
