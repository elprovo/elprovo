package setup

import (
	"errors"
	"io/fs"
	"net/netip"
)

// Hetzner is a Hetzner Cloud project to bootstrap. Hetzner has no managed
// container registry, so the agent pushes to and pulls from a registry the
// operator already has. The hosts, preview machines and build workers share
// a private network, which Hetzner's firewalls do not filter.
type Hetzner struct {
	Token string
	// Endpoint is the API base URL with its /v1 path.
	Endpoint string
	Location string
	// Registry is the image name prefix in that registry, such as
	// registry.example.com/acme.
	Registry         string
	RegistryUsername string
	RegistryPassword string
	// GoldenImage is the id of a snapshot from images/hetzner that the agent
	// boots preview machines and build workers from. Empty means the stock
	// image with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the project with module.
func (h Hetzner) Cloud(module fs.FS) (Cloud, error) {
	if h.Token == "" || h.Endpoint == "" || h.Location == "" || h.Registry == "" {
		return Cloud{}, errors.New("setup on Hetzner needs a token, an API endpoint, a location and a registry")
	}
	return Cloud{
		Provider:       "hetzner",
		Module:         module,
		TofuEnv:        []string{"HCLOUD_TOKEN=" + h.Token},
		Vars:           map[string]any{"endpoint": h.Endpoint, "location": h.Location},
		PrivateNetwork: netip.MustParsePrefix(privateNetworkRange),
		Agent: func(o map[string]any) ([]string, map[string]string, error) {
			values := map[string]string{}
			for _, name := range []string{"preview_label", "machine_prefix", "network_id", "agent_private_ip", "gateway_private_ip"} {
				value, err := outputString(o, name)
				if err != nil {
					return nil, nil, err
				}
				values[name] = value
			}
			args := []string{
				"-hcloud-api-url", h.Endpoint, "-hcloud-location", h.Location, "-hcloud-label", values["preview_label"],
				"-hcloud-network", values["network_id"], "-hcloud-network-range", privateNetworkRange,
				"-hcloud-network-allow", values["agent_private_ip"] + "," + values["gateway_private_ip"],
				"-machine-prefix", values["machine_prefix"], "-registry", h.Registry,
			}
			if h.GoldenImage != "" {
				args = append(args, "-hcloud-golden-image", h.GoldenImage)
			}
			env := map[string]string{"HCLOUD_TOKEN": h.Token, "ELPROVO_REGISTRY_USERNAME": h.RegistryUsername, "ELPROVO_REGISTRY_PASSWORD": h.RegistryPassword}
			return args, env, nil
		},
	}, nil
}
