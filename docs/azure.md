# Run previews on Azure

With `-provider azure`, the agent creates one virtual machine for each preview and one for each build worker through Azure Resource Manager, and drives them over SSH. [agent.md](agent.md) explains what runs on each. `elprovo setup azure` creates everything the agent needs; see [setup.md](setup.md#azure).

## Start the agent

```sh
export AZURE_TENANT_ID=<tenant id> AZURE_CLIENT_ID=<client id> AZURE_CLIENT_SECRET=<client secret>
go run ./cmd/elprovo-agent run -provider azure \
  -azure-subscription <subscription id> -azure-resource-group <group> -azure-location westeurope \
  -azure-subnet <subnet id> -azure-registry <registry id> \
  -registry <registry>.azurecr.io \
  -enrollment-token <agent token>
```

The agent signs in as a service principal with the client credentials grant. It reads the login endpoint from the metadata document of `-azure-arm-url`, as Azure tools do for any cloud, so the same flag points it at Azure, a sovereign cloud or a simulation.

Before each build and each start, the agent creates a repository-scoped token in the container registry and a password for it that expires after two hours: the token can push for a build worker and only pull for a preview machine. The machine's Docker signs in with it. Each set of credentials gets its own token, because a token has only two passwords and jobs run at the same time, and the agent removes its expired tokens. The service principal never reaches a machine, and the registry's admin user stays off.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-azure-arm-url` | `https://management.azure.com` | Resource Manager endpoint. Point it at a local simulation for tests. |
| `-azure-subscription` | none | Subscription id |
| `-azure-resource-group` | none | Resource group that the machines go in |
| `-azure-location` | none | Location of the machines, such as `westeurope` |
| `-azure-subnet` | none | Id of the subnet that the machines join. Its network security group decides who reaches them. |
| `-azure-size` | `Standard_B2s` | Virtual machine size |
| `-azure-image` | `Canonical:ubuntu-24_04-lts:server:latest` | Platform image as `publisher:offer:sku:version`. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new machine. |
| `-azure-golden-image` | none | Id of a Elprovo managed image with Docker already installed, from [images/azure](../images/README.md#azure). When set, machines start from it instead of `-azure-image` and install nothing. |
| `-azure-registry` | none | Id of the container registry that machines get token credentials for. Without it, machines sign in to no registry unless `-registry-username` is set. |
| `-azure-tag` | `elprovo-preview` | Value of the `elprovo` tag on every machine the agent creates. The agent lists and deletes only machines with it. Setup passes `<environment>-preview`. |

The service principal stays on the agent host. Pass it only through the environment.

## What the agent creates

- For each preview, a network interface `elprovo-<preview id>-nic` in the subnet, and a virtual machine `elprovo-<preview id>` on it with an OS disk `elprovo-<preview id>-osdisk`. Both carry the tag, and the machine has no public IP.
- Up to `-build-workers` machines named `elprovo-build-<n>` the same way. They stay when previews come and go.
- A host key for each machine. The agent generates it, gives it to the machine in its custom data, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to a machine that presents that key, as root, on its private address.
- The admin user `elprovo`, which Azure requires, with the agent's key. The custom data keeps root logins with the same key (`disable_root: false`), because the agent drives the machine as root.
- Firewall rules on each machine that reject new connections from containers to the instance metadata service at `169.254.169.254`, and to Azure's wireserver at `168.63.129.16` on every port but 53. The wireserver serves the VM's goal state and extension settings. The Azure Linux agent's `OS.EnableFirewall` already lets only the agent talk to it from the VM itself, but its rules do not cover connections that Docker forwards from containers. Port 53 stays open because the wireserver is also the VM's DNS server: containers on Docker's default bridge, such as `docker build` steps, query it directly, while Compose networks resolve through Docker's embedded server, which forwards from the host when the host runs systemd-resolved. The rules sit in Docker's `DOCKER-USER` chain and are added on every boot, before Docker starts.
- Registry tokens named `elprovo-job-<random>`, removed after their passwords expire.

The virtual machine deletes its OS disk and network interface with it. Delete removes the virtual machine, then the network interface if it is left from a failed create, and waits until Azure answers 404 for both rather than trusting the delete's answer.

## Network access

Machines take the network security group of their subnet. The bootstrap module's group lets in TCP only from the agent's and the gateway's private addresses and denies the rest of the virtual network, which Azure's default rules would let in, so one preview cannot reach another or the agent host.

Machines have no public IP. They reach registries and package mirrors through Azure's default outbound access, which the module turns on explicitly for the preview subnet (`default_outbound_access_enabled = true`): virtual networks made with Azure API versions released after March 31, 2026 get private subnets unless they say otherwise. Default outbound access gives each machine an address that Microsoft owns and may change; when your registry or firewall needs fixed source addresses, put a NAT gateway on the preview subnet instead. The hosts' subnet is private, and the hosts reach out through their own public IPs.

## A role for the agent

Setup applies the module with a service principal that can create resource groups and registries, and by default gives the agent the same one. The agent needs much less, and only in the environment's resource group:

```json
{
  "Name": "Elprovo agent",
  "Description": "Creates, drives and deletes Elprovo preview machines and makes short-lived registry tokens in one environment.",
  "Actions": [
    "Microsoft.Resources/subscriptions/resourceGroups/read",
    "Microsoft.Compute/virtualMachines/read",
    "Microsoft.Compute/virtualMachines/write",
    "Microsoft.Compute/virtualMachines/delete",
    "Microsoft.Compute/disks/read",
    "Microsoft.Compute/disks/write",
    "Microsoft.Compute/disks/delete",
    "Microsoft.Compute/images/read",
    "Microsoft.Compute/locations/operations/read",
    "Microsoft.Compute/locations/diskOperations/read",
    "Microsoft.Network/networkInterfaces/read",
    "Microsoft.Network/networkInterfaces/write",
    "Microsoft.Network/networkInterfaces/delete",
    "Microsoft.Network/networkInterfaces/join/action",
    "Microsoft.Network/virtualNetworks/subnets/read",
    "Microsoft.Network/virtualNetworks/subnets/join/action",
    "Microsoft.Network/locations/operations/read",
    "Microsoft.Network/locations/operationResults/read",
    "Microsoft.ContainerRegistry/registries/read",
    "Microsoft.ContainerRegistry/registries/scopeMaps/read",
    "Microsoft.ContainerRegistry/registries/tokens/read",
    "Microsoft.ContainerRegistry/registries/tokens/write",
    "Microsoft.ContainerRegistry/registries/tokens/delete",
    "Microsoft.ContainerRegistry/registries/tokens/operationStatuses/read",
    "Microsoft.ContainerRegistry/registries/generateCredentials/action",
    "Microsoft.ContainerRegistry/locations/operationResults/read"
  ],
  "NotActions": [],
  "AssignableScopes": ["/subscriptions/<subscription id>"]
}
```

It covers the virtual machines, their OS disks and network interfaces, joining the preview subnet, reading a golden image, the registry tokens and their passwords, and polling the long-running operations those calls start. To use it instead of Contributor:

1. Create the role once: `az role definition create --role-definition elprovo-agent.json`.
2. Create a service principal for the agent without a role: `az ad sp create-for-rbac --name elprovo-agent-production`.
3. Run `elprovo setup azure` with that principal in `ARM_AGENT_CLIENT_ID` and `ARM_AGENT_CLIENT_SECRET`, next to the setup principal in `ARM_*`. Setup gives the agent only the agent principal.
4. Assign the role on the resource group that setup made: `az role assignment create --assignee <agent client id> --role "Elprovo agent" --scope /subscriptions/<subscription id>/resourceGroups/<environment>-elprovo`. Assign it on the golden image's resource group too, when the image lives elsewhere.

The agent then cannot touch anything outside the environment, and a compromised agent host exposes only that resource group.
