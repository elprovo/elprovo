#!/bin/sh
# Checks the Hetzner Cloud simulation against real clients. It needs Docker
# and Go, and OpenTofu (the repository's pinned tofu) for the terraform
# check. Run it through mise from the repository root:
#
#   mise exec -- sim/scripts/check-hetzner.sh agent
#   mise exec -- sim/scripts/check-hetzner.sh terraform
#   mise exec -- sim/scripts/check-hetzner.sh image
#
# agent runs the agent's Hetzner provider test
# (internal/agent/provider/hetzner, build tag simdocker) in a container on
# the simulation's private and public Docker networks, because server
# addresses are reachable only from there. The private network's range is
# the Hetzner network's that the test makes.
#
# terraform applies the bootstrap module terraform/hetzner, then
# sim/internal/hetzner/testdata/terraform, with the official provider
# through OpenTofu. For each, it checks that a second plan has no changes,
# destroys it, and checks that no machine is left.
#
# image builds images/hetzner with Packer against the simulation, which
# snapshots the build server, then runs the agent check on a server booted
# from that snapshot.
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
network="elprovo-sim-hz-check-$$"
port=${ELPROVO_SIM_CHECK_PORT:-9391}
# Hetzner tokens, and so the ones the Terraform provider accepts, are 64
# characters long.
token="elprovo-sim-check-$(od -An -N23 -tx1 /dev/urandom | tr -d ' \n')"
sim_pid=""

project() {
	sed -n 's/.*"uuid": "\([^"]*\)".*/\1/p' "$work/data/hetzner/state.json" 2>/dev/null | head -n 1
}

machines_left() {
	docker ps --all --quiet --filter "label=elprovo-sim.account=$(project)" | wc -l | tr -d ' '
}

cleanup() {
	status=$?
	if [ -n "$sim_pid" ]; then
		kill "$sim_pid" 2>/dev/null || true
		wait "$sim_pid" 2>/dev/null || true
	fi
	docker rm --force "$network-agent" "$network-packer" >/dev/null 2>&1 || true
	if [ -n "$(project)" ]; then
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^elprovo-sim-hz-snapshot:$(project | cut -c1-8)-" | xargs docker rmi --force >/dev/null 2>&1 || true
		docker ps --all --quiet --filter "label=elprovo-sim.account=$(project)" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
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
	(cd "$repo/sim" && go build -o "$work/elprovo-sim" ./cmd/elprovo-sim)
	"$work/elprovo-sim" -hetzner -data-dir "$work/data" -hcloud-network "$network" \
		-hcloud-addr ":$port" -hcloud-token "$token" >"$work/sim.log" 2>&1 &
	sim_pid=$!
	tries=0
	until curl -fs -o /dev/null -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/locations"; do
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
		-o "$work/provider.test" ./internal/agent/provider/hetzner)
	private_range=$(docker network inspect "$network" --format '{{(index .IPAM.Config 0).Subnet}}')
	docker create --name "$network-agent" --network "$network" --network "$network-public" --add-host elprovo-sim-host:host-gateway \
		-e ELPROVO_SIM_HCLOUD_URL="http://elprovo-sim-host:$port/v1" -e ELPROVO_SIM_HCLOUD_TOKEN="$token" \
		-e ELPROVO_SIM_HCLOUD_NETWORK_RANGE="$private_range" \
		-e ELPROVO_SIM_HCLOUD_GOLDEN_IMAGE="${golden_image:-}" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its server" >&2
		exit 1
	fi
	echo "agent check passed"
}

# apply_module applies the module in $1 with the variables in the
# environment, checks that it made $2 machines and that a second plan has
# no changes, destroys it, and checks that no machine is left.
apply_module() {
	rm -rf "$work/tf"
	cp -R "$1" "$work/tf"
	(
		cd "$work/tf"
		tofu init -no-color >/dev/null
		tofu apply -auto-approve -no-color
		if [ "$(machines_left)" != "$2" ]; then
			echo "expected $2 machines after apply, found $(machines_left)" >&2
			exit 1
		fi
		tofu plan -detailed-exitcode -no-color
		tofu destroy -auto-approve -no-color
	)
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after destroy" >&2
		exit 1
	fi
}

check_terraform() {
	ssh-keygen -q -t ed25519 -N '' -C elprovo-sim-check -f "$work/key"
	TF_VAR_ssh_public_key=$(cat "$work/key.pub")
	TF_VAR_agent_user_data=$(printf '#cloud-config\nwrite_files:\n  - path: /etc/elprovo-check\n    content: "agent\\n"\n')
	TF_VAR_gateway_user_data=$(printf '#cloud-config\nwrite_files:\n  - path: /etc/elprovo-check\n    content: "gateway\\n"\n')
	export TF_IN_AUTOMATION=1 TF_INPUT=0 HCLOUD_TOKEN="$token" TF_VAR_endpoint="http://127.0.0.1:$port/v1"
	export TF_VAR_ssh_public_key TF_VAR_agent_user_data TF_VAR_gateway_user_data
	export TF_VAR_name=hzcheck TF_VAR_location=fsn1 TF_VAR_operator_cidrs='["0.0.0.0/0"]' TF_VAR_network_range=10.42.0.0/20
	apply_module "$repo/terraform/hetzner" 2
	echo "bootstrap module check passed"
	apply_module "$repo/sim/internal/hetzner/testdata/terraform" 2
	echo "terraform check passed"
}

check_image() {
	docker create --name "$network-packer" --network "$network" --network "$network-public" --add-host elprovo-sim-host:host-gateway \
		-e HCLOUD_TOKEN="$token" -w /images --entrypoint sh hashicorp/packer:1.16.1 \
		-c "packer init . && packer build -color=false -var location=fsn1 -var endpoint=http://elprovo-sim-host:$port/v1 -var snapshot_name=elprovo-image-check ." >/dev/null
	docker cp "$repo/images/hetzner/." "$network-packer:/images/"
	docker start --attach "$network-packer"
	golden_image=$(curl -fs -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/images?type=snapshot" |
		python3 -c 'import json, sys; print(next(i["id"] for i in json.load(sys.stdin)["images"] if i["description"] == "elprovo-image-check"))')
	if [ "$(machines_left)" != 0 ]; then
		echo "Packer left its build server" >&2
		exit 1
	fi
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
