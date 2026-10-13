locals {
  preview_tag = var.preview_tag != "" ? var.preview_tag : "${var.name}-preview"
  agent_tag   = "${var.name}-agent"
  gateway_tag = "${var.name}-gateway"
  ssh_keys    = "root:${var.ssh_public_key}"

  # The Compute Engine guest agent replaces the SSH host keys on an
  # instance's first boot unless this file says not to. Cloud-init writes it
  # before the guest agent starts, so the host key from the user data stays.
  guest_agent_config = <<-EOT
    write_files:
      - path: /etc/default/instance_configs.cfg
        content: |
          [InstanceSetup]
          set_host_keys = false
  EOT
}

resource "google_compute_network" "environment" {
  name                    = "${var.name}-elprovo"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "environment" {
  name          = "${var.name}-elprovo"
  region        = var.region
  network       = google_compute_network.environment.id
  ip_cidr_range = var.subnet_cidr
}

# The agent accepts no inbound connections except SSH from the operator.
resource "google_compute_firewall" "agent_ssh" {
  name          = "${var.name}-agent-ssh"
  network       = google_compute_network.environment.id
  direction     = "INGRESS"
  source_ranges = var.operator_cidrs
  target_tags   = [local.agent_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# The gateway is the only host that accepts internet traffic.
resource "google_compute_firewall" "gateway_web" {
  name          = "${var.name}-gateway-web"
  network       = google_compute_network.environment.id
  direction     = "INGRESS"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [local.gateway_tag]

  allow {
    protocol = "tcp"
    ports    = ["80", "443"]
  }
}

resource "google_compute_firewall" "gateway_ssh" {
  name          = "${var.name}-gateway-ssh"
  network       = google_compute_network.environment.id
  direction     = "INGRESS"
  source_ranges = var.operator_cidrs
  target_tags   = [local.gateway_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# Preview machines and build workers accept traffic only from the agent,
# which drives them over SSH and checks health, and from the gateway. The
# network's implied rules deny other ingress and allow all egress.
resource "google_compute_firewall" "previews" {
  name        = "${var.name}-previews"
  network     = google_compute_network.environment.id
  direction   = "INGRESS"
  source_tags = [local.agent_tag, local.gateway_tag]
  target_tags = [local.preview_tag]

  allow {
    protocol = "tcp"
  }
}

resource "google_compute_address" "gateway" {
  name   = "${var.name}-gateway"
  region = var.region
}

resource "google_compute_instance" "agent" {
  name         = "${var.name}-agent"
  zone         = var.zone
  machine_type = var.agent_machine_type
  tags         = [local.agent_tag]

  boot_disk {
    initialize_params {
      image = var.image
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.environment.id
    access_config {}
  }

  metadata = {
    user-data              = var.agent_user_data == "" ? "" : "${var.agent_user_data}${local.guest_agent_config}"
    ssh-keys               = local.ssh_keys
    block-project-ssh-keys = "TRUE"
  }
}

resource "google_compute_instance" "gateway" {
  name         = "${var.name}-gateway"
  zone         = var.zone
  machine_type = var.gateway_machine_type
  tags         = [local.gateway_tag]

  boot_disk {
    initialize_params {
      image = var.image
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.environment.id
    access_config {
      nat_ip = google_compute_address.gateway.address
    }
  }

  metadata = {
    user-data              = var.gateway_user_data == "" ? "" : "${var.gateway_user_data}${local.guest_agent_config}"
    ssh-keys               = local.ssh_keys
    block-project-ssh-keys = "TRUE"
  }
}

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = var.registry_name
  format        = "DOCKER"
  description   = "Images that Elprovo builds for ${var.name}"
}

# Machines sign in to the registry with short-lived tokens of these accounts,
# which may only push to or only pull from the repository. The agent's
# service account impersonates them, so its key never reaches a machine.
resource "google_service_account" "registry_push" {
  account_id   = "${substr(var.name, 0, 24)}-push"
  display_name = "Elprovo ${var.name} registry push"
}

resource "google_service_account" "registry_pull" {
  account_id   = "${substr(var.name, 0, 24)}-pull"
  display_name = "Elprovo ${var.name} registry pull"
}

resource "google_artifact_registry_repository_iam_member" "push" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.repository_id
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.registry_push.member
}

resource "google_artifact_registry_repository_iam_member" "pull" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.repository_id
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.registry_pull.member
}

resource "google_service_account_iam_member" "agent_impersonates_push" {
  service_account_id = google_service_account.registry_push.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${var.agent_service_account}"
}

resource "google_service_account_iam_member" "agent_impersonates_pull" {
  service_account_id = google_service_account.registry_pull.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${var.agent_service_account}"
}
