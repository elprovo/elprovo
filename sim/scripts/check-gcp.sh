#!/bin/sh
# Checks the Google Cloud simulation against real clients. It needs Docker
# and Go, and OpenTofu (the repository's pinned tofu) for the terraform
# check. Run it through mise from the repository root:
#
#   mise exec -- sim/scripts/check-gcp.sh agent
#   mise exec -- sim/scripts/check-gcp.sh terraform
#   mise exec -- sim/scripts/check-gcp.sh image
#
# agent runs the agent's Google Cloud provider test
# (internal/agent/provider/gcp, build tag simdocker) in a container on the
# simulation's internal and external Docker networks, because instance
# addresses are reachable only from there. A CNCF Distribution registry
# answers as us-central1-docker.pkg.dev on the external network, with a
# certificate from the check's CA, which the machines trust, and with the
# simulation as its token service; the test pushes and pulls an image there
# with the tokens of registry service accounts.
#
# terraform applies the bootstrap module terraform/google-cloud with the
# official provider through OpenTofu, checks that a second plan has no
# changes, destroys it, and checks that no machine is left.
#
# image builds images/google-cloud with Packer against the simulation, which
# keeps the build instance's disk and makes an image of it, then runs the
# agent check on an instance booted from that image.
#
# Each run uses its own data directory and Docker networks, and port 9393
# (STAGELY_SIM_CHECK_PORT changes it), and removes all of it when it ends.
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
network="stagely-sim-gcp-check-$$"
port=${STAGELY_SIM_CHECK_PORT:-9393}
project=stagely-sim
registry_host=us-central1-docker.pkg.dev
token="ya29.check-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
sim_pid=""

account() {
	sed -n 's/.*"account": "\([^"]*\)".*/\1/p' "$work/data/gcp/state.json" 2>/dev/null | head -n 1
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
	docker rm --force --volumes "$network-agent" "$network-packer" "$network-ssh" "$network-registry" >/dev/null 2>&1 || true
	if [ -n "$(account)" ]; then
		docker ps --all --quiet --filter "label=stagely-sim.account=$(account)" | xargs docker rm --force --volumes >/dev/null 2>&1 || true
		docker images --format '{{.Repository}}:{{.Tag}}' | grep "^stagely-sim-gcp-disk:$(account | cut -c1-8)-" | xargs docker rmi --force >/dev/null 2>&1 || true
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

api() {
	method=$1
	path=$2
	shift 2
	curl -fsS -X "$method" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' "$@" \
		"http://127.0.0.1:$port/compute/v1/projects/$project/$path"
}

# key_file writes the simulation's service account key file with the token
# URI that a client at the given address reaches.
key_file() {
	python3 -c 'import json, sys
key = json.load(open(sys.argv[1]))
key["token_uri"] = sys.argv[2]
json.dump(key, open(sys.argv[3], "w"))' "$work/data/gcp/service-account.json" "$1" "$2"
}

# make_certs makes the check's CA, which every machine trusts, and the
# registry's certificate.
make_certs() {
	mkdir -p "$work/certs"
	cd "$work/certs"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=stagely-sim-gcp-check \
		-keyout ca.key -out ca.pem 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj /CN=$registry_host \
		-keyout registry.key -out registry.csr 2>/dev/null
	echo "subjectAltName=DNS:$registry_host" >registry.ext
	openssl x509 -req -in registry.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 1 -extfile registry.ext \
		-out registry.pem 2>/dev/null
	cd "$repo"
}

start_simulation() {
	(cd "$repo/sim" && go build -o "$work/stagely-sim" ./cmd/stagely-sim)
	make_certs
	"$work/stagely-sim" -gcp -data-dir "$work/data" -gcp-network "$network" -gcp-project "$project" \
		-gcp-addr ":$port" -gcp-access-token "$token" -machine-ca "$work/certs/ca.pem" >"$work/sim.log" 2>&1 &
	sim_pid=$!
	tries=0
	until api GET zones -o /dev/null 2>/dev/null; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "the simulation did not start" >&2
			exit 1
		fi
		sleep 1
	done
}

# start_registry runs the registry that Artifact Registry's host stands for.
# Docker inside the machines asks the simulation for tokens at the Docker
# host's address.
start_registry() {
	host_ip=$(docker run --rm --add-host stagely-sim-host:host-gateway ubuntu:24.04 getent hosts stagely-sim-host | cut -d' ' -f1)
	cp "$work/data/gcp/registry-token.pem" "$work/certs/token.pem"
	docker create --name "$network-registry" --network "$network-public" --network-alias "$registry_host" \
		-e REGISTRY_HTTP_ADDR=:443 -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.pem -e REGISTRY_HTTP_TLS_KEY=/certs/registry.key \
		-e REGISTRY_AUTH=token -e REGISTRY_AUTH_TOKEN_REALM="http://$host_ip:$port/v2/token" -e REGISTRY_AUTH_TOKEN_SERVICE="$registry_host" \
		-e REGISTRY_AUTH_TOKEN_ISSUER=stagely-sim -e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/token.pem \
		registry:2.8.3 >/dev/null
	docker cp "$work/certs" "$network-registry:/certs"
	docker start "$network-registry" >/dev/null
}

check_agent() {
	start_registry
	arch=$(docker version --format '{{.Server.Arch}}')
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags simdocker \
		-o "$work/provider.test" ./internal/agent/provider/gcp)
	key_file "http://stagely-sim-host:$port/token" "$work/agent-key.json"
	docker create --name "$network-agent" --network "$network" --network "$network-public" --add-host stagely-sim-host:host-gateway \
		-e STAGELY_SIM_GCP_ENDPOINT="http://stagely-sim-host:$port/" -e STAGELY_SIM_GCP_CREDENTIALS=/key.json \
		-e STAGELY_SIM_GCP_GOLDEN_IMAGE="${golden_image:-}" -e STAGELY_SIM_GCP_REGISTRY="$registry_host/$project/stagely-check" \
		ubuntu:24.04 /provider.test -test.v -test.run TestProviderAgainstSimulation -test.timeout 20m >/dev/null
	docker cp "$work/provider.test" "$network-agent:/provider.test"
	docker cp "$work/agent-key.json" "$network-agent:/key.json"
	docker start --attach "$network-agent"
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after the provider deleted its instance" >&2
		exit 1
	fi
	echo "agent check passed"
}

