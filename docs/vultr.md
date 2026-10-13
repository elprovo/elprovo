# Run previews on Vultr

With `-provider vultr`, the agent creates one Vultr instance for each preview and one for each build worker through the Vultr API, and drives them over SSH. [agent.md](agent.md) explains what runs on each. `elprovo setup vultr` ([setup.md](setup.md#vultr)) creates the VPC, the firewall group and the registry that the agent uses, and starts the agent with these flags.

## Start the agent

```sh
export VULTR_API_KEY=<key>
go run ./cmd/elprovo-agent run -provider vultr -vultr-region <region> \
  -vultr-firewall-group <firewall group id> -vultr-registry <registry id> \
  -vultr-vpc <VPC id> -vultr-vpc-range 10.42.0.0/20 \
  -vultr-vpc-allow <agent's address in the VPC>,<gateway's address in the VPC> \
  -registry <region>.vultrcr.com/<registry name> \
  -enrollment-token <agent token>
```

Before each build and each start, the agent asks the API for Docker credentials for the Vultr Container Registry `-vultr-registry` that expire after two hours: read and write for a build worker, read only for a preview instance. The machine's Docker signs in with them, so neither the API key nor the registry's root user reaches an instance.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-vultr-api-url` | `https://api.vultr.com` | API base URL |
| `-vultr-region` | none | Region of the instances, such as `fra` |
| `-vultr-plan` | `vc2-2c-4gb` | Instance plan |
| `-vultr-os` | `2284` | Stock operating system id; 2284 is Ubuntu 24.04 LTS x64. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new instance. |
| `-vultr-golden-image` | none | A Elprovo golden image (snapshot id) with Docker already installed. When set, instances start from it instead of `-vultr-os` and install nothing. |
| `-vultr-firewall-group` | none | Firewall group id that every instance joins. Setup passes the preview group, which has no rules, so instances accept nothing on their public interface; see [Network access](#network-access). Without it, instances accept traffic from anywhere there. |
| `-vultr-vpc` | required | Id of the VPC that every instance joins. The agent drives each instance, and the gateway reaches it, on its address there. |
| `-vultr-vpc-range` | required | Address range of `-vultr-vpc`, such as `10.42.0.0/20` |
| `-vultr-vpc-allow` | required | Comma-separated addresses in the VPC that may connect to instances: the agent host's and the gateway host's. See [Network access](#network-access). |
| `-vultr-tag` | `elprovo-preview` | Tag on every instance the agent creates. The agent finds its instances by this tag and their label. Setup passes `<environment>-preview`. |
| `-vultr-registry` | none | Id of the container registry that machines get short-lived credentials for. Without it, machines sign in only with `-registry-username`, as for a registry outside Vultr. |

The API key stays on the agent host. Pass it only through `VULTR_API_KEY`. A Vultr API key can act on the whole account, so restrict it in the Vultr account's API access control to the agent host's address, which setup makes a reserved IP, and to the addresses you run `elprovo setup` from.

## What the agent creates

- One SSH key, `elprovo-agent <fingerprint>`, from `<data-dir>/ssh.key`. The agent reuses a key in the account that has the same key data.
- For each preview, an instance labeled `elprovo-<preview id>`, with the same host name and the tag. Cloud-init installs Docker and the Compose plugin.
- Up to `-build-workers` instances labeled `elprovo-build-<n>` with the tag. They stay when previews come and go.
- A host key for each instance. The agent generates it, gives it to the instance through cloud-init, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to an instance that presents that key.
- A firewall rule on each instance that rejects new connections from containers to the metadata service at `169.254.169.254`. The metadata service serves the instance's user data, which holds the host key, and the instance runs customer code in containers. The rule sits in Docker's `DOCKER-USER` chain and is added on every boot, before Docker starts.
- Firewall rules on each instance that accept new connections from the VPC only from `-vultr-vpc-allow`; [Network access](#network-access) describes them.

The agent downloads the commit's source itself and copies it to a build worker, so no instance receives a GitHub token, and preview instances never receive the source. The agent acts only on instances that carry its tag and the machine's label. Delete removes the instance and waits until Vultr answers 404 for it: while Vultr destroys an instance, it answers `Server is pending destruction`.

## Network access

The agent host, the gateway host, the preview machines and the build workers share one VPC. The agent reaches each instance there over SSH, with the host key it pinned, and reports that address to the control plane, so the gateway forwards visitors to it there over HTTP; TLS ends at the gateway.

A Vultr firewall group filters only inbound traffic on the public interface; traffic inside a VPC passes it unfiltered. So each machine filters that traffic itself, with iptables rules that its user data adds on every boot, before Docker starts:

- A preview machine or build worker accepts a new connection from the VPC only from an address in `-vultr-vpc-allow`. The rules sit in the `INPUT` chain, for the instance's own ports such as SSH, and in Docker's `DOCKER-USER` chain, for the ports Docker publishes, which never reach `INPUT`.
- Its containers can't open a connection to the VPC, so they can't reach other machines there with a forged source address either.
- The agent host and the gateway host, which setup creates, accept no new connection from the VPC.

So code in one preview can't reach another preview, the agent host, which holds the API key, or the gateway host over the VPC. The rules trust that a packet's source address in the VPC is its sender's own.

On the public interface, the preview group that setup creates has no rules, so preview machines and build workers accept nothing there. They keep the public IPv4 address that comes with every Vultr instance at no extra price, to reach the registry, package mirrors and the internet. The agent host's group lets in only SSH from `-operator-cidr`, and the gateway's 80 and 443 from anywhere and SSH from `-operator-cidr`.
