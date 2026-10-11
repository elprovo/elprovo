package setup

import (
	"errors"
	"io/fs"
	"strings"
)

// DigitalOcean is a DigitalOcean account to bootstrap.
type DigitalOcean struct {
	Token          string
	APIEndpoint    string
	Region         string
	RegistryName   string
	CreateRegistry bool
	// DNSDomain is a domain in DigitalOcean DNS that holds the stagelet
	// domain; the module then adds the wildcard record.
	DNSDomain string
	// GoldenImage is a snapshot from images/digitalocean that the agent
	// boots stagelet machines and build workers from. Empty means the stock
	// image with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the account with module, for
// an environment whose stagelets live under stageletDomain.
func (d DigitalOcean) Cloud(module fs.FS, stageletDomain string) (Cloud, error) {
	if d.Token == "" || d.Region == "" || d.RegistryName == "" {
		return Cloud{}, errors.New("DigitalOcean setup needs a token, a region and a registry name")
	}
	vars := map[string]any{"api_endpoint": d.APIEndpoint, "region": d.Region, "registry_name": d.RegistryName, "create_registry": d.CreateRegistry}
	if sub, ok := strings.CutSuffix(stageletDomain, "."+d.DNSDomain); d.DNSDomain != "" && ok {
		vars["dns_domain"], vars["stagelet_subdomain"] = d.DNSDomain, sub
	}
	return Cloud{
		Provider: "digitalocean",
		Module:   module,
		TofuEnv:  []string{"DIGITALOCEAN_TOKEN=" + d.Token},
		Vars:     vars,
		Agent: func(o map[string]any) ([]string, map[string]string, error) {
			vpc, err := outputString(o, "vpc_id")
			if err != nil {
				return nil, nil, err
			}
			tag, err := outputString(o, "stagelet_tag")
			if err != nil {
				return nil, nil, err
			}
			registry, err := outputString(o, "registry")
			if err != nil {
				return nil, nil, err
			}
			args := []string{"-do-api-url", d.APIEndpoint, "-do-region", d.Region, "-do-vpc", vpc, "-do-tag", tag, "-registry", registry}
			if d.GoldenImage != "" {
				args = append(args, "-do-golden-image", d.GoldenImage)
			}
			env := map[string]string{"DIGITALOCEAN_TOKEN": d.Token}
			return args, env, nil
		},
	}, nil
}
