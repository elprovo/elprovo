#!/bin/sh
# Checks the Vultr simulation against real clients. It needs Docker, Go and
# openssl. Run it through mise from the repository root:
#
#   mise exec -- sim/scripts/check-vultr.sh agent
#   mise exec -- sim/scripts/check-vultr.sh terraform
#   mise exec -- sim/scripts/check-vultr.sh image
#
# The Vultr Terraform provider and Packer plugin always call
# https://api.vultr.com. So the simulation runs in a container on the public
# network under that name, with a certificate from a CA this run makes, and
# every client runs in a container on the same network that trusts the CA.
#
# agent runs the agent's Vultr provider test (internal/agent/provider/vultr,
# build tag simdocker) in a container on the private and public networks,
# because instance addresses are reachable only from there. The private
# network's range is the VPC's that the test makes.
#
# terraform applies sim/internal/vultr/testdata/terraform and then the
# bootstrap module terraform/vultr with the official provider through
# OpenTofu, checks that a second plan of each has no changes, destroys them,
# and checks that no machine is left.
#
# image builds images/vultr with Packer against the simulation, which
# snapshots the build instance, then runs the agent check on an instance
# booted from that snapshot.
#
# Each run uses its own data directory, Docker networks, containers and port,
# and removes them when it ends.
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
network="stagely-sim-vultr-check-$$"
port=${STAGELY_SIM_CHECK_PORT:-9390}
key="CHECK$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | tr a-f A-F)"
arch=$(docker version --format '{{.Server.Arch}}')
account=""

machines_left() {
	docker ps --all --quiet --filter "label=stagely-sim.cloud=vultr" --filter "label=stagely-sim.account=$account" | wc -l | tr -d ' '
}

# wait_no_machines waits until the machines of instances that a client
# deleted are gone: Vultr deletes instances in the background, and neither
# the Terraform provider nor Packer waits for it.
wait_no_machines() {
	tries=0
	until [ "$(machines_left)" = 0 ]; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "$1: $(machines_left) machines are left" >&2
			exit 1
		fi
		sleep 1
	done
}

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "--- simulation log" >&2
		docker logs --tail 50 "$network-sim" >&2 2>&1 || true
	fi
	docker rm --force "$network-sim" "$network-agent" "$network-tofu" "$network-packer" >/dev/null 2>&1 || true
	if [ -n "$account" ]; then
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^stagely-sim-vultr-snapshot:$(echo "$account" | cut -c1-8)-" | xargs docker rmi --force >/dev/null 2>&1 || true
		docker ps --all --quiet --filter "label=stagely-sim.account=$account" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
	fi
	docker network rm "$network" "$network-public" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# make_certs makes the run's CA and a certificate for api.vultr.com.
make_certs() {
	mkdir -p "$work/certs"
	(
		cd "$work/certs"
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
			-subj "/CN=stagely-sim Vultr check CA" -keyout ca.key -out ca.pem \
			-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null
		openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
			-subj "/CN=api.vultr.com" -keyout api.key -out api.csr 2>/dev/null
		printf 'subjectAltName=DNS:api.vultr.com\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\n' >api.ext
		openssl x509 -req -in api.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -extfile api.ext -out api.crt 2>/dev/null
		rm ca.key api.csr api.ext
	)
}

api() {
	curl -fs --cacert "$work/certs/ca.pem" --resolve "api.vultr.com:$port:127.0.0.1" \
		-H "Authorization: Bearer $key" "https://api.vultr.com:$port$1"
}

start_simulation() {
	(cd "$repo/sim" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$work/stagely-sim" ./cmd/stagely-sim)
	make_certs
	docker network create --label stagely-sim.network=true "$network" >/dev/null
	docker network create --label stagely-sim.network=true "$network-public" >/dev/null
	docker create --name "$network-sim" --network "$network-public" --network-alias api.vultr.com \
		--publish "127.0.0.1:$port:443" --volume /var/run/docker.sock:/var/run/docker.sock \
		--entrypoint /stagely-sim docker:29.8.1-cli \
		-data-dir /data -vultr -vultr-addr :443 -vultr-network "$network" -vultr-api-key "$key" \
		-vultr-tls-cert /certs/api.crt -vultr-tls-key /certs/api.key >/dev/null
	docker cp "$work/stagely-sim" "$network-sim:/stagely-sim"
	docker cp "$work/certs/." "$network-sim:/certs/"
	docker start "$network-sim" >/dev/null
	tries=0
	until api /v2/account >/dev/null; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "the simulation did not start" >&2
			exit 1
		fi
		sleep 1
	done
	account=$(docker exec "$network-sim" cat /data/vultr/state.json | sed -n 's/^  "id": "\([^"]*\)".*/\1/p')
}

check_agent() {
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags simdocker \
		-o "$work/provider.test" ./internal/agent/provider/vultr)
	private_range=$(docker network inspect "$network" --format '{{(index .IPAM.Config 0).Subnet}}')
	docker create --name "$network-agent" --network "$network" --network "$network-public" \
		-e VULTR_API_KEY="$key" -e STAGELY_SIM_VULTR_GOLDEN_IMAGE="${golden_image:-}" -e SSL_CERT_FILE=/ca.pem \
		-e STAGELY_SIM_VULTR_NETWORK_RANGE="$private_range" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	docker cp "$work/certs/ca.pem" "$network-agent:/ca.pem"
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its instance" >&2
		exit 1
	fi
	echo "agent check passed"
}