check_terraform() {
	cp -R "$repo/terraform/google-cloud" "$work/tf"
	ssh-keygen -q -t ed25519 -N '' -C stagely-sim-check -f "$work/key"
	ssh-keygen -q -t ed25519 -N '' -C stagely-sim-host -f "$work/host"
	key_file "http://127.0.0.1:$port/token" "$work/tf-key.json"
	GOOGLE_CREDENTIALS=$(cat "$work/tf-key.json")
	export GOOGLE_CREDENTIALS TF_IN_AUTOMATION=1 TF_INPUT=0
	# The hosts get the user data that stagely setup writes: a host key that
	# the check pins when it connects.
	python3 -c 'import json, sys
work, port, project = sys.argv[1:]
pem = open(work + "/host").read().strip().splitlines()
public = " ".join(open(work + "/host.pub").read().split()[:2])
user_data = "#cloud-config\nssh_deletekeys: true\nssh_genkeytypes: []\nssh_keys:\n  ed25519_private: |\n"
user_data += "".join("    " + line + "\n" for line in pem) + "  ed25519_public: " + public + "\n"
json.dump({
    "name": "check", "project": project, "region": "us-central1", "zone": "us-central1-a",
    "compute_endpoint": "http://127.0.0.1:%s/compute/v1/" % port, "artifact_registry_endpoint": "http://127.0.0.1:%s/v1/" % port,
    "iam_endpoint": "http://127.0.0.1:%s/v1/" % port, "agent_service_account": json.load(open(work + "/tf-key.json"))["client_email"],
    "registry_name": "stagely-check", "operator_cidrs": ["0.0.0.0/0"], "ssh_public_key": open(work + "/key.pub").read().strip(),
    "agent_user_data": user_data, "gateway_user_data": user_data,
}, open(work + "/tf/terraform.tfvars.json", "w"))' "$work" "$port" "$project"
	cd "$work/tf"
	tofu init -no-color >/dev/null
	tofu apply -auto-approve -no-color
	if [ "$(machines_left)" != 2 ]; then
		echo "expected 2 machines after apply, found $(machines_left)" >&2
		exit 1
	fi
	check_host_login "$(tofu output -raw agent_address)" "$(tofu output -raw gateway_ip)"
	check_impersonation "$(tofu output -raw registry_push_account)" "$(tofu output -raw registry_pull_account)"
	tofu plan -detailed-exitcode -no-color
	tofu destroy -auto-approve -no-color
	if [ "$(machines_left)" != 0 ]; then
		echo "machines are left after destroy" >&2
		exit 1
	fi
	echo "terraform check passed"
}

