resource "docker_image" "registry" {
  name         = "registry:2"
  keep_locally = true
}

resource "docker_volume" "registry" {
  name = "${var.name}-stagely-registry"
}

# Build workers push here and stagelet VMs pull from here, over plain HTTP
# at host.lima.internal. The agent tells Docker in each VM to trust it.
resource "docker_container" "registry" {
  name    = "${var.name}-stagely-registry"
  image   = docker_image.registry.image_id
  restart = "unless-stopped"

  ports {
    internal = 5000
    external = var.registry_port
  }

  volumes {
    volume_name    = docker_volume.registry.name
    container_path = "/var/lib/registry"
  }
}

# The gateway on the Mac serves stagelet host names with a certificate from
# mkcert's local certificate authority, which `mkcert -install` makes the
# Mac's browsers trust.
resource "terraform_data" "gateway_certificate" {
  triggers_replace = [var.stagelet_domain, var.certificate_dir]

  provisioner "local-exec" {
    command = "mkdir -p \"$DIR\" && mkcert -cert-file \"$DIR/tls.crt\" -key-file \"$DIR/tls.key\" \"*.$DOMAIN\""
    environment = {
      DIR    = var.certificate_dir
      DOMAIN = var.stagelet_domain
    }
  }
}
