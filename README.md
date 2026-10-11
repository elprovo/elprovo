# Stagely

Stagely gives every GitHub pull request a stagelet: its own private, running copy of the application, built and run in your own cloud account.

This repository holds the parts of Stagely that run outside the Stagely service:

- `stagely-agent` runs in your cloud. It provisions machines, builds images and runs stagelets. It holds your cloud credentials and application secrets, and it accepts no inbound connections.
- `stagely-gateway` runs in your cloud. It serves stagelet traffic and holds no credentials.
- `stagely` runs on your machine. It sets up your cloud account and enrolls the agent and the gateway.

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

## Install

Each release on the [releases page](https://github.com/stagely-dev/stagely/releases) has one archive for each platform, `stagely_<os>_<arch>.tar.gz`, for `linux` and `darwin` on `amd64` and `arm64`. Each archive holds `stagely`, `stagely-agent` and `stagely-gateway`. `checksums.txt` lists their SHA-256 sums.

Download the archive for your computer to run `stagely`, and the `linux_amd64` archive for the binaries that `stagely setup` installs on your hosts:

```sh
version=v0.1.0
curl -fsSLO "https://github.com/stagely-dev/stagely/releases/download/$version/stagely_darwin_arm64.tar.gz"
curl -fsSLO "https://github.com/stagely-dev/stagely/releases/download/$version/stagely_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/stagely-dev/stagely/releases/download/$version/checksums.txt"
shasum -a 256 --check --ignore-missing checksums.txt
mkdir -p stagely-bin/linux-amd64
tar -xzf stagely_darwin_arm64.tar.gz -C stagely-bin
tar -xzf stagely_linux_amd64.tar.gz -C stagely-bin/linux-amd64
```

`stagely setup <cloud> -binaries stagely-bin/linux-amd64` then installs the agent and the gateway from that directory.

## Release

Push a tag `vX.Y.Z` on `main`. `.github/workflows/release.yml` runs `make dist`, which builds the three binaries for each platform, packs them and writes `checksums.txt`, and publishes the GitHub release with them. Tags `protocol/vX.Y.Z` version the `protocol` module and publish no binaries.

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

## License

Apache License 2.0. See [LICENSE](LICENSE).