# check_host_login signs in as root with the operator key to each address,
# trusting only the host key from the user data, as stagely setup does. The
# gateway answers at its static address.
check_host_login() {
	mkdir -p "$work/ssh"
	cp "$work/key" "$work/ssh/key"
	echo "$1,$2 $(cut -d' ' -f1,2 "$work/host.pub")" >"$work/ssh/known_hosts"
	machine_image=$(docker inspect --format '{{.Config.Image}}' "$(docker ps --quiet --filter "label=stagely-sim.account=$(account)" | head -n 1)")
	for host in "$@"; do
		docker create --name "$network-ssh" --network "$network-public" --entrypoint ssh "$machine_image" \
			-i /keys/key -o UserKnownHostsFile=/keys/known_hosts -o StrictHostKeyChecking=yes -o BatchMode=yes -o ConnectTimeout=10 \
			"root@$host" 'cloud-init status --wait >/dev/null; grep -q "set_host_keys = false" /etc/default/instance_configs.cfg' >/dev/null
		docker cp "$work/ssh" "$network-ssh:/keys"
		docker start --attach "$network-ssh"
		docker rm "$network-ssh" >/dev/null
		echo "signed in to $host as root with the pinned host key"
	done
}

# owner_token signs a JWT with the key in tf-key.json, as the agent does,
# and prints the access token the simulation gives for it.
owner_token() {
	b64() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
	python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["private_key"], end="")' "$work/tf-key.json" >"$work/sa.pem"
	email=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["client_email"])' "$work/tf-key.json")
	now=$(date +%s)
	header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64)
	claims=$(printf '{"iss":"%s","scope":"https://www.googleapis.com/auth/cloud-platform","aud":"http://127.0.0.1:%s/token","iat":%s,"exp":%s}' \
		"$email" "$port" "$now" "$((now + 600))" | b64)
	signature=$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign "$work/sa.pem" | b64)
	curl -fsS -d grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer -d "assertion=$header.$claims.$signature" "http://127.0.0.1:$port/token" |
		python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])'
}

# check_impersonation checks that the module lets the agent's account get
# tokens of the registry accounts.
check_impersonation() {
	owner=$(owner_token)
	for account in "$@"; do
		curl -fsS -X POST -H "Authorization: Bearer $owner" -H 'Content-Type: application/json' -o /dev/null \
			-d '{"scope": ["https://www.googleapis.com/auth/cloud-platform"], "lifetime": "3600s"}' \
			"http://127.0.0.1:$port/v1/projects/-/serviceAccounts/$account:generateAccessToken"
		echo "the agent's account got a token of $account"
	done
}

check_image() {
	key_file "http://stagely-sim-host:$port/token" "$work/packer-key.json"
	docker create --name "$network-packer" --network "$network" --network "$network-public" --add-host stagely-sim-host:host-gateway \
		-e GOOGLE_APPLICATION_CREDENTIALS=/images/key.json -w /images --entrypoint sh hashicorp/packer:1.16.1 \
		-c "packer init . && packer build -color=false -var project_id=$project -var zone=us-central1-a -var compute_endpoint=http://stagely-sim-host:$port/compute/v1/ -var image_name=stagely-image-check ." >/dev/null
	docker cp "$repo/images/google-cloud/." "$network-packer:/images/"
	docker cp "$work/packer-key.json" "$network-packer:/images/key.json"
	docker start --attach "$network-packer"
	api GET global/images/stagely-image-check -o /dev/null
	if [ "$(machines_left)" != 0 ]; then
		echo "Packer left its build instance" >&2
		exit 1
	fi
	golden_image=stagely-image-check
	echo "built the golden image $golden_image"
	check_agent
	echo "image check passed"
}

start_simulation
"check_$check"
