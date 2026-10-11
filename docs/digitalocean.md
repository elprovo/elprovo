# Run stagelets on DigitalOcean

With `-provider digitalocean`, the agent creates one droplet for each stagelet and one for each build worker through the DigitalOcean API, and drives them over SSH. [agent.md](agent.md) explains what runs on each.

## Start the agent

```sh
export DIGITALOCEAN_TOKEN=<token>
go run ./cmd/stagely-agent run -provider digitalocean -do-region <region> \
  -registry registry.digitalocean.com/<registry name> \
  -enrollment-token <agent token>
```

Before each build and each start, the agent asks the API for Docker credentials for DigitalOcean Container Registry that expire after two hours: read and write for a build worker, read only for a stagelet droplet. The machine's Docker signs in with them, so the API token never reaches a droplet.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-do-api-url` | `https://api.digitalocean.com` | API base URL. Point it at a local simulation for tests. |
| `-do-region` | none | Region of the droplets, such as `fra1` |
| `-do-size` | `s-2vcpu-4gb` | Droplet size |
| `-do-image` | `ubuntu-24-04-x64` | Stock droplet image. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new droplet. |
| `-do-golden-image` | none | A Stagely golden image (snapshot id) with Docker already installed. When set, droplets start from it instead of `-do-image` and install nothing. |
| `-do-vpc` | none | VPC UUID. Droplets join it, and the agent reaches them on their private address. Without it, the agent uses their public address. |
| `-do-tag` | `stagely-stagelet` | Tag on every droplet the agent creates. The agent finds its droplets by this tag. Setup passes `<environment>-stagelet`, which the bootstrap module creates. |

The token stays on the agent host. Pass it only through `DIGITALOCEAN_TOKEN`.

## What the agent creates

- One SSH key, `stagely-agent <fingerprint>`, registered once from `<data-dir>/ssh.key`.
- For each stagelet, a droplet named `stagely-<stagelet id>` with the tag. Cloud-init installs Docker and the Compose plugin.
- Up to `-build-workers` droplets named `stagely-build-<n>` with the tag. They stay when stagelets come and go.
- A host key for each droplet. The agent generates it, gives it to the droplet through cloud-init, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to a droplet that presents that key.
- A firewall rule on each droplet that rejects new connections from containers to the metadata service at `169.254.169.254`. The metadata service serves the droplet's user data, which holds the host key, and the droplet runs customer code in containers. The rule sits in Docker's `DOCKER-USER` chain and is added on every boot, before Docker starts.

The agent downloads the commit's source itself and copies it to a build worker, so no droplet receives a GitHub token, and stagelet droplets never receive the source. Delete removes the droplet and waits until DigitalOcean no longer lists it.

## Network access

The agent does not create firewalls. Put a DigitalOcean cloud firewall on the tag that lets in only the agent and the gateway, and allows outbound traffic for packages and images.
