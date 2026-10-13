#!/bin/sh
# Checks the AWS simulation against real clients. It needs Docker, Go,
# curl with --aws-sigv4 and python3, and OpenTofu (the repository's pinned
# tofu) for the terraform check. Run it through mise from the repository root:
#
#   mise exec -- sim/scripts/check-aws.sh agent
#   mise exec -- sim/scripts/check-aws.sh terraform
#   mise exec -- sim/scripts/check-aws.sh image
#
# agent runs the agent's AWS provider test (internal/agent/provider/aws,
# build tag simdocker) in a container on the simulation's private and public
# Docker networks, because instance addresses are reachable only from there.
# A CNCF Distribution registry answers as the account's ECR registry on the
# public network, with a certificate from the check's CA, which the machines
# trust, and with the simulation as its token service; the test pushes with
# a build worker's registry credentials, pulls with a preview machine's and
# is refused a push with them.
#
# image builds images/aws with Packer against the simulation, which images
# the build instance, then runs the agent check on an instance booted from
# that AMI.
#
# terraform applies the bootstrap module terraform/aws, through
# sim/internal/aws/testdata/terraform, with the official provider and
# OpenTofu, signs in as root to the agent host and to the gateway's Elastic
# IP with the operator key, checks that a second plan has no changes,
# destroys it, and checks that no machine is left.
#
# Each run uses its own data directory, Docker networks, port and
# credentials, and removes them when it ends.
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
network="elprovo-sim-aws-check-$$"
port=${ELPROVO_SIM_CHECK_PORT:-9394}
region=us-east-1
access_key="AKIA$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n' | tr a-f A-F)"
secret_key=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')
sim_pid=""

# Keep AWS clients away from the operator's own AWS configuration.
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_EC2_METADATA_DISABLED=true
unset AWS_PROFILE AWS_SESSION_TOKEN AWS_ENDPOINT_URL 2>/dev/null || true

account() {
	sed -n 's/.*"account_id": "\([^"]*\)".*/\1/p' "$work/data/aws/state.json" 2>/dev/null | head -n 1
}

machines_left() {
	docker ps --all --quiet --filter label=elprovo-sim.cloud=aws --filter "label=elprovo-sim.account=$(account)" | wc -l | tr -d ' '
}

cleanup() {
	status=$?
	if [ -n "$sim_pid" ]; then
		kill "$sim_pid" 2>/dev/null || true
		wait "$sim_pid" 2>/dev/null || true
	fi
	docker rm --force --volumes "$network-agent" "$network-packer" "$network-ssh" "$network-registry" >/dev/null 2>&1 || true
	if [ -n "$(account)" ]; then
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^elprovo-sim-aws-image:$(account)-" | xargs docker rmi --force >/dev/null 2>&1 || true
		docker ps --all --quiet --filter label=elprovo-sim.cloud=aws --filter "label=elprovo-sim.account=$(account)" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
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

# ec2 calls the simulated EC2 Query API with the form parameters in $1.
ec2() {
	curl -fsS --aws-sigv4 "aws:amz:$region:ec2" --user "$access_key:$secret_key" \
		-H 'Content-Type: application/x-www-form-urlencoded' --data "Version=2016-11-15&$1" "http://127.0.0.1:$port/"
}

# xml_values prints the text of every element named $1 in the XML on stdin.
xml_values() {
	python3 -I -c 'import sys, xml.etree.ElementTree as ET
for e in ET.parse(sys.stdin).iter():
    if e.tag.rsplit("}", 1)[-1] == sys.argv[1]:
        print(e.text)' "$1"
}

# make_certs makes the check's CA, which every machine trusts, and the
# certificate of the registry under the account's ECR host name.
make_certs() {
	mkdir -p "$work/certs"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=elprovo-sim-aws-check \
		-keyout "$work/certs/ca.key" -out "$work/certs/ca.pem" 2>/dev/null
}

start_simulation() {
	(cd "$repo/sim" && go build -o "$work/elprovo-sim" ./cmd/elprovo-sim)
	make_certs
	"$work/elprovo-sim" -aws -data-dir "$work/data" -aws-network "$network" -aws-addr ":$port" \
		-aws-access-key-id "$access_key" -aws-secret-access-key "$secret_key" -machine-ca "$work/certs/ca.pem" >"$work/sim.log" 2>&1 &
	sim_pid=$!
	tries=0
	until ec2 "Action=DescribeRegions" >/dev/null 2>&1; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "the simulation did not start" >&2
			exit 1
		fi
		sleep 1
	done
}

# start_registry runs the registry that the account's ECR host stands for.
# Docker inside the machines asks the simulation for tokens at the Docker
# host's address.
start_registry() {
	registry_host="$(account).dkr.ecr.$region.amazonaws.com"
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=$registry_host" \
		-keyout "$work/certs/registry.key" -out "$work/certs/registry.csr" 2>/dev/null
	echo "subjectAltName=DNS:$registry_host" >"$work/certs/registry.ext"
	openssl x509 -req -in "$work/certs/registry.csr" -CA "$work/certs/ca.pem" -CAkey "$work/certs/ca.key" -CAcreateserial -days 1 \
		-extfile "$work/certs/registry.ext" -out "$work/certs/registry.pem" 2>/dev/null
	cp "$work/data/aws/registry-token.pem" "$work/certs/token.pem"
	host_ip=$(docker run --rm --add-host elprovo-sim-host:host-gateway ubuntu:24.04 getent hosts elprovo-sim-host | cut -d' ' -f1)
	docker create --name "$network-registry" --network "$network-public" --network-alias "$registry_host" \
		-e REGISTRY_HTTP_ADDR=:443 -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.pem -e REGISTRY_HTTP_TLS_KEY=/certs/registry.key \
		-e REGISTRY_AUTH=token -e REGISTRY_AUTH_TOKEN_REALM="http://$host_ip:$port/v2/token" -e REGISTRY_AUTH_TOKEN_SERVICE="$registry_host" \
		-e REGISTRY_AUTH_TOKEN_ISSUER=elprovo-sim -e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/token.pem \
		registry:2.8.3 >/dev/null
	docker cp "$work/certs" "$network-registry:/certs"
	docker start "$network-registry" >/dev/null
}

check_agent() {
	start_registry
	image=$(ec2 "Action=DescribeImages&Owner.1=099720109477&Filter.1.Name=name&Filter.1.Value.1=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" | xml_values imageId | head -n 1)
	arch=$(docker version --format '{{.Server.Arch}}')
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags simdocker \
		-o "$work/provider.test" ./internal/agent/provider/aws)
	docker create --name "$network-agent" --network "$network" --network "$network-public" --add-host elprovo-sim-host:host-gateway \
		-e ELPROVO_SIM_AWS_URL="http://elprovo-sim-host:$port" -e ELPROVO_SIM_AWS_REGION="$region" \
		-e ELPROVO_SIM_AWS_ACCESS_KEY_ID="$access_key" -e ELPROVO_SIM_AWS_SECRET_ACCESS_KEY="$secret_key" \
		-e ELPROVO_SIM_AWS_IMAGE="$image" -e ELPROVO_SIM_AWS_REGISTRY="$registry_host/elprovo-check" \
		-e ELPROVO_SIM_AWS_GOLDEN_IMAGE="${golden_image:-}" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its instance" >&2
		exit 1
	fi
	echo "agent check passed"
}

