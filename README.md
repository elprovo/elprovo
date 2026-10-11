# Stagely

Stagely gives every GitHub pull request a stagelet: its own private, running copy of the application, built and run in your own cloud account.

This repository holds the parts of Stagely that run outside the Stagely service:

- `stagely-agent` runs in your cloud. It provisions machines, builds images and runs stagelets. It holds your cloud credentials and application secrets, and it accepts no inbound connections.
- `stagely-gateway` runs in your cloud. It serves stagelet traffic and holds no credentials.
- `stagely` runs on your machine. It sets up your cloud account and enrolls the agent and the gateway.

Status: early development, not released.

| Read | To learn |
| --- | --- |
| [docs/setup.md](docs/setup.md) | How `stagely login` and `stagely setup` bootstrap DigitalOcean, AWS, Google Cloud, Azure, Hetzner Cloud or Vultr, or the local environment on a Mac |
| [docs/agent.md](docs/agent.md) | What the agent does for a deploy: build workers, compiled bundles, migrations, reset, secrets, logs, its journal and inventory |
| [docs/stagely-yml.md](docs/stagely-yml.md) | The `.stagely.yml` file and the supported Compose subset |
| [docs/gateway.md](docs/gateway.md) | The gateway: ACME certificates, routes, handoffs and fail-closed sessions |
| [docs/digitalocean.md](docs/digitalocean.md) | The DigitalOcean adapter |
| [docs/aws.md](docs/aws.md) | The AWS adapter |
| [docs/google-cloud.md](docs/google-cloud.md) | The Google Cloud adapter, and the registry accounts it signs machines in with |
| [docs/hetzner.md](docs/hetzner.md) | The Hetzner Cloud adapter, and the container registry it uses |
| [docs/vultr.md](docs/vultr.md) | The Vultr adapter |
| [docs/azure.md](docs/azure.md) | The Azure adapter |
| [docs/local-environment.md](docs/local-environment.md) | Running stagelets as Lima VMs on a Mac |
| [images/README.md](images/README.md) | Golden images |
| [sim/README.md](sim/README.md) | Local simulations of GitHub, Stripe and the clouds, for development and tests |

## Validate this repository

You need a Mac with Apple Silicon, [mise](https://mise.jdx.dev) and Docker with Compose. Run the commands from the repository root.

1. Install the pinned tools:

   ```sh
   mise install
   ```

2. Run the quality gate. It checks formatting, lint, unit tests, race tests, the Go modules, known vulnerabilities, the Terraform modules and the Packer templates, and builds all binaries:

   ```sh
   mise exec -- make check
   ```

3. Run the ACME test of the gateway against Pebble in Docker:

   ```sh
   mise exec -- make test-acme
   ```

4. Run the simulation checks of each cloud. The `agent` mode runs the cloud's adapter against its simulation. The `terraform` mode applies the bootstrap module with OpenTofu. The `image` mode builds the golden image with Packer:

   ```sh
   for cloud in digitalocean hetzner vultr aws gcp azure; do
     for mode in agent terraform image; do
       mise exec -- sim/scripts/check-$cloud.sh $mode || { echo "FAILED: $cloud $mode"; break 2; }
     done
   done
   ```

Each command must end with exit code 0. Each simulation check must print `agent check passed`, `terraform check passed` or `image check passed`. [sim/README.md](sim/README.md) describes what each check proves.

Layout and working rules: [AGENTS.md](AGENTS.md).
