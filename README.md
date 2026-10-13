# Elprovo

Elprovo gives every GitHub pull request a preview: its own private, running copy of the application, built and run in your own cloud account.

This repository holds the parts of Elprovo that run outside the Elprovo service:

- `elprovo-agent` runs in your cloud. It provisions machines, builds images and runs previews. It holds your cloud credentials and application secrets, and it accepts no inbound connections.
- `elprovo-gateway` runs in your cloud. It serves preview traffic and holds no credentials.
- `elprovo` runs on your machine. It sets up your cloud account and enrolls the agent and the gateway.

| Read | To learn |
| --- | --- |
| [docs/setup.md](docs/setup.md) | How `elprovo login` and `elprovo setup` bootstrap DigitalOcean, AWS, Google Cloud, Azure, Hetzner Cloud or Vultr, or the local environment on a Mac |
| [docs/agent.md](docs/agent.md) | What the agent does for a deploy: build workers, compiled bundles, migrations, reset, secrets, logs, its journal and inventory |
| [docs/elprovo-yml.md](docs/elprovo-yml.md) | The `.elprovo.yml` file and the supported Compose subset |
| [docs/gateway.md](docs/gateway.md) | The gateway: ACME certificates, routes, handoffs and fail-closed sessions |
| [docs/digitalocean.md](docs/digitalocean.md) | The DigitalOcean adapter |
| [docs/aws.md](docs/aws.md) | The AWS adapter |
| [docs/google-cloud.md](docs/google-cloud.md) | The Google Cloud adapter, and the registry accounts it signs machines in with |
| [docs/hetzner.md](docs/hetzner.md) | The Hetzner Cloud adapter, and the container registry it uses |
| [docs/vultr.md](docs/vultr.md) | The Vultr adapter |
| [docs/azure.md](docs/azure.md) | The Azure adapter |
| [docs/local-environment.md](docs/local-environment.md) | Running previews as Lima VMs on a Mac |
| [images/README.md](images/README.md) | Golden images |
| [sim/README.md](sim/README.md) | Local simulations of GitHub, Stripe and the clouds, for development and tests |

## Install

Each release on the [releases page](https://github.com/elprovo/elprovo/releases) has one archive for each platform, `elprovo_<os>_<arch>.tar.gz`, for `linux` and `darwin` on `amd64` and `arm64`. Each archive holds `elprovo`, `elprovo-agent` and `elprovo-gateway`. `checksums.txt` lists their SHA-256 sums.

Download the archive for your computer to run `elprovo`, and the `linux_amd64` archive for the binaries that `elprovo setup` installs on your hosts:

```sh
version=v0.1.0
curl -fsSLO "https://github.com/elprovo/elprovo/releases/download/$version/elprovo_darwin_arm64.tar.gz"
curl -fsSLO "https://github.com/elprovo/elprovo/releases/download/$version/elprovo_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/elprovo/elprovo/releases/download/$version/checksums.txt"
shasum -a 256 --check --ignore-missing checksums.txt
mkdir -p elprovo-bin/linux-amd64
tar -xzf elprovo_darwin_arm64.tar.gz -C elprovo-bin
tar -xzf elprovo_linux_amd64.tar.gz -C elprovo-bin/linux-amd64
```

`elprovo setup <cloud> -binaries elprovo-bin/linux-amd64` then installs the agent and the gateway from that directory.

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