# check_root_login signs in as root with the operator key at the address $2
# from the public network, as elprovo setup does, and expects the file that
# the host's user data wrote to say $1.
check_root_login() {
	vm_image=$(docker inspect --format '{{.Config.Image}}' "$(docker ps --quiet --filter label=elprovo-sim.cloud=aws --filter "label=elprovo-sim.account=$(account)" | head -n 1)")
	docker create --name "$network-ssh" --network "$network-public" --entrypoint ssh "$vm_image" \
		-i /key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 "root@$2" \
		'cloud-init status --wait >/dev/null; cat /etc/elprovo-check' >/dev/null
	docker cp "$work/key" "$network-ssh:/key"
	tries=0
	until said=$(docker start --attach "$network-ssh" 2>/dev/null) && [ "$said" = "$1" ]; do
		tries=$((tries + 1))
		if [ "$tries" -ge 30 ]; then
			echo "could not sign in as root to the $1 host at $2: ${said:-no answer}" >&2
			exit 1
		fi
		sleep 2
	done
	docker rm --force "$network-ssh" >/dev/null
	echo "signed in as root to the $1 host at $2"
}

check_terraform() {
	cp -R "$repo/sim/internal/aws/testdata/terraform" "$work/tf"
	cp -R "$repo/terraform/aws" "$work/tf/module"
	rm -rf "$work/tf/module/.terraform"
	ssh-keygen -q -t ed25519 -N '' -C elprovo-sim-check -f "$work/key"
	TF_VAR_ssh_public_key=$(cat "$work/key.pub")
	mkdir -p "${TF_PLUGIN_CACHE_DIR:=$HOME/.cache/elprovo/tofu-plugins}"
	export TF_PLUGIN_CACHE_DIR TF_IN_AUTOMATION=1 TF_INPUT=0 TF_VAR_endpoint="http://127.0.0.1:$port" TF_VAR_ssh_public_key \
		AWS_ACCESS_KEY_ID="$access_key" AWS_SECRET_ACCESS_KEY="$secret_key" \
		AWS_ENDPOINT_URL="http://127.0.0.1:$port" AWS_EC2_METADATA_DISABLED=true
	cd "$work/tf"
	tofu init -no-color >/dev/null
	tofu apply -auto-approve -no-color
	if [ "$(machines_left)" != 2 ]; then
		echo "expected 2 machines after apply, found $(machines_left)" >&2
		exit 1
	fi
	check_root_login agent "$(tofu output -raw agent_address)"
	check_root_login gateway "$(tofu output -raw gateway_ip)"
	tofu plan -detailed-exitcode -no-color
	tofu destroy -auto-approve -no-color
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after destroy" >&2
		exit 1
	fi
	echo "terraform check passed"
}

check_image() {
	docker create --name "$network-packer" --network "$network" --network "$network-public" --add-host elprovo-sim-host:host-gateway \
		-e AWS_ACCESS_KEY_ID="$access_key" -e AWS_SECRET_ACCESS_KEY="$secret_key" \
		-e AWS_ENDPOINT_URL="http://elprovo-sim-host:$port" -e AWS_EC2_METADATA_DISABLED=true \
		-w /images --entrypoint sh hashicorp/packer:1.16.1 \
		-c "packer init . && packer build -color=false -var region=$region -var ami_name=elprovo-image-check ." >/dev/null
	docker cp "$repo/images/aws/." "$network-packer:/images/"
	docker start --attach "$network-packer"
	golden_image=$(ec2 "Action=DescribeImages&Owner.1=self&Filter.1.Name=name&Filter.1.Value.1=elprovo-image-check" | xml_values imageId)
	if [ -z "$golden_image" ]; then
		echo "Packer registered no AMI" >&2
		exit 1
	fi
	if [ "$(machines_left)" != 0 ]; then
		echo "Packer left its build instance" >&2
		exit 1
	fi
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
