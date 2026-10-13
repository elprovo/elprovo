package azure

import (
	"errors"
	"flag"
	"strings"
)

const (
	defaultTenant       = "0000a2e0-0000-4000-8000-000000000001"
	defaultSubscription = "0000a2e0-0000-4000-8000-000000000002"
	defaultClientID     = "0000a2e0-0000-4000-8000-000000000003"
	defaultClientSecret = "elprovo-sim-secret"
	defaultLocations    = "eastus,eastus2,westus2,westus3,centralus,northeurope,westeurope,uksouth,germanywestcentral,swedencentral,francecentral,southeastasia,australiaeast,canadacentral,japaneast"
)

type config struct {
	enabled      bool
	addr         string
	tlsCert      string
	tlsKey       string
	tenant       string
	subscription string
	clientID     string
	clientSecret string
	locations    []string
	network      string
	registry     registryConfig
}

type registryConfig struct {
	domain   string
	username string
	password string
}

func registerFlags(fs *flag.FlagSet, cfg *config, locations *string) {
	fs.BoolVar(&cfg.enabled, "azure", false, "run the Azure simulation")
	fs.StringVar(&cfg.addr, "azure-addr", "127.0.0.1:9305", "listen address of the simulated Azure endpoints: Resource Manager, its metadata document and the Entra ID token endpoint; virtual machine custom data runs as root in privileged containers, so expose it with care")
	fs.StringVar(&cfg.tlsCert, "azure-tls-cert", "", "PEM certificate to serve the endpoints over HTTPS with, which the Terraform provider requires; plain HTTP without it")
	fs.StringVar(&cfg.tlsKey, "azure-tls-key", "", "PEM private key of -azure-tls-cert")
	fs.StringVar(&cfg.tenant, "azure-tenant", defaultTenant, "Microsoft Entra tenant id that the token endpoint serves")
	fs.StringVar(&cfg.subscription, "azure-subscription", defaultSubscription, "id of the one subscription that Resource Manager serves")
	fs.StringVar(&cfg.clientID, "azure-client-id", defaultClientID, "client id of the service principal that may get tokens")
	fs.StringVar(&cfg.clientSecret, "azure-client-secret", defaultClientSecret, "client secret of the service principal")
	fs.StringVar(locations, "azure-locations", defaultLocations, "comma-separated location names that resources may use")
	fs.StringVar(&cfg.network, "azure-network", "elprovo-sim-azure", "Docker network for virtual machines' private addresses; public addresses use <network>-public")
	fs.StringVar(&cfg.registry.domain, "azure-registry-domain", "azurecr.io", "domain of container registry login servers, <registry name>.<domain>")
	fs.StringVar(&cfg.registry.username, "azure-registry-username", "elprovo", "user name that every container registry token's generated credentials carry")
	fs.StringVar(&cfg.registry.password, "azure-registry-password", "elprovo", "password of every container registry's admin user, whose name is the registry name, and of every generated token credential")
}

func (c *config) finish(locations string) error {
	for _, l := range strings.Split(locations, ",") {
		if l = strings.TrimSpace(strings.ToLower(l)); l != "" {
			c.locations = append(c.locations, l)
		}
	}
	switch {
	case len(c.locations) == 0:
		return errors.New("azure-locations must name at least one location")
	case c.network == "":
		return errors.New("azure-network must not be empty")
	case (c.tlsCert == "") != (c.tlsKey == ""):
		return errors.New("set both -azure-tls-cert and -azure-tls-key, or neither")
	case c.tenant == "" || c.subscription == "" || c.clientID == "" || c.clientSecret == "":
		return errors.New("azure-tenant, -azure-subscription, -azure-client-id and -azure-client-secret must not be empty")
	}
	return nil
}
