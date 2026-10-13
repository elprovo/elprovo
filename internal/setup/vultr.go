package setup

import (
	"errors"
	"io/fs"
	"net/netip"
)

// Vultr is a Vultr account to bootstrap. The module and the agent call
// https://api.vultr.com, the only API URL the Terraform provider knows. The
// hosts, preview machines and build workers share a VPC, which Vultr's
// firewall does not filter.
type Vultr struct {
	APIKey       string
	Region       string
	RegistryName string
	// GoldenImage is a snapshot from images/vultr that the agent boots
	// preview machines and build workers from. Empty means the stock
	// operating system with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the account with module.
func (v Vultr) Cloud(module fs.FS) (Cloud, error) {
	if v.APIKey == "" || v.Region == "" || v.RegistryName == "" {
		return Cloud{}, errors.New("Vultr setup needs an API key, a region and a registry name")
	}
	return Cloud{
		Provider:       "vultr",
		Module:         module,
		TofuEnv:        []string{"VULTR_API_KEY=" + v.APIKey},
		Vars:           map[string]any{"region": v.Region, "registry_name": v.RegistryName},
		PrivateNetwork: netip.MustParsePrefix(privateNetworkRange),
		Agent:          v.agent,
	}, nil
}

// agent returns the agent's flags and credentials from the module's
// outputs. The agent's only credential is the API key: it gets short-lived
// registry credentials for each machine with it.
func (v Vultr) agent(o map[string]any) ([]string, map[string]string, error) {
	values := map[string]string{}
	for _, name := range []string{"preview_firewall_group_id", "preview_tag", "registry", "registry_id", "vpc_id", "agent_private_ip", "gateway_private_ip"} {
		value, err := outputString(o, name)
		if err != nil {
			return nil, nil, err
		}
		values[name] = value
	}
	args := []string{
		"-vultr-region", v.Region, "-vultr-firewall-group", values["preview_firewall_group_id"],
		"-vultr-tag", values["preview_tag"], "-vultr-vpc", values["vpc_id"], "-vultr-vpc-range", privateNetworkRange,
		"-vultr-vpc-allow", values["agent_private_ip"] + "," + values["gateway_private_ip"],
		"-vultr-registry", values["registry_id"], "-registry", values["registry"],
	}
	if v.GoldenImage != "" {
		args = append(args, "-vultr-golden-image", v.GoldenImage)
	}
	return args, map[string]string{"VULTR_API_KEY": v.APIKey}, nil
}
