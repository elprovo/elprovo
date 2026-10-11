# Run stagelets on AWS

With `-provider aws`, the agent launches one EC2 instance for each stagelet and one for each build worker through the EC2 API, and drives them over SSH. [agent.md](agent.md) explains what runs on each.

## Start the agent

```sh
export AWS_ACCESS_KEY_ID=<access key id>
export AWS_SECRET_ACCESS_KEY=<secret access key>
go run ./cmd/stagely-agent run -provider aws -aws-region <region> \
  -aws-image <Ubuntu 24.04 AMI id> -aws-subnet <subnet id> -aws-security-group <security group id> \
  -registry <account id>.dkr.ecr.<region>.amazonaws.com/<prefix> \
  -enrollment-token <agent token>
```

Leave `-registry-username` empty to use Amazon ECR. Before each build and each deploy, the agent signs the machine's Docker in with the user `AWS` and a fresh ECR authorization token, which it gets in two steps:

1. It calls STS `GetFederationToken` for temporary credentials of the federated user `stagely-registry-pull`, or `stagely-registry-push` for a build worker. Their session policy allows `ecr:GetAuthorizationToken` and, only on the repositories under the prefix (`arn:aws:ecr:<region>:<account id>:repository/<prefix>/*`), the pull actions `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer` and `ecr:BatchCheckLayerAvailability`. For a build worker, it also allows the push actions `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`, `ecr:CompleteLayerUpload` and `ecr:PutImage`, and `ecr:CreateRepository`, so the first push of a new image creates its repository under the prefix through create on push.
2. It calls ECR `GetAuthorizationToken` with those temporary credentials.

ECR gives a token the permissions of the credentials that asked for it: here, the permissions that both the agent's IAM user and the session policy allow. So a stagelet machine's token can only pull, and no token reaches repositories outside the prefix. The token expires after 12 hours, as does the session, and works only with the registry. Neither the agent's AWS credentials nor the temporary credentials reach a machine. The agent hands a token out only for the account's own registry host in the region, `<account id>.dkr.ecr.<region>.amazonaws.com`. ECR refuses a push to a repository that does not exist, unless a repository creation template for create on push matches the repository name; the bootstrap module of `stagely setup aws` creates one for the prefix. With `-registry-username`, the agent signs in to any other registry with `STAGELY_REGISTRY_PASSWORD`, as on other clouds.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-aws-region` | none | Region of the instances, such as `eu-central-1` |
| `-aws-ec2-url` | `https://ec2.<region>.amazonaws.com` | EC2 API endpoint. Point it at a local simulation for tests. |
| `-aws-ecr-url` | `https://api.ecr.<region>.amazonaws.com` | ECR API endpoint, for the registry sign-in |
| `-aws-sts-url` | `https://sts.<region>.amazonaws.com` | STS API endpoint, for the temporary credentials of the registry sign-in |
| `-aws-instance-type` | `t3.medium` | Instance type |
| `-aws-image` | none | Stock Ubuntu 24.04 AMI id of the region. It must run cloud-init, which installs the `docker.io`, `docker-compose-v2` and `docker-buildx` packages on each new instance. |
| `-aws-golden-image` | none | A Stagely golden image (AMI id) with Docker already installed. When set, instances start from it instead of `-aws-image` and install nothing. One of the two is required. |
| `-aws-subnet` | none | Subnet id. Instances launch into it, and the agent reaches them on their private address. Without it, they launch into the default VPC and the agent uses their public address. |
| `-aws-security-group` | none, required | Security group id of every instance. It must let in only the agent and the gateway, and must not let in its own members: a group that does, such as a VPC's default group, lets stagelets reach each other. |
| `-aws-volume-size` | `30` | Root volume size of every instance in GiB, for images, build cache and stagelet data. Ubuntu AMIs come with 8 GiB. |
| `-aws-group` | `stagely-stagelet` | Value of the `stagely-group` tag on every instance the agent launches. The agent finds its instances by it. |

The credentials come from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and, for temporary credentials, `AWS_SESSION_TOKEN`. They stay on the agent host. The agent signs each request with Signature Version 4. It needs `ec2:DescribeInstances`, `ec2:RunInstances`, `ec2:CreateTags` (for the tags at launch), `ec2:TerminateInstances`, `ec2:DescribeKeyPairs`, `ec2:ImportKeyPair`, `sts:GetFederationToken` and `ecr:GetAuthorizationToken`, and the ECR push and pull permissions, with `ecr:CreateRepository`, for the prefix. Only the long-term access key of an IAM user can call `GetFederationToken`: with temporary credentials, the registry sign-in fails.

## What the agent creates

- One EC2 key pair, `stagely-agent-<MD5 fingerprint>`, imported once from `<data-dir>/ssh.key`.
- For each stagelet, an instance with the tags `Name=stagely-<stagelet id>` and `stagely-group=<group>`. Cloud-init installs Docker and the Compose plugin.
- Up to `-build-workers` instances named `stagely-build-<n>` with the group tag. They stay when stagelets come and go.
- A host key for each instance. The agent generates it, gives it to the instance through cloud-init, and keeps its public half in `<data-dir>/hostkeys/`. The agent connects only to an instance that presents that key.
- `disable_root: false` in each instance's cloud-config. Ubuntu AMIs on AWS refuse SSH logins as root by default, and the agent signs in as root with its key pair.
- A firewall rule on each instance that rejects new connections from containers to the instance metadata service at `169.254.169.254`. The metadata service serves the instance's user data, which holds the host key, and the instance runs customer code in containers. The rule sits in Docker's `DOCKER-USER` chain and is added on every boot, before Docker starts. The instance also requires IMDSv2 with a hop limit of 1, so a container, one network hop further, gets no metadata token even without the rule.

The agent downloads the commit's source itself and copies it to a build worker, so no instance receives a GitHub token, and stagelet instances never receive the source. Delete terminates the instance and waits until EC2 reports it `terminated` or no longer knows it. A terminated instance stays listed for about an hour; the agent ignores it.

## Network access

The agent does not create security groups. Give the stagelet instances a security group that lets in only the agent and the gateway, for example with rules whose source is the agent's and the gateway's security groups, and allows outbound traffic for packages and images. Security groups also filter traffic inside the VPC, so with no rule that names the group itself, stagelets can't reach each other or the agent host. The instances need a public address or a NAT gateway for that outbound traffic.
