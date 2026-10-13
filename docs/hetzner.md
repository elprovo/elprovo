# Run previews on Hetzner Cloud

With `-provider hetzner`, the agent creates one Hetzner Cloud server for each preview and one for each build worker through the Hetzner Cloud API, and drives them over SSH. [agent.md](agent.md) explains what runs on each. `elprovo setup hetzner` sets up a project for you; [setup.md](setup.md) describes it.

## Start the agent

```sh
export HCLOUD_TOKEN=<token>
export ELPROVO_REGISTRY_PASSWORD=<registry password>
go run ./cmd/elprovo-agent run -provider hetzner -hcloud-location fsn1 \
  -hcloud-network <network id> -hcloud-network-range 10.42.0.0/20 \
  -hcloud-network-allow <agent's address on the network>,<gateway's address on the network> \
  -registry registry.acme.dev/elprovo -registry-username <registry user> \
  -enrollment-token <agent token>
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-hcloud-api-url` | `https://api.hetzner.cloud/v1` | API base URL, with the `/v1` path. Point it at a local simulation for tests. |
| `-hcloud-location` | none | Location of the servers, such as `fsn1` |
| `-hcloud-server-type` | `cx23` | Server type |
| `-hcloud-image` | `ubuntu-24.04` | Stock image. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new server. |
| `-hcloud-golden-image` | none | A Elprovo golden image (snapshot id) with Docker already installed. When set, servers start from it instead of `-hcloud-image` and install nothing. |
| `-hcloud-label` | `elprovo=preview` | Label, as `key=value`, on every server the agent creates. The agent sees, lists and deletes only servers with this label. Setup passes `elprovo=<environment>-preview`, the label the bootstrap module's preview firewall applies to. |
| `-hcloud-network` | required | Id of the private network that every server joins. The agent drives each server, and the gateway reaches it, on its address there. |
| `-hcloud-network-range` | required | Address range of `-hcloud-network`, such as `10.42.0.0/20` |
| `-hcloud-network-allow` | required | Comma-separated addresses on the network that may connect to servers: the agent host's and the gateway host's. See [Network access](#network-access). |

The token stays on the agent host. Pass it only through `HCLOUD_TOKEN`. It needs read and write access to the project.

## The container registry

Hetzner has no managed container registry, so the agent uses one that you already run: Docker Hub, GitHub Container Registry, GitLab's registry, a cloud registry elsewhere, or CNCF Distribution (`registry:2`) on a server of your own. Build workers push to it and preview servers pull from it.

- `-registry` is the image name prefix, such as `registry.acme.dev/elprovo` or `ghcr.io/acme`.
- `-registry-username` and `ELPROVO_REGISTRY_PASSWORD` are the credentials. Use a token that can push and pull under that prefix and nothing else.

Setup does not create a registry, because Hetzner offers none to create, and running one for you would add a host to patch and store your images on a disk setup does not back up. The local simulation's checks use CNCF Distribution, the registry most self-hosted registries build on.

## What the agent creates

- One SSH key, `elprovo-agent-<fingerprint>`, registered once from `<data-dir>/ssh.key`. A key with the same fingerprint that the project has already is used as it is.
- For each preview, a server named `elprovo-<preview id>` with the label. Cloud-init installs Docker and the Compose plugin.
- Up to `-build-workers` servers named `elprovo-build-<n>` with the label. They stay when previews come and go.
- Hetzner keeps server names unique in a project, and `elprovo-` is the agent's default `-machine-prefix`. Setup gives each environment's agent the prefix `elprovo-<environment>-` from the bootstrap module, so environments can share a project.
- A host key for each server. The agent generates it, gives it to the server through cloud-init, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to a server that presents that key.
- A firewall rule on each server that rejects new connections from containers to the metadata service at `169.254.169.254`. The metadata service serves the server's user data, which holds the host key, and the server runs customer code in containers. The rule sits in Docker's `DOCKER-USER` chain and is added on every boot, before Docker starts.
- Firewall rules on each server that accept new connections from the network only from `-hcloud-network-allow`; [Network access](#network-access) describes them.

Each server is on `-hcloud-network` and has a public IPv4 address, which it uses only to reach the internet, and no IPv6 address. The agent drives the server, and the gateway reaches it, on its address on the network. Delete removes the server and waits until the API answers 404 for it. The agent downloads the commit's source itself and copies it to a build worker, so no server receives a GitHub token, and preview servers never receive the source.

## Network access

The agent host, the gateway host, the preview servers and the build workers share one private network. Hetzner's firewalls filter only public interfaces; traffic inside a private network passes them unfiltered. So each machine filters that traffic itself, with iptables rules that its user data adds on every boot, before Docker starts:

- A preview server or build worker accepts a new connection from the network only from an address in `-hcloud-network-allow`. The rules sit in the `INPUT` chain, for the server's own ports such as SSH, and in Docker's `DOCKER-USER` chain, for the ports Docker publishes, which never reach `INPUT`.
- Its containers can't open a connection to the network, so they can't reach other machines there with a forged source address either.
- The agent host and the gateway host, which setup creates, accept no new connection from the network.

So code in one preview can't reach another preview, the agent host, which holds the cloud token, or the gateway host over the network. The rules trust that a packet's source address on the network is its sender's own.

On the public interface, the bootstrap module's firewall on the preview label has no rules, so preview servers and build workers accept nothing there. They keep a public IPv4 address only to reach registries, package mirrors and the internet: Hetzner has no managed NAT gateway, a NAT server would route every preview's traffic through one more host to patch, and without IPv4, registries and services that have no IPv6 address would fail. The agent host accepts only SSH from the operator, and the gateway host 80 and 443 from anywhere and SSH from the operator.

Traffic from the gateway to a preview is plain HTTP over the private network; TLS ends at the gateway. A Hetzner network holds at most 100 servers, so with the agent host and the gateway host, an environment runs at most 98 preview servers and build workers at a time.

The gateway's stable address is a primary IP that the module keeps when the gateway server is replaced. A primary IP is the server's own address, so the server needs no network configuration for it, as a floating IP would.
