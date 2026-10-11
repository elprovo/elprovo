package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
)

// Azure is an Azure subscription to bootstrap, through a service principal.
type Azure struct {
	TenantID       string
	ClientID       string
	ClientSecret   string
	SubscriptionID string
	// AgentClientID and AgentClientSecret, when set, are a service principal
	// of the same tenant that the agent uses instead of the one above, with a
	// role limited to the environment's resource group.
	AgentClientID     string
	AgentClientSecret string
	// ARMURL is the Resource Manager endpoint whose metadata document
	// describes the cloud: https://management.azure.com on Azure.
	ARMURL       string
	Location     string
	RegistryName string
	// GoldenImage is the id of a managed image from images/azure that the
	// agent boots stagelet machines and build workers from. Empty means the
	// stock image with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the subscription with module.
func (a Azure) Cloud(module fs.FS) (Cloud, error) {
	if a.TenantID == "" || a.ClientID == "" || a.ClientSecret == "" || a.SubscriptionID == "" || a.Location == "" || a.RegistryName == "" {
		return Cloud{}, errors.New("Azure setup needs a tenant id, client id, client secret, subscription id, location and registry name")
	}
	if (a.AgentClientID == "") != (a.AgentClientSecret == "") {
		return Cloud{}, errors.New("set both the agent's client id and client secret, or neither")
	}
	agentID, agentSecret := a.ClientID, a.ClientSecret
	if a.AgentClientID != "" {
		agentID, agentSecret = a.AgentClientID, a.AgentClientSecret
	}
	arm, err := url.Parse(a.ARMURL)
	if err != nil || arm.Scheme != "https" || arm.Host == "" || (arm.Path != "" && arm.Path != "/") {
		return Cloud{}, fmt.Errorf("the Resource Manager URL %q must be https://host[:port]", a.ARMURL)
	}
	return Cloud{
		Provider: "azure",
		Module:   module,
		TofuEnv: []string{
			"ARM_TENANT_ID=" + a.TenantID, "ARM_CLIENT_ID=" + a.ClientID, "ARM_CLIENT_SECRET=" + a.ClientSecret, "ARM_SUBSCRIPTION_ID=" + a.SubscriptionID,
		},
		Vars: map[string]any{"metadata_host": arm.Host, "location": a.Location, "registry_name": a.RegistryName},
		Agent: func(o map[string]any) ([]string, map[string]string, error) {
			names := []string{"resource_group", "location", "stagelet_subnet_id", "stagelet_tag", "registry", "registry_id"}
			values := map[string]string{}
			for _, name := range names {
				v, err := outputString(o, name)
				if err != nil {
					return nil, nil, err
				}
				values[name] = v
			}
			args := []string{
				"-azure-arm-url", "https://" + arm.Host, "-azure-subscription", a.SubscriptionID, "-azure-resource-group", values["resource_group"],
				"-azure-location", values["location"], "-azure-subnet", values["stagelet_subnet_id"], "-azure-tag", values["stagelet_tag"],
				"-azure-registry", values["registry_id"], "-registry", values["registry"],
			}
			if a.GoldenImage != "" {
				args = append(args, "-azure-golden-image", a.GoldenImage)
			}
			env := map[string]string{"AZURE_TENANT_ID": a.TenantID, "AZURE_CLIENT_ID": agentID, "AZURE_CLIENT_SECRET": agentSecret}
			return args, env, nil
		},
	}, nil
}
