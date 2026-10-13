# Golden images

Each folder holds a Packer template for one cloud. A golden image is the cloud's stock Ubuntu image with Docker, the Compose plugin and BuildKit installed, so preview machines and build workers start without installing packages. The agent uses a golden image when one is configured and falls back to the stock image with cloud-init otherwise.

Every template cleans cloud-init's state at the end, so each machine made from the image runs cloud-init again and gets the SSH host key that the agent pins.

## DigitalOcean

```sh
export DIGITALOCEAN_TOKEN=<token>
cd images/digitalocean
packer init .
packer build -var region=fra1 .
```

Packer prints the snapshot id. Start the agent with `-do-golden-image <snapshot id>`. Set `-var api_url=...` to build against a simulation of the DigitalOcean API.

## AWS

```sh
export AWS_ACCESS_KEY_ID=<access key id> AWS_SECRET_ACCESS_KEY=<secret access key>
cd images/aws
packer init .
packer build -var region=eu-central-1 .
```

Packer prints the AMI id. Start the agent with `-aws-golden-image <AMI id>`, or pass `-golden-image <AMI id>` to `elprovo setup aws`. Without `-var subnet_id=...`, the build instance runs in a default subnet of the default VPC. Set `AWS_ENDPOINT_URL` to build against a simulation of the AWS APIs: the plugin's `custom_endpoint_ec2` does not reach the client that tags the AMI.

## Hetzner Cloud

```sh
export HCLOUD_TOKEN=<token>
cd images/hetzner
packer init .
packer build -var location=fsn1 .
```

Packer prints the snapshot id. Start the agent with `-hcloud-golden-image <snapshot id>`, or pass `-golden-image <snapshot id>` to `elprovo setup hetzner`. A snapshot can boot servers in every location, but only on server types of the architecture it was built on: the default `cx23` builds an x86 image. Set `-var endpoint=...` to build against a simulation of the Hetzner Cloud API.

## Vultr

```sh
export VULTR_API_KEY=<key>
cd images/vultr
packer init .
packer build -var region=fra .
```

Packer prints the snapshot id. Start the agent with `-vultr-golden-image <snapshot id>`, or pass `-golden-image <snapshot id>` to `elprovo setup vultr`. The Vultr plugin always calls `https://api.vultr.com`; `sim/scripts/check-vultr.sh image` builds against the simulation by resolving that name to it.

## Azure

```sh
export ARM_TENANT_ID=<tenant id> ARM_CLIENT_ID=<client id> ARM_CLIENT_SECRET=<client secret> ARM_SUBSCRIPTION_ID=<subscription id>
cd images/azure
packer init .
packer build -var location=westeurope -var image_resource_group=<existing group> .
```

Packer builds in a temporary resource group with the `azure-arm` builder and captures a managed image into `image_resource_group`. It prints the image id. Start the agent with `-azure-golden-image <image id>`, or pass `-golden-image` to `elprovo setup azure`. Set `-var metadata_host=<URL>` to build against another cloud, such as a simulation of Azure Resource Manager.

Besides cleaning cloud-init's state, the build removes its own user `packer`, that user's sudo rule and root's authorized keys, so a machine made from the image gets only the admin user and keys of its own create. It does not run `waagent -deprovision`, which Microsoft documents for generalizing a Linux VM, because cloud-init provisions Ubuntu on Azure and the build already resets what cloud-init and the build added.
