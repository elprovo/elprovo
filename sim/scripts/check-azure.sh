#!/bin/sh
# Checks the Azure simulation against real clients. It needs Docker, Go and
# openssl. Run it through mise from the repository root:
#
#   mise exec -- sim/scripts/check-azure.sh agent
#   mise exec -- sim/scripts/check-azure.sh terraform
#   mise exec -- sim/scripts/check-azure.sh image
#
# The simulation serves HTTPS with a certificate from a CA that the run
# makes, because the Terraform provider reads the cloud's metadata document
# only over HTTPS. Each client runs in a container that trusts the CA and
# reaches the simulation as elprovo-sim-host through host-gateway.
#
# agent runs the agent's Azure provider test (internal/agent/provider/azure,
# build tag simdocker) in a container on the simulation's private and
# public Docker networks, because virtual machine addresses are reachable
# only from there.
#
# terraform applies the bootstrap module terraform/azure with the official
# provider through OpenTofu, checks that a second plan has no changes,
# destroys it, and checks that no machine is left.
#
# image builds images/azure with Packer against the simulation, which
# captures the build virtual machine as a managed image, then runs the agent
# check on a virtual machine booted from that image.
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
network="elprovo-sim-azcheck-$$"
port=${ELPROVO_SIM_CHECK_PORT:-9395}
tenant=0000a2e0-0000-4000-8000-0000000000c1
subscription=0000a2e0-0000-4000-8000-0000000000c2
client_id=0000a2e0-0000-4000-8000-0000000000c3
client_secret="check-$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
arm_url="https://elprovo-sim-host:$port"
sim_pid=""

account() {
	sed -n 's/.*"account": "\([^"]*\)".*/\1/p' "$work/data/azure/state.json" 2>/dev/null | head -n 1
}

machines_left() {
	docker ps --all --quiet --filter "label=elprovo-sim.account=$(account)" | wc -l | tr -d ' '
}

cleanup() {
	status=$?
	if [ -n "$sim_pid" ]; then
		kill "$sim_pid" 2>/dev/null || true
		wait "$sim_pid" 2>/dev/null || true
	fi
	docker rm --force "$network-agent" "$network-packer" "$network-tofu" >/dev/null 2>&1 || true
	if [ -n "$(account)" ]; then
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^elprovo-sim-az-image:$(account | cut -c1-8)-" | xargs docker rmi --force >/dev/null 2>&1 || true
		docker ps --all --quiet --filter "label=elprovo-sim.account=$(account)" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
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

make_certificates() {
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=elprovo-sim-check-ca \
		-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign \
		-keyout "$work/ca.key" -out "$work/ca.pem" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj /CN=elprovo-sim-host \
		-keyout "$work/tls.key" -out "$work/tls.csr" 2>/dev/null
	printf 'subjectAltName=DNS:elprovo-sim-host,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' >"$work/tls.ext"
	openssl x509 -req -in "$work/tls.csr" -CA "$work/ca.pem" -CAkey "$work/ca.key" -CAcreateserial -days 1 \
		-extfile "$work/tls.ext" -out "$work/tls.pem" 2>/dev/null
}

start_simulation() {
	make_certificates
	(cd "$repo/sim" && go build -o "$work/elprovo-sim" ./cmd/elprovo-sim)
	"$work/elprovo-sim" -azure -data-dir "$work/data" -azure-network "$network" -azure-addr ":$port" \
		-azure-tls-cert "$work/tls.pem" -azure-tls-key "$work/tls.key" -azure-tenant "$tenant" -azure-subscription "$subscription" \
		-azure-client-id "$client_id" -azure-client-secret "$client_secret" >"$work/sim.log" 2>&1 &
	sim_pid=$!
	tries=0
	until curl -fs -o /dev/null --cacert "$work/ca.pem" "https://127.0.0.1:$port/metadata/endpoints?api-version=2022-09-01"; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "the simulation did not start" >&2
			exit 1
		fi
		sleep 1
	done
}

# client_env prints docker create flags that give a client the
# simulation's credentials and a trust store with the check's CA.
client_env() {
	printf '%s\n' --add-host elprovo-sim-host:host-gateway \
		-e "ARM_TENANT_ID=$tenant" -e "ARM_SUBSCRIPTION_ID=$subscription" -e "ARM_CLIENT_ID=$client_id" -e "ARM_CLIENT_SECRET=$client_secret" \
		-e SSL_CERT_FILE=/etc/elprovo-sim-check/ca-bundle.pem
}

# copy_trust puts the system CA bundle of the container's image, plus the
# check's CA, where SSL_CERT_FILE points.
copy_trust() {
	container=$1
	image=$2
	docker run --rm --entrypoint cat "$image" /etc/ssl/certs/ca-certificates.crt >"$work/ca-bundle.pem" 2>/dev/null || : >"$work/ca-bundle.pem"
	cat "$work/ca.pem" >>"$work/ca-bundle.pem"
	mkdir -p "$work/trust"
	cp "$work/ca-bundle.pem" "$work/trust/ca-bundle.pem"
	docker cp "$work/trust" "$container:/etc/elprovo-sim-check"
}

check_agent() {
	arch=$(docker version --format '{{.Server.Arch}}')
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags simdocker \
		-o "$work/provider.test" ./internal/agent/provider/azure)
	# shellcheck disable=SC2046
	docker create --name "$network-agent" --network "$network" --network "$network-public" $(client_env) \
		-e ELPROVO_SIM_AZURE_URL="$arm_url" -e ELPROVO_SIM_AZURE_GOLDEN_IMAGE="${golden_image:-}" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	copy_trust "$network-agent" ubuntu:24.04
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its virtual machine" >&2
		exit 1
	fi
	echo "agent check passed"
}

