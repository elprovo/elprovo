# stagely

Public. The parts of Stagely that run outside the Stagely service: the agent and the gateway in the customer's cloud, and the `stagely` CLI on an operator's machine.

## Layout

| Path | Holds |
| --- | --- |
| `cmd/stagely-agent/` | Agent daemon, `stagely-agent run`, and the `stagely-agent secrets` admin commands. Wires the agent's dependencies. |
| `cmd/stagely-gateway/` | Gateway daemon, `stagely-gateway run`. Wires the gateway's dependencies. |
| `cmd/stagely/` | Operator CLI: parses the `login`, `whoami`, `setup` and `logout` commands and prints their results |
| `internal/agent/` | Job runner: takes leased jobs, runs their steps, streams steps and logs with secret values hidden, reports results under the lease, keeps jobs across reconnects for adoption, owns the build-worker pool and reports the machine inventory. [docs/agent.md](docs/agent.md) describes it. |
| `internal/agent/provider/` | One adapter per cloud that creates, finds and deletes machines. Today `lima`, for local testing on a Mac, `digitalocean`, `aws`, `gcp`, `hetzner`, `vultr` and `azure` exist. |
| `internal/agent/build/` | Builds a commit on a build worker: the agent downloads the source and copies it in, so the worker never receives a GitHub token. Resolves the Compose project as an unprivileged user, checks it against the supported Compose subset, builds and pushes images, and returns them by digest. |
| `internal/agent/runtime/` | Compiles the deploy bundle from a build, applies it on the stagelet machine with images pulled by digest and secrets from a root-only file, runs the migration command and checks the health path |
| `internal/agent/secrets/` | Encrypted local secret store and pipeline secrets file |
| `internal/agent/journal/` | SQLite record of what the agent has done: machine create and delete intents, and leased jobs with their results, used to recover after a restart |
| `internal/gateway/` | TLS ingress, routes, handoff exchange, stagelet sessions and route health checks. It gets a certificate for each stagelet host through ACME, or reads one certificate from files. See [docs/gateway.md](docs/gateway.md). |
| `internal/controlplane/` | Enrollment and the WebSocket session to the control plane, shared by the agent and the gateway |
| `internal/setup/` | Cloud bootstrap: makes sure the environment exists on the control plane, applies the cloud's module with OpenTofu or Terraform, then installs the agent and the gateway over SSH as systemd services and enrolls them, leaving one of each. [docs/setup.md](docs/setup.md) describes it. Today: DigitalOcean, AWS, Google Cloud, Hetzner Cloud, Vultr, Azure, and Lima on a Mac with the agent under launchd and the gateway in its own Lima VM. |
| `internal/cloudinit/` | Cloud-init user data that gives a new machine an SSH host key its creator pinned |
| `internal/sshrun/` | Runs commands on a machine over SSH as root, trusting only its pinned host key; pins the host keys of the machines the cloud providers create |
| `internal/login/` | Operator login through the control plane's OAuth server (PKCE with a loopback redirect, a token for the JSON API), local token storage, and an API session that refreshes the token |
| `protocol/` | Session protocol types. A separate Go module, tagged `protocol/vX.Y.Z`. The Stagely control plane imports it. |
| `sim/` | `stagely-sim`: local simulations of GitHub, Stripe and the clouds behind their real API contracts. A separate Go module that no product binary imports. See [sim/README.md](sim/README.md). |
| `terraform/` | Bootstrap modules that `stagely setup` applies, one folder per cloud, embedded in the CLI. Today: `digitalocean`, `aws`, `google-cloud`, `hetzner`, `vultr`, `azure` and `lima`. |
| `images/` | Golden image builds, one Packer template per cloud. Today: `digitalocean`, `aws`, `google-cloud`, `hetzner`, `vultr` and `azure`. See [images/README.md](images/README.md). |
| `docs/` | User docs |
| `docs/context/` | Docs that describe the current code of this repository |

Target clouds: DigitalOcean, AWS, Google Cloud, Hetzner, Azure and Vultr, plus a local adapter for testing on macOS. The local adapter is `internal/agent/provider/lima`; [docs/local-environment.md](docs/local-environment.md) explains how to run it. [docs/digitalocean.md](docs/digitalocean.md) explains the DigitalOcean adapter, [docs/aws.md](docs/aws.md) the AWS adapter, [docs/google-cloud.md](docs/google-cloud.md) the Google Cloud adapter, [docs/hetzner.md](docs/hetzner.md) the Hetzner Cloud adapter, [docs/vultr.md](docs/vultr.md) the Vultr adapter, and [docs/azure.md](docs/azure.md) the Azure adapter.

## Commands

The Makefile is the command interface. Tools, including Go, are pinned in `mise.toml`: run `mise install` once, then use `make`:

```sh
make fmt     # format Go source with gofumpt and goimports
make lint    # static analysis and the import rules
make test    # unit tests for the root module and protocol
make check   # every local quality gate: format, lint, race tests, module tidiness, vulnerabilities, build
make test-acme  # the gateway's ACME integration test against Pebble; needs Docker, not part of make check
make dist       # the release archives of stagely, stagely-agent and stagely-gateway for each platform, with checksums, in dist/
```

GitHub Actions CI is disabled; `make check` is the gate. The only workflow that runs is `.github/workflows/release.yml`: a tag `vX.Y.Z` publishes the `make dist` archives as a GitHub release. Run it before you push, and push to `main`. The targets cover all three modules: the root, `protocol` and `sim`. Do not install Go tools globally; add or bump them in `mise.toml`. 

## Dependency direction

- `cmd/stagely-agent` → `agent`, `controlplane`, and the agent subpackages it wires in: `agent/build`, `agent/journal`, `agent/provider/*` and `agent/secrets`
- `cmd/stagely-gateway` → `gateway`, `controlplane`
- `cmd/stagely` → `setup`, `login`, `terraform`
- `setup`, `agent/provider/digitalocean`, `agent/provider/aws`, `agent/provider/gcp`, `agent/provider/hetzner`, `agent/provider/vultr` and `agent/provider/azure` → `cloudinit`, `sshrun`
- `agent` → its subpackages and `protocol`
- `gateway` → `controlplane` → `protocol`
- `setup` → `login`

Within these edges:

- `agent` uses its subpackages. The subpackages do not import `agent` or each other; the runner passes data between them. Extract shared code only when two of them need it.
- `gateway` never imports `agent` or its subpackages, so the gateway binary contains no code that reads credentials or secrets.
- `protocol` imports nothing from `internal/`.
- `sim` is its own module: product code never imports it, and it imports nothing from the root module.

`depguard` in `.golangci.yml` enforces these rules. Change both files together.

## Rules

- The agent holds cloud credentials and application secrets. The gateway holds neither. Do not give the gateway access to them.
- The agent accepts no inbound connections. The agent and the gateway each open an outbound WebSocket session to the control plane.
- No Stagely code runs on build-worker or runtime VMs. The agent drives them over SSH.
- The control plane accepts a range of protocol versions, from its oldest to `protocol.Version`, and refuses others with a message that names the range. Until the first release `protocol.Version` stays 1. Keep message changes compatible, with new fields optional: the control plane's checks run the agent and gateway of the previous `protocol/vX.Y.Z` tag against it.
- The agent, gateway and CLI release together under one tag.
- This repository becomes public at release, so customers can read the code that runs next to their cloud credentials. Do not add private plans, customer data or links to private repositories.
