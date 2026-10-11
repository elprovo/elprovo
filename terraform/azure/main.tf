locals {
  stagelet_tag = var.stagelet_tag != "" ? var.stagelet_tag : "${var.name}-stagelet"
  # Azure gives the operator's key to the admin user. Setup installs over SSH
  # as root, so the hosts' cloud-config keeps root logins with that key.
  agent_custom_data   = base64encode("${var.agent_user_data != "" ? var.agent_user_data : "#cloud-config\n"}disable_root: false\n")
  gateway_custom_data = base64encode("${var.gateway_user_data != "" ? var.gateway_user_data : "#cloud-config\n"}disable_root: false\n")
}

resource "azurerm_resource_group" "environment" {
  name     = "${var.name}-stagely"
  location = var.location
}

resource "azurerm_virtual_network" "environment" {
  name                = "${var.name}-stagely"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name
  address_space       = [var.address_space]
}

# The hosts reach out through their own public IPs, so their subnet is
# private.
resource "azurerm_subnet" "hosts" {
  name                            = "hosts"
  resource_group_name             = azurerm_resource_group.environment.name
  virtual_network_name            = azurerm_virtual_network.environment.name
  address_prefixes                = [var.hosts_subnet]
  default_outbound_access_enabled = false
}

# Stagelet machines have no public IP, so they reach registries and package
# mirrors through Azure's default outbound access, which the subnet turns on
# explicitly: networks made with API versions released after March 2026
# get private subnets by default.
resource "azurerm_subnet" "stagelets" {
  name                            = "stagelets"
  resource_group_name             = azurerm_resource_group.environment.name
  virtual_network_name            = azurerm_virtual_network.environment.name
  address_prefixes                = [var.stagelets_subnet]
  default_outbound_access_enabled = true
}

resource "azurerm_public_ip" "agent" {
  name                = "${var.name}-agent"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name
  allocation_method   = "Static"
  sku                 = "Standard"
}

resource "azurerm_public_ip" "gateway" {
  name                = "${var.name}-gateway"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name
  allocation_method   = "Static"
  sku                 = "Standard"
}

# The agent accepts no inbound connections except SSH from the operator.
resource "azurerm_network_security_group" "agent" {
  name                = "${var.name}-agent"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name

  security_rule {
    name                       = "ssh-from-operators"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefixes    = var.operator_cidrs
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "deny-other-inbound"
    priority                   = 4096
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

# The gateway is the only host that accepts internet traffic.
resource "azurerm_network_security_group" "gateway" {
  name                = "${var.name}-gateway"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name

  security_rule {
    name                       = "web-from-internet"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_ranges    = ["80", "443"]
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "ssh-from-operators"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefixes    = var.operator_cidrs
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "deny-other-inbound"
    priority                   = 4096
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

# Stagelet machines and build workers accept traffic only from the agent,
# which drives them over SSH and checks health, and from the gateway.
resource "azurerm_network_security_group" "stagelets" {
  name                = "${var.name}-stagelets"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name

  security_rule {
    name                       = "from-agent-and-gateway"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefixes    = [azurerm_network_interface.agent.private_ip_address, azurerm_network_interface.gateway.private_ip_address]
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "deny-other-inbound"
    priority                   = 4096
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

resource "azurerm_subnet_network_security_group_association" "stagelets" {
  subnet_id                 = azurerm_subnet.stagelets.id
  network_security_group_id = azurerm_network_security_group.stagelets.id
}

resource "azurerm_network_interface" "agent" {
  name                = "${var.name}-agent"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name

  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.hosts.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.agent.id
  }
}

resource "azurerm_network_interface" "gateway" {
  name                = "${var.name}-gateway"
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name

  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.hosts.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.gateway.id
  }
}

resource "azurerm_network_interface_security_group_association" "agent" {
  network_interface_id      = azurerm_network_interface.agent.id
  network_security_group_id = azurerm_network_security_group.agent.id
}

resource "azurerm_network_interface_security_group_association" "gateway" {
  network_interface_id      = azurerm_network_interface.gateway.id
  network_security_group_id = azurerm_network_security_group.gateway.id
}

# Each host boots with its network security group in place.
resource "azurerm_linux_virtual_machine" "agent" {
  name                  = "${var.name}-agent"
  location              = azurerm_resource_group.environment.location
  resource_group_name   = azurerm_resource_group.environment.name
  size                  = var.agent_size
  admin_username        = var.admin_username
  network_interface_ids = [azurerm_network_interface.agent.id]
  custom_data           = local.agent_custom_data

  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
  }

  source_image_reference {
    publisher = var.image.publisher
    offer     = var.image.offer
    sku       = var.image.sku
    version   = var.image.version
  }

  depends_on = [azurerm_network_interface_security_group_association.agent]
}

resource "azurerm_linux_virtual_machine" "gateway" {
  name                  = "${var.name}-gateway"
  location              = azurerm_resource_group.environment.location
  resource_group_name   = azurerm_resource_group.environment.name
  size                  = var.gateway_size
  admin_username        = var.admin_username
  network_interface_ids = [azurerm_network_interface.gateway.id]
  custom_data           = local.gateway_custom_data

  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
  }

  source_image_reference {
    publisher = var.image.publisher
    offer     = var.image.offer
    sku       = var.image.sku
    version   = var.image.version
  }

  depends_on = [azurerm_network_interface_security_group_association.gateway]
}

# Machines sign in with short-lived passwords of repository-scoped tokens
# that the agent makes, so the registry has no admin user.
resource "azurerm_container_registry" "stagely" {
  name                = var.registry_name
  location            = azurerm_resource_group.environment.location
  resource_group_name = azurerm_resource_group.environment.name
  sku                 = var.registry_sku
  admin_enabled       = false
}
