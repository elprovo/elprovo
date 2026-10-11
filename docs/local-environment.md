# Run the agent and the gateway on a Mac

In the local environment, each stagelet and each build worker is a Lima VM on your Mac. The agent and an image registry run on the Mac itself. The gateway runs in its own Lima VM, as it runs on its own host in a cloud.

The gateway cannot run on the Mac as a background service. macOS Local Network privacy blocks background programs that Apple did not sign from reaching other machines on the local network, and stagelet VMs count as such machines. The agent is not affected, because it reaches VMs through `limactl` and runs its health checks inside the stagelet VM.

Every VM is on Lima's `user-v2` network. On it, VMs reach each other, and they reach the Mac as `host.lima.internal`. Lima's `vzNAT` network does not work here: a `vzNAT` VM is reachable from the Mac but not from other VMs. `user-v2` runs in user space and needs no root setup.

On `user-v2`, `host.lima.internal` leads to the Mac's loopback address, so it reaches services that listen only on `127.0.0.1`. The agent therefore makes Docker in each VM it creates reject connections that containers open to the VM's own network: the other VMs and the Mac's loopback services. Containers keep DNS and the internet, and the VM itself still pulls images from the registry on the Mac. Unlike a cloud firewall, this does not stop processes that run on a VM outside Docker. Use the local environment only for testing.

`stagely setup lima` does everything on this page for you; see [setup.md](setup.md). The steps below show what it does, for running the daemons by hand.

## Requirements

- macOS on Apple Silicon, with Lima 2.0 or newer (`limactl` on `PATH`).
- `mkcert`, for the gateway's local TLS certificate.
- Docker on the Mac, for the image registry.
- A running Stagely control plane, plus one enrollment token for the agent and one for the gateway.
- `stagely-gateway` built for Linux on your Mac's CPU: `make build-hosts` writes it to `bin/linux-arm64/`.

## Create the gateway certificate

```sh
mkcert -install
mkdir -p ~/.stagely/gateway
mkcert -cert-file ~/.stagely/gateway/tls.crt -key-file ~/.stagely/gateway/tls.key '*.stagelets.localhost'
```

`mkcert -install` adds the mkcert certificate authority to your browsers, once per Mac. Tools such as `curl` can use it directly with `--cacert "$(mkcert -CAROOT)/rootCA.pem"`.

## Start a registry

Build workers push images to a registry, and stagelet VMs pull them by digest. Run one on the Mac. VMs reach it at `host.lima.internal:5050`:

```sh
docker run -d --name stagely-registry --restart unless-stopped -p 5050:5000 registry:2
```

The registry speaks plain HTTP, so the agent tells Docker in each new VM to trust it with `-lima-insecure-registry`. Use this only for local testing.

## Start the agent

```sh
go run ./cmd/stagely-agent run -control-plane ws://localhost:8080/v1/connect -enrollment-token <agent token> \
  -registry host.lima.internal:5050/stagely -lima-insecure-registry host.lima.internal:5050
```

The agent needs its enrollment token only on the first start. After that, the key in `~/.stagely/agent/` identifies it.

## Start the gateway in a VM

Create the gateway's VM on the `user-v2` network, where it reaches stagelet VMs. Lima forwards each port the gateway listens on to the same port on the Mac's `127.0.0.1`:

```sh
limactl create --tty=false --name=stagely-gateway --vm-type=vz --network=lima:user-v2 --cpus=1 --memory=1 --disk=10 \
  --set='.mounts = []' template:default
limactl start stagely-gateway
```

Copy the binary and the certificate into it, then run the gateway. Inside a VM, the Mac's `localhost` is `host.lima.internal`:

```sh
limactl cp bin/linux-arm64/stagely-gateway ~/.stagely/gateway/tls.crt ~/.stagely/gateway/tls.key stagely-gateway:/tmp/
limactl shell stagely-gateway -- /tmp/stagely-gateway run -control-plane ws://host.lima.internal:8080/v1/connect \
  -data-dir /tmp/stagely-gateway-data -enrollment-token <gateway token> -tls-cert /tmp/tls.crt -tls-key /tmp/tls.key
```

Stagelets then open at `https://<stagelet id>.stagelets.localhost:8443` on the Mac.

## What the agent creates

For each stagelet, the agent creates a Lima VM named `stagely-<stagelet id>`, and for each build worker one named `stagely-build-<n>`:

- `vz` backend on Lima's `user-v2` network, where the gateway VM reaches it by IP address.
- Plain mode: no host folders are mounted, and no ports are forwarded to the Mac. Traffic reaches the stagelet only through the gateway.
- Docker from the `docker-rootful` template, with a `docker.service` drop-in that runs `/usr/local/sbin/stagely-container-isolation` after each start. The script fills Docker's `DOCKER-USER` chain with the rules above.

The agent downloads the commit's source on the Mac and copies it into a build worker. No VM receives a GitHub token, and stagelet VMs never receive the source.

## Run a second environment

To test a branch next to another environment on the same Mac, give the second agent its own control plane URL, data directory, VM name prefix and registry path, and give the second gateway its own VM and port:

```sh
go run ./cmd/stagely-agent run -control-plane ws://localhost:18080/v1/connect \
  -data-dir ~/.stagely/dev-agent -machine-prefix stagely-dev- -enrollment-token <token> \
  -registry host.lima.internal:5050/stagely-dev -lima-insecure-registry host.lima.internal:5050
limactl shell stagely-dev-gateway -- /tmp/stagely-gateway run -control-plane ws://host.lima.internal:18080/v1/connect \
  -data-dir /tmp/stagely-gateway-data -listen :18443 -enrollment-token <token> -tls-cert /tmp/tls.crt -tls-key /tmp/tls.key
```