# tofu runs OpenTofu in the directory $1 under /work in the tofu container.
tofu_in() {
	dir=$1
	shift
	docker exec -w "/work/$dir" "$network-tofu" tofu "$@"
}

# apply_twice applies the module in $1, checks that it made $2 machines and
# that a second plan has no changes, then destroys it.
apply_twice() {
	tofu_in "$1" init -no-color >/dev/null
	tofu_in "$1" apply -auto-approve -no-color
	if [ "$(machines_left)" != "$2" ]; then
		echo "expected $2 machines after applying $1, found $(machines_left)" >&2
		exit 1
	fi
	tofu_in "$1" plan -detailed-exitcode -no-color
	tofu_in "$1" destroy -auto-approve -no-color
	wait_no_machines "after destroying $1"
}

check_terraform() {
	mkdir -p "$work/tofu"
	cp -R "$repo/sim/internal/vultr/testdata/terraform" "$work/tofu/testdata"
	cp -R "$repo/terraform/vultr" "$work/tofu/module"
	ssh-keygen -q -t ed25519 -N '' -C stagely-sim-check -f "$work/key"
	public_key=$(cat "$work/key.pub")
	cat >"$work/tofu/testdata/terraform.tfvars.json" <<EOF
{"ssh_public_key": "$public_key"}
EOF
	# The variables stagely setup passes, with user data like the host keys
	# it pins.
	cat >"$work/tofu/module/terraform.tfvars.json" <<EOF
{
  "name": "check-$$",
  "region": "ewr",
  "ssh_public_key": "$public_key",
  "operator_cidrs": ["203.0.113.7/32", "2001:db8::/48"],
  "network_range": "10.42.0.0/20",
  "agent_user_data": "#cloud-config\nssh_genkeytypes: []\n",
  "gateway_user_data": "#cloud-config\nssh_genkeytypes: []\n",
  "registry_name": "check$$"
}
EOF
	cp "$work/certs/ca.pem" "$work/tofu/ca.pem"
	docker create --name "$network-tofu" --network "$network-public" \
		-e VULTR_API_KEY="$key" -e TF_IN_AUTOMATION=1 -e TF_INPUT=0 -e SSL_CERT_FILE=/work/bundle.pem \
		--entrypoint sleep ghcr.io/opentofu/opentofu:1.12.6 infinity >/dev/null
	docker cp "$work/tofu/." "$network-tofu:/work/"
	docker start "$network-tofu" >/dev/null
	docker exec "$network-tofu" sh -c 'cat /etc/ssl/certs/ca-certificates.crt /work/ca.pem >/work/bundle.pem'
	apply_twice testdata 2
	apply_twice module 2
	echo "terraform check passed"
}

check_image() {
	docker create --name "$network-packer" --network "$network" --network "$network-public" \
		-e VULTR_API_KEY="$key" -w /images --entrypoint sh hashicorp/packer:1.16.1 \
		-c 'cat /etc/ssl/certs/ca-certificates.crt /ca.pem >/tmp/bundle.pem && export SSL_CERT_FILE=/tmp/bundle.pem &&
			packer init . && packer build -color=false -var region=ewr -var snapshot_description=stagely-image-check .' >/dev/null
	docker cp "$repo/images/vultr/." "$network-packer:/images/"
	docker cp "$work/certs/ca.pem" "$network-packer:/ca.pem"
	docker start --attach "$network-packer"
	golden_image=$(api "/v2/snapshots?description=stagely-image-check" |
		python3 -c 'import json, sys; print(next(s["id"] for s in json.load(sys.stdin)["snapshots"] if s["status"] == "complete"))')
	wait_no_machines "after Packer deleted its build instance"
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
