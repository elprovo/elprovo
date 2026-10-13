locals {
  preview_group   = var.preview_group != "" ? var.preview_group : "${var.name}-preview"
  registry_prefix = var.registry_prefix != "" ? var.registry_prefix : var.name
  image           = var.image != "" ? var.image : data.aws_ami.ubuntu[0].id
  # Ubuntu AMIs let only the ubuntu user sign in over SSH; `elprovo setup`
  # and the agent sign in as root, which disable_root false allows.
  root_login = "disable_root: false\n"
}

data "aws_caller_identity" "current" {}

data "aws_ami" "ubuntu" {
  count       = var.image == "" ? 1 : 0
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
}

resource "aws_vpc" "environment" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true

  tags = {
    Name = "${var.name}-elprovo"
  }
}

resource "aws_internet_gateway" "environment" {
  vpc_id = aws_vpc.environment.id

  tags = {
    Name = "${var.name}-elprovo"
  }
}

# Every machine gets a public address, so previews reach registries and
# package mirrors without a NAT gateway. Security groups keep them closed.
resource "aws_subnet" "environment" {
  vpc_id                  = aws_vpc.environment.id
  cidr_block              = var.subnet_cidr
  availability_zone       = var.availability_zone != "" ? var.availability_zone : null
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.name}-elprovo"
  }
}

resource "aws_route_table" "environment" {
  vpc_id = aws_vpc.environment.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.environment.id
  }

  tags = {
    Name = "${var.name}-elprovo"
  }
}

resource "aws_route_table_association" "environment" {
  subnet_id      = aws_subnet.environment.id
  route_table_id = aws_route_table.environment.id
}

resource "aws_key_pair" "operator" {
  key_name   = "${var.name}-elprovo-setup"
  public_key = var.ssh_public_key
}

# The agent accepts no inbound connections except SSH from the operator.
resource "aws_security_group" "agent" {
  name        = "${var.name}-agent"
  description = "Elprovo agent: SSH from the operator only"
  vpc_id      = aws_vpc.environment.id

  ingress {
    protocol    = "tcp"
    from_port   = 22
    to_port     = 22
    cidr_blocks = var.operator_cidrs
  }

  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# The gateway is the only host that accepts internet traffic.
resource "aws_security_group" "gateway" {
  name        = "${var.name}-gateway"
  description = "Elprovo gateway: HTTP and HTTPS from anywhere, SSH from the operator"
  vpc_id      = aws_vpc.environment.id

  ingress {
    protocol    = "tcp"
    from_port   = 80
    to_port     = 80
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    protocol    = "tcp"
    from_port   = 443
    to_port     = 443
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    protocol    = "tcp"
    from_port   = 22
    to_port     = 22
    cidr_blocks = var.operator_cidrs
  }

  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Preview machines and build workers accept traffic only from the agent,
# which drives them over SSH and checks health, and from the gateway.
resource "aws_security_group" "previews" {
  name        = "${var.name}-previews"
  description = "Elprovo preview machines and build workers: traffic from the agent and the gateway only"
  vpc_id      = aws_vpc.environment.id

  ingress {
    protocol        = "tcp"
    from_port       = 0
    to_port         = 65535
    security_groups = [aws_security_group.agent.id, aws_security_group.gateway.id]
  }

  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# A newer Ubuntu AMI must not replace running hosts, so the hosts ignore AMI
# changes. A changed host key in the user data replaces the host, because
# cloud-init installs host keys only on an instance's first boot.
resource "aws_instance" "agent" {
  ami                         = local.image
  instance_type               = var.agent_instance_type
  subnet_id                   = aws_subnet.environment.id
  vpc_security_group_ids      = [aws_security_group.agent.id]
  key_name                    = aws_key_pair.operator.key_name
  user_data                   = var.agent_user_data != "" ? "${var.agent_user_data}${local.root_login}" : "#cloud-config\n${local.root_login}"
  user_data_replace_on_change = true

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  tags = {
    Name = "${var.name}-agent"
  }

  lifecycle {
    ignore_changes = [ami]
  }
}

resource "aws_instance" "gateway" {
  ami                         = local.image
  instance_type               = var.gateway_instance_type
  subnet_id                   = aws_subnet.environment.id
  vpc_security_group_ids      = [aws_security_group.gateway.id]
  key_name                    = aws_key_pair.operator.key_name
  user_data                   = var.gateway_user_data != "" ? "${var.gateway_user_data}${local.root_login}" : "#cloud-config\n${local.root_login}"
  user_data_replace_on_change = true

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  tags = {
    Name = "${var.name}-gateway"
  }

  lifecycle {
    ignore_changes = [ami]
  }
}

# An Elastic IP needs the internet gateway attached to its VPC.
resource "aws_eip" "gateway" {
  domain     = "vpc"
  depends_on = [aws_internet_gateway.environment]

  tags = {
    Name = "${var.name}-gateway"
  }
}

resource "aws_eip_association" "gateway" {
  allocation_id = aws_eip.gateway.id
  instance_id   = aws_instance.gateway.id
}

# ECR creates a repository on the first push only when a template with the
# CREATE_ON_PUSH feature matches its name.
resource "aws_ecr_repository_creation_template" "previews" {
  prefix      = local.registry_prefix
  description = "Images that Elprovo build workers push for ${var.name}"
  applied_for = ["CREATE_ON_PUSH"]
}
