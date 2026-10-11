# Stagely

Stagely gives every GitHub pull request a stagelet: its own private, running copy of the application, built and run in your own cloud account.

This repository holds the parts of Stagely that run outside the Stagely service:

- `stagely-agent` runs in your cloud. It provisions machines, builds images and runs stagelets. It holds your cloud credentials and application secrets, and it accepts no inbound connections.
- `stagely-gateway` runs in your cloud. It serves stagelet traffic and holds no credentials.
- `stagely` runs on your machine. It sets up your cloud account and enrolls the agent and the gateway.

Status: early development, not released.

| Read | To learn |
| --- | --- |
| [docs/setup.md](docs/setup.md) | How `stagely login` and `stagely setup` bootstrap DigitalOcean, AWS, Google Cloud, Hetzner Cloud or Vultr, or the local environment on a Mac |
| [docs/agent.md](docs/agent.md) | What the agent does for a deploy: build workers, compiled bundles, migrations, reset, secrets, logs, its journal and inventory |
| [docs/stagely-yml.md](docs/stagely-yml.md) | The `.stagely.yml` file and the supported Compose subset |
| [docs/gateway.md](docs/gateway.md) | The gateway: ACME certificates, routes, handoffs and fail-closed sessions |
| [docs/digitalocean.md](docs/digitalocean.md) | The DigitalOcean adapter |
| [docs/aws.md](docs/aws.md) | The AWS adapter |
| [docs/google-cloud.md](docs/google-cloud.md) | The Google Cloud adapter, and the registry accounts it signs machines in with |
| [docs/hetzner.md](docs/hetzner.md) | The Hetzner Cloud adapter, and the container registry it uses |
| [docs/vultr.md](docs/vultr.md) | The Vultr adapter |
| [docs/local-environment.md](docs/local-environment.md) | Running stagelets as Lima VMs on a Mac |
| [images/README.md](images/README.md) | Golden images |
| [sim/README.md](sim/README.md) | Local simulations of GitHub, Stripe and the clouds, for development and tests |

Layout and working rules: [AGENTS.md](AGENTS.md).