tofu_run() {
	docker exec -w /module "$network-tofu" tofu "$@"
}

check_terraform() {
	ssh-keygen -q -t ed25519 -N '' -C elprovo-sim-check -f "$work/key"
	image=ghcr.io/opentofu/opentofu:1.12.6
	# shellcheck disable=SC2046
	docker create --name "$network-tofu" $(client_env) -e TF_IN_AUTOMATION=1 -e TF_INPUT=0 \
		-e TF_VAR_metadata_host="elprovo-sim-host:$port" -e TF_VAR_name=azcheck -e TF_VAR_location=westeurope \
		-e TF_VAR_registry_name="elprovocheck$$" -e TF_VAR_operator_cidrs='["0.0.0.0/0"]' \
		-e TF_VAR_ssh_public_key="$(cat "$work/key.pub")" \
		-e TF_VAR_agent_user_data="$(printf '#cloud-config\nwrite_files:\n  - path: /etc/elprovo-check\n    content: agent\n')" \
		--entrypoint sleep "$image" 3600 >/dev/null
	mkdir -p "$work/module"
	cp "$repo"/terraform/azure/*.tf "$repo/terraform/azure/.terraform.lock.hcl" "$work/module/"
	docker cp "$work/module" "$network-tofu:/module"
	copy_trust "$network-tofu" "$image"
	docker start "$network-tofu" >/dev/null
	tofu_run init -no-color >/dev/null
	tofu_run apply -auto-approve -no-color
	if [ "$(machines_left)" != 2 ]; then
		echo "expected 2 machines after apply, found $(machines_left)" >&2
		exit 1
	fi
	tofu_run plan -detailed-exitcode -no-color
	tofu_run destroy -auto-approve -no-color
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after destroy" >&2
		exit 1
	fi
	echo "terraform check passed"
}

check_image() {
	image=hashicorp/packer:1.16.1
	# shellcheck disable=SC2046
	docker create --name "$network-packer" --network "$network" --network "$network-public" $(client_env) -w /images \
		--entrypoint sh "$image" -c "packer init . && packer build -color=false -var metadata_host=$arm_url -var location=westeurope \
		-var image_resource_group=elprovo-images -var image_name=elprovo-image-check ." >/dev/null
	docker cp "$repo/images/azure/." "$network-packer:/images/"
	copy_trust "$network-packer" "$image"
	token=$(curl -fs --cacert "$work/ca.pem" -d grant_type=client_credentials -d "client_id=$client_id" --data-urlencode "client_secret=$client_secret" \
		--data-urlencode "scope=https://127.0.0.1:$port/.default" "https://127.0.0.1:$port/$tenant/oauth2/v2.0/token" |
		python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])')
	curl -fs -o /dev/null --cacert "$work/ca.pem" -X PUT -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
		-d '{"location": "westeurope"}' "https://127.0.0.1:$port/subscriptions/$subscription/resourceGroups/elprovo-images?api-version=2023-07-01"
	docker start --attach "$network-packer"
	golden_image="/subscriptions/$subscription/resourceGroups/elprovo-images/providers/Microsoft.Compute/images/elprovo-image-check"
	if [ "$(machines_left)" != 0 ]; then
		echo "Packer left its build virtual machine" >&2
		exit 1
	fi
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
