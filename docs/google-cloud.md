# Run previews on Google Cloud

With `-provider gcp`, the agent creates one Compute Engine instance for each preview and one for each build worker, and drives them over SSH. [agent.md](agent.md) explains what runs on each.

## Start the agent

```sh
export GOOGLE_CREDENTIALS="$(cat key.json)"
go run ./cmd/elprovo-agent run -provider gcp -gcp-zone us-central1-a \
  -gcp-subnetwork projects/<project>/regions/us-central1/subnetworks/<subnetwork> \
  -registry us-central1-docker.pkg.dev/<project>/<repository> \
  -gcp-registry-push-account <push account email> -gcp-registry-pull-account <pull account email> \
  -enrollment-token <agent token>
```

`key.json` is a key of a service account that may manage Compute Engine instances. The agent signs in with it: it signs a JWT with the key and exchanges it for an access token at the key's `token_uri`. `GOOGLE_CREDENTIALS` holds the key file as JSON or as base64-encoded JSON.

The key never reaches a machine. Before each build and each start, the agent impersonates a registry service account with the IAM Service Account Credentials API and signs the machine's Docker in to Artifact Registry as `oauth2accesstoken` with that account's access token, which expires after an hour: the push account, with `roles/artifactregistry.writer` on the repository, for a build worker, and the pull account, with `roles/artifactregistry.reader`, for a preview machine. The agent's service account needs `roles/iam.serviceAccountTokenCreator` on both accounts. `elprovo setup gcp` creates the accounts and the roles.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-gcp-compute-url` | `https://compute.googleapis.com/compute/v1/` | Compute Engine API base URL. Point it at a local simulation for tests. |
| `-gcp-project` | the key's project | Project of the instances |
| `-gcp-zone` | none | Zone of the instances, such as `us-central1-a` |
| `-gcp-machine-type` | `e2-medium` | Machine type |
| `-gcp-image` | `projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64` | Stock image. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new instance. |
| `-gcp-golden-image` | none | A Elprovo golden image with Docker already installed: an image name in the project, or its path. When set, instances start from it instead of `-gcp-image` and install nothing. |
| `-gcp-subnetwork` | the `default` network | Subnetwork the instances join. The agent reaches them on their internal address, so it must run in the same network. |
| `-gcp-tag` | `elprovo-preview` | Network tag on every instance the agent creates, and the value of its `elprovo` label. The agent finds its instances by the label. Setup passes `<environment>-preview`, which the bootstrap module's firewall rule targets. |
| `-gcp-iam-credentials-url` | `https://iamcredentials.googleapis.com/v1/` | IAM Service Account Credentials API base URL |
| `-gcp-registry-push-account` | none | Service account whose tokens build workers push with |
| `-gcp-registry-pull-account` | none | Service account whose tokens preview machines pull with |

The key stays on the agent host. Pass it only through `GOOGLE_CREDENTIALS`.

## What the agent creates

- For each preview, an instance named `<machine prefix><preview id>` with the tag, the `elprovo` label, an external address for its outbound traffic, and a boot disk that is deleted with it. Cloud-init installs Docker and the Compose plugin.
- Up to `-build-workers` instances named `<machine prefix>build-<n>`. They stay when previews come and go. The prefix is `-machine-prefix`, `elprovo-` by default. Instance names are unique in a zone, so give each agent in a project its own prefix; setup passes `elprovo-<environment>-`.
- A host key for each instance. The agent generates it, gives it to the instance through cloud-init user data, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to an instance that presents that key. The user data also tells the Compute Engine guest agent not to replace the host keys on the first boot.
- The agent's own SSH key from `<data-dir>/ssh.key`, for root, in the instance's `ssh-keys` metadata, with `block-project-ssh-keys` so project-wide keys do not reach preview machines.
- A firewall rule on each instance that rejects new connections from containers to the metadata server at `169.254.169.254`, which `metadata.google.internal` names. The metadata server serves the instance's user data, which holds the host key, and the instance runs customer code in containers. DNS to that address stays open, because Compute Engine's DNS server has it too. The rule sits in Docker's `DOCKER-USER` chain and is added on every boot, before Docker starts.

The agent lists, drives and deletes only instances with its label, so other instances in the project are safe from it. Delete removes the instance and waits until Compute Engine answers 404 for it.

## Network access

The agent does not create firewall rules. Use a custom mode network, and add a rule on the tag that lets in only the agent and the gateway, by their tags or addresses; the network's implied rules deny other inbound traffic, so preview machines can't reach each other or the agent host, and allow outbound traffic for packages and images. Keep rules that let in a whole range, such as `default-allow-internal` of the `default` network, off the network, because preview machines would match them. `elprovo setup gcp` creates such a network and rule.
