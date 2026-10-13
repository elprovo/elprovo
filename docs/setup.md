# Set up a cloud with elprovo setup

`elprovo setup` prepares a cloud account for one Elprovo environment. It runs on your machine, once, and again whenever you want to repair or upgrade the environment. Each run converges on the same result: the environment exists in Elprovo, the cloud resources exist, and one agent and one gateway run, enrolled and connected.

Today it supports DigitalOcean, [AWS](#aws), [Google Cloud](#google-cloud), [Hetzner Cloud](#hetzner-cloud), [Vultr](#vultr), [Azure](#azure), and the local environment on a Mac with Lima.

## Before you start

- A Elprovo account, and `elprovo login` done. It signs in to Elprovo's hosted control plane at `https://app.elprovo.com`; pass `-server` to sign in to another control plane. Setup acts in the account you chose when you signed in, with your role there; creating an environment needs the admin or owner role.
- OpenTofu or Terraform on `PATH` (`tofu` is preferred).
- `elprovo-agent` and `elprovo-gateway` built for linux/amd64. From this repository: `make build-hosts`, which writes them to `bin/linux-amd64/` (and to `bin/linux-arm64/`), with a Linux build of `elprovo` itself.
- A DigitalOcean API token with write access, in `DIGITALOCEAN_TOKEN`. It stays on your machine and on the agent host only. For AWS, see [AWS](#aws), and for Hetzner Cloud, [Hetzner Cloud](#hetzner-cloud).

## Run it

```sh
export DIGITALOCEAN_TOKEN=<token>
elprovo login
elprovo setup digitalocean \
  -environment production \
  -region fra1 \
  -preview-domain previews.acme.dev \
  -registry acme-elprovo \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

At the end, setup prints the gateway's reserved IP. Point `*.previews.acme.dev` at it with an A record, or pass `-dns-domain acme.dev` when the domain is in DigitalOcean DNS and setup adds the record itself.

## What it does

1. **Environment.** It finds the environment by name in your account, or creates it with the DigitalOcean provider and the preview domain.
2. **Keys.** It creates an SSH key for itself and an SSH host key for each host, and keeps them in the work directory. The hosts get their host keys through cloud-init, so setup never trusts a key it did not make.
3. **Cloud resources.** It applies `terraform/digitalocean` with OpenTofu or Terraform:
   - a VPC for the environment;
   - the agent host and the gateway host, each on its own droplet;
   - a reserved IP for the gateway;
   - a firewall for each host: the agent host accepts only SSH from `-operator-cidr`, and the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`;
   - a firewall on the preview tag: preview machines and build workers accept traffic only from the agent and the gateway;
   - the container registry (skip it with `-create-registry=false` when the account has one), and the DNS record with `-dns-domain`.
4. **Install.** Over SSH it installs each daemon as a systemd service (`elprovo-agent`, `elprovo-gateway`) that runs as its own system user, keeps its state in `/var/lib/<name>`, and reads its credentials from `/etc/elprovo/<name>.env`, which only root can read. The unit files hold no secrets.
5. **Enroll.** When a daemon has no key yet, or the environment lists no client with its role (for example after the environment was deleted and created again), setup creates a one-time enrollment token for it. It waits until the daemon it just installed connects, as a new client or as its existing client seen again, then removes the environment's other clients of the same role, so the environment keeps one agent and one gateway.

The gateway gets certificates for preview host names through ACME (Let's Encrypt by default). Test against a staging directory with `-acme-directory`, and pass `-acme-directory-ca` for a test server such as Pebble.

## The work directory

Setup keeps the module, its variables, its state, and the keys in `~/.elprovo/cli/setup/<environment>/` (or `-work-dir`). Keep it. A later run needs the state to see the resources it made, and the keys to reach the hosts. The variables and the state hold the hosts' private host keys, so the files are readable only by you. To reach a host yourself:

```sh
ssh -i ~/.elprovo/cli/setup/production/operator_ed25519 root@<host address>
```

## The local environment on a Mac

`elprovo setup lima` sets up the environment that [local-environment.md](local-environment.md) describes, on your Mac:

```sh
make build build-hosts
elprovo login -server http://localhost:8080
elprovo setup lima
```

1. It finds or creates the environment, `local` by default, with the Lima provider and the preview domain `previews.localhost`.
2. It applies `terraform/lima`, which runs an image registry in Docker on port 5050 (macOS AirPlay often holds 5000) and makes the gateway's certificate for `*.previews.localhost` with `mkcert`. Lima VMs reach the registry at `host.lima.internal` on Lima's `user-v2` network; the agent tells Docker in each VM to trust it over plain HTTP.
3. It runs `elprovo-agent` as a launchd job of your user, `dev.elprovo.<environment>.agent`, which starts at login and restarts when it stops. Its data and log live in the work directory.
4. It creates the Lima VM `elprovo-<environment>-gateway` and installs `elprovo-gateway` in it as a systemd service, as on a cloud host, with the certificate in `/etc/elprovo`. Lima forwards the gateway's port, 8443 unless `-gateway-port` says otherwise, to the Mac's `127.0.0.1`. The gateway runs in a VM because macOS Local Network privacy blocks background programs that Apple did not sign from reaching preview VMs. Every VM is on Lima's `user-v2` network, where VMs reach each other.
5. It enrolls each daemon that is not enrolled in the environment yet, waits until it connects, and keeps one of each.

It needs Docker (for example Colima), Lima, `mkcert` (run `mkcert -install` once), and OpenTofu or Terraform. The module creates no Lima VM: the agent creates every preview VM and build worker with `limactl`, setup creates the gateway's VM with `limactl` too, and the registry runs in Docker. So the module uses the Docker provider and OpenTofu's own `terraform_data` resource, not a Lima provider.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | `local` | Environment name in Elprovo |
| `-preview-domain` | `previews.localhost` | Domain that preview host names live under |
| `-gateway-port` | `8443` | HTTPS port of the gateway on the Mac |
| `-host-address` | `host.lima.internal` | Address at which Lima VMs reach the Mac |
| `-registry-port` | `5050` | Port of the local image registry |
| `-binaries` | `bin` | Directory with `elprovo-agent` for the Mac |
| `-gateway-binary` | `bin/linux-<arch>/elprovo-gateway` | `elprovo-gateway` for Linux on the Mac's CPU, for the gateway's VM |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |

## DigitalOcean flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | required | Environment name in Elprovo |
| `-region` | required | DigitalOcean region |
| `-preview-domain` | required | Domain that preview host names live under |
| `-registry` | required | Container registry name |
| `-create-registry` | `true` | Create the registry |
| `-dns-domain` | none | Domain in DigitalOcean DNS that holds the preview domain |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for linux/amd64 |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-do-api-url` | `https://api.digitalocean.com` | DigitalOcean API base URL |
| `-golden-image` | none | Snapshot id of a golden image from `images/digitalocean`; the agent boots preview machines and build workers from it instead of the stock image |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |

## AWS

```sh
export AWS_ACCESS_KEY_ID=<access key id> AWS_SECRET_ACCESS_KEY=<secret access key>
elprovo login
elprovo setup aws \
  -environment production \
  -region eu-central-1 \
  -preview-domain previews.acme.dev \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

At the end, setup prints the gateway's Elastic IP. Point `*.previews.acme.dev` at it with an A record.

It applies `terraform/aws`, which creates:

- a VPC with an internet gateway, and one subnet whose instances get public addresses, so previews reach registries and package mirrors without a NAT gateway;
- the agent host and the gateway host, each on its own instance of the newest Ubuntu 24.04 AMI from Canonical. A later run keeps the hosts when Canonical publishes a newer AMI;
- an Elastic IP for the gateway;
- a security group for each host: the agent host accepts only SSH from `-operator-cidr`, and the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`;
- a security group for preview machines and build workers that accepts traffic only from the agent's and the gateway's groups;
- an EC2 key pair with setup's SSH key, and the line `disable_root: false` in both hosts' cloud-config, because Ubuntu AMIs refuse SSH logins as root otherwise. Both hosts require IMDSv2;
- an ECR repository creation template for create on push, so build workers can push to `<account>.dkr.ecr.<region>.amazonaws.com/<prefix>/...` repositories that do not exist yet. For each build and deploy, the agent gets a fresh ECR authorization token through temporary credentials that can push for a build worker and only pull for a preview machine; [aws.md](aws.md) explains how.

The agent keeps the access key that setup ran with, in `/etc/elprovo/elprovo-agent.env`. Use a long-lived key of an IAM user for it: temporary credentials, with `AWS_SESSION_TOKEN`, expire, and the agent then stops working. They also cannot call STS `GetFederationToken`, which the registry sign-in needs. [aws.md](aws.md) lists the permissions the agent needs.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | required | Environment name in Elprovo |
| `-region` | required | AWS region |
| `-preview-domain` | required | Domain that preview host names live under |
| `-availability-zone` | AWS chooses | Availability zone of the environment's subnet |
| `-registry-prefix` | the environment's name | ECR repository name prefix that build workers push under |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for linux/amd64 |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-aws-endpoint` | AWS | One URL for the EC2, ECR and STS APIs, such as a simulation; setup passes it to the module and the agent |
| `-golden-image` | none | AMI id of a golden image from `images/aws`; the agent boots preview machines and build workers from it instead of the stock AMI |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |

## Google Cloud

```sh
export GOOGLE_APPLICATION_CREDENTIALS=key.json
elprovo login
elprovo setup gcp \
  -environment production \
  -region us-central1 \
  -zone us-central1-a \
  -preview-domain previews.acme.dev \
  -registry elprovo \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

`key.json` is a key of a service account in the project with the roles Compute Admin (`roles/compute.admin`), Artifact Registry Administrator (`roles/artifactregistry.admin`) and Service Account Admin (`roles/iam.serviceAccountAdmin`), and the project needs the Compute Engine, Artifact Registry, IAM and IAM Service Account Credentials APIs enabled. The key stays on your machine and on the agent host only. At the end, setup prints the gateway's static address; point `*.previews.acme.dev` at it with an A record.

Setup applies `terraform/google-cloud` with the key in `GOOGLE_CREDENTIALS`:

- a custom mode network and a subnetwork for the environment, with no rule that lets in a whole address range, so preview machines reach neither each other nor the agent host;
- the agent host and the gateway host, each on its own instance in the zone, with root's SSH key from setup, project-wide SSH keys blocked, and user data that installs the host key setup pins and keeps the guest agent from replacing it;
- a static external address for the gateway;
- firewall rules on network tags: the agent host accepts only SSH from `-operator-cidr`, the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`, and preview machines and build workers accept traffic only from the agent and the gateway;
- an Artifact Registry Docker repository named `-registry` in the region;
- two service accounts that may only push to and only pull from the repository, and that the key's service account may impersonate.

The agent gets the key in base64 in its environment file. Machines never get it: the agent signs a build worker in to the registry with a one-hour token of the push account, and a preview machine with a token of the pull account. The agent names its machines `elprovo-<environment>-...`, so environments in one project and zone keep apart.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-credentials` | `$GOOGLE_APPLICATION_CREDENTIALS` | Service account key file |
| `-environment` | required | Environment name in Elprovo |
| `-project` | the key's project | Google Cloud project ID |
| `-region` | required | Region of the network, the address and the repository |
| `-zone` | required | Zone in the region of the hosts and preview machines |
| `-preview-domain` | required | Domain that preview host names live under |
| `-registry` | required | ID of the Artifact Registry Docker repository |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for linux/amd64 |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-gcp-compute-url` | Google's | Compute Engine API base URL, ending in `/compute/v1/` |
| `-gcp-artifact-registry-url` | Google's | Artifact Registry API base URL, ending in `/v1/` |
| `-gcp-iam-url` | Google's | IAM API base URL, ending in `/v1/` |
| `-gcp-iam-credentials-url` | Google's | IAM Service Account Credentials API base URL, ending in `/v1/` |
| `-golden-image` | none | Image from `images/google-cloud`; the agent boots preview machines and build workers from it instead of the stock image |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |

## Hetzner Cloud

`elprovo setup hetzner` sets up a Hetzner Cloud project the same way. It needs an API token of the project with read and write access, in `HCLOUD_TOKEN`, and a container registry you already run, because Hetzner has none: [hetzner.md](hetzner.md#the-container-registry) explains the choice.

```sh
export HCLOUD_TOKEN=<token>
export ELPROVO_REGISTRY_PASSWORD=<registry password>
elprovo login
elprovo setup hetzner \
  -environment production \
  -location fsn1 \
  -preview-domain previews.acme.dev \
  -registry registry.acme.dev/elprovo \
  -registry-username elprovo \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

At the end, setup prints the gateway's primary IP. Point `*.previews.acme.dev` at it with an A record.

It applies `terraform/hetzner`:

- a private network, `10.42.0.0/20`, for the hosts, preview machines and build workers;
- the agent host and the gateway host, each on its own server with a public IPv4 address, attached to the network;
- a primary IP for the gateway, which stays when the gateway server is replaced;
- a firewall for each host: the agent host accepts only SSH from `-operator-cidr`, and the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`;
- a firewall without rules on the label `elprovo=<environment>-preview`: preview machines and build workers accept nothing on their public address.

Hetzner's firewalls do not filter private networks, so each machine's user data adds iptables rules on every boot: the hosts accept no new connection from the network, and preview machines and build workers accept new ones only from the agent and the gateway. [hetzner.md](hetzner.md#network-access) explains them. The agent gets the label, the network, its range, the hosts' addresses on it and the machine name prefix `elprovo-<environment>-` as flags, so environments can share a project, and the token and the registry credentials in `/etc/elprovo/elprovo-agent.env`, which only root can read.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | required | Environment name in Elprovo |
| `-location` | required | Hetzner location, such as `fsn1` |
| `-preview-domain` | required | Domain that preview host names live under |
| `-registry` | required | Image name prefix in your container registry |
| `-registry-username` | none | Registry user name; the password comes from `ELPROVO_REGISTRY_PASSWORD` |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for the hosts' Linux CPU: `bin/linux-amd64` for the default `cx23` hosts |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-hcloud-api-url` | `https://api.hetzner.cloud/v1` | Hetzner Cloud API base URL, with the `/v1` path |
| `-golden-image` | none | Snapshot id of a golden image from `images/hetzner`; the agent boots preview machines and build workers from it instead of the stock image |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |

## Vultr

`elprovo setup vultr` does the same for a Vultr account, with a Vultr API key in `VULTR_API_KEY`:

```sh
export VULTR_API_KEY=<key>
elprovo setup vultr \
  -environment production \
  -region fra \
  -preview-domain previews.acme.dev \
  -registry acmeelprovo \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

It applies `terraform/vultr`, which creates:

- a VPC, `10.42.0.0/20`, for the hosts, preview machines and build workers;
- the agent host and the gateway host, each on its own instance in the VPC, each with a reserved IP as its main address, so the address stays when the host is replaced;
- a firewall group for each host: the agent host accepts only SSH from `-operator-cidr`, and the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`;
- a firewall group without rules for preview machines and build workers, which accept nothing on their public interface. The agent puts every machine it creates in it and in the VPC;
- the container registry `<region>.vultrcr.com/<registry>`. The agent gets credentials that expire after two hours for each machine from the API.

Vultr's firewall does not filter VPC traffic, so each machine's user data adds iptables rules on every boot: the hosts accept no new connection from the VPC, and preview machines and build workers accept new ones only from the agent and the gateway. [vultr.md](vultr.md#network-access) describes what can reach what. The Terraform provider and the agent call `https://api.vultr.com`. At the end, setup prints the gateway's reserved IP: point `*.previews.acme.dev` at it with an A record.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | required | Environment name in Elprovo |
| `-region` | required | Vultr region, such as `fra` |
| `-preview-domain` | required | Domain that preview host names live under |
| `-registry` | required | Name of the container registry to create. Vultr registry names are global and hold lowercase letters and digits only. |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for linux/amd64 |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-golden-image` | none | Snapshot id of a golden image from `images/vultr`; the agent boots preview machines and build workers from it instead of the stock image |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |

## Azure

`elprovo setup azure` bootstraps an Azure subscription through a service principal, which the Terraform provider and the agent use:

```sh
export ARM_TENANT_ID=<tenant id> ARM_CLIENT_ID=<client id> ARM_CLIENT_SECRET=<client secret> ARM_SUBSCRIPTION_ID=<subscription id>
elprovo login
elprovo setup azure \
  -environment production \
  -location westeurope \
  -preview-domain previews.acme.dev \
  -registry acmeelprovo \
  -operator-cidr 203.0.113.7/32 \
  -binaries bin/linux-amd64 \
  -acme-email ops@acme.dev
```

The service principal needs to create resources in the subscription; the Contributor role on it is enough. It stays on your machine and, unless you give the agent its own principal in `ARM_AGENT_CLIENT_ID` and `ARM_AGENT_CLIENT_SECRET`, on the agent host in `/etc/elprovo/elprovo-agent.env`. [azure.md](azure.md#a-role-for-the-agent) describes a role that limits the agent's principal to the environment's resource group. At the end, setup prints the gateway's static public IP. Point `*.previews.acme.dev` at it with an A record; setup does not manage Azure DNS.

Setup applies `terraform/azure`, with the provider's `metadata_host` set to the host of `-azure-arm-url`:

- a resource group `<environment>-elprovo`, and a virtual network with a private `hosts` subnet and a `previews` subnet with default outbound access turned on explicitly;
- the agent host and the gateway host, each a virtual machine with the admin user `elprovo` and a static Standard public IP; the gateway's is the stable address that preview host names point at;
- a network security group on each host's network interface: the agent host accepts only SSH from `-operator-cidr`, and the gateway host accepts 80 and 443 from anywhere and SSH from `-operator-cidr`; both deny the rest, including the virtual network;
- a network security group on the `previews` subnet: preview machines and build workers accept TCP only from the agent's and the gateway's private addresses, and nothing else from the virtual network;
- the container registry, with its admin user off: the agent gives machines short-lived token credentials instead.

Azure gives the operator's key to the admin user. The hosts' cloud-config keeps root logins with the same key, so setup installs over SSH as root, as on other clouds. The agent gets the module's resource group, preview subnet, tag and registry as flags; [azure.md](azure.md) describes them.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-environment` | required | Environment name in Elprovo |
| `-location` | required | Azure location |
| `-preview-domain` | required | Domain that preview host names live under |
| `-registry` | required | Name of the container registry to create: 5 to 50 letters and digits, unique across Azure |
| `-operator-cidr` | required | Address range that may reach the hosts over SSH; repeat for more. Use only the operators' own addresses: a range that includes the gateway host lets the gateway reach the agent host |
| `-binaries` | required | Directory with `elprovo-agent` and `elprovo-gateway` for linux/amd64 |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory` | Let's Encrypt | ACME directory URL |
| `-acme-directory-ca` | none | PEM file the gateway trusts for the ACME directory |
| `-azure-arm-url` | `https://management.azure.com` | Resource Manager endpoint whose metadata document describes the cloud; it must use HTTPS |
| `-golden-image` | none | Id of a managed image from `images/azure`; the agent boots preview machines and build workers from it instead of the stock image |
| `-tofu` | `tofu`, else `terraform` | OpenTofu or Terraform binary |
| `-work-dir` | `~/.elprovo/cli/setup/<environment>` | Work directory |
| `-ssh-port` | `22` | SSH port of the hosts |
