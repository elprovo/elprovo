package setup

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var azureOutputs = map[string]any{
	"resource_group": "acme-stagely", "location": "westeurope", "stagelet_subnet_id": "/subscriptions/sub/resourceGroups/acme-stagely/providers/Microsoft.Network/virtualNetworks/acme/subnets/stagelets",
	"stagelet_tag": "acme-stagelet", "registry": "acmestagely.azurecr.io",
	"registry_id": "/subscriptions/sub/resourceGroups/acme-stagely/providers/Microsoft.ContainerRegistry/registries/acmestagely",
}

func azureSetup() Azure {
	return Azure{
		TenantID: "tenant", ClientID: "client", ClientSecret: "s3cret", SubscriptionID: "sub", ARMURL: "https://sim.test:9305",
		Location: "westeurope", RegistryName: "acmestagely",
	}
}

func TestAzureCloudGivesTheAgentItsFlagsAndCredentials(t *testing.T) {
	cloud, err := azureSetup().Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	if cloud.Vars["metadata_host"] != "sim.test:9305" || !slices.Contains(cloud.TofuEnv, "ARM_CLIENT_SECRET=s3cret") || !slices.Contains(cloud.TofuEnv, "ARM_SUBSCRIPTION_ID=sub") {
		t.Fatalf("cloud = %+v", cloud)
	}

	args, env, err := cloud.Agent(azureOutputs)

	want := []string{
		"-azure-arm-url", "https://sim.test:9305", "-azure-subscription", "sub", "-azure-resource-group", "acme-stagely", "-azure-location", "westeurope",
		"-azure-subnet", azureOutputs["stagelet_subnet_id"].(string), "-azure-tag", "acme-stagelet", "-azure-registry", azureOutputs["registry_id"].(string),
		"-registry", "acmestagely.azurecr.io",
	}
	wantEnv := map[string]string{"AZURE_TENANT_ID": "tenant", "AZURE_CLIENT_ID": "client", "AZURE_CLIENT_SECRET": "s3cret"}
	if err != nil || !reflect.DeepEqual(args, want) || !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("agent = %v %v %v; want the service principal only in the agent's environment, so machines get short-lived registry tokens instead", args, env, err)
	}
	if strings.Contains(strings.Join(args, " "), "s3cret") {
		t.Fatal("the client secret is in the agent's arguments, which other users can read")
	}
	if _, _, err := cloud.Agent(map[string]any{"registry": "acmestagely.azurecr.io"}); err == nil {
		t.Fatal("missing outputs were accepted")
	}
}

func TestAzureCloudBootsMachinesFromAGoldenImage(t *testing.T) {
	a := azureSetup()
	a.GoldenImage = "/subscriptions/sub/resourceGroups/images/providers/Microsoft.Compute/images/stagely"
	cloud, err := a.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}

	args, _, err := cloud.Agent(azureOutputs)

	if err != nil || !slices.Equal(args[len(args)-2:], []string{"-azure-golden-image", a.GoldenImage}) {
		t.Fatalf("agent args = %v, %v; want -azure-golden-image", args, err)
	}
}

func TestAzureCloudNeedsAnHTTPSResourceManager(t *testing.T) {
	a := azureSetup()
	a.ARMURL = "http://sim.test:9305"

	if _, err := a.Cloud(fstest.MapFS{}); err == nil {
		t.Fatal("a plain HTTP Resource Manager URL was accepted; the Terraform provider reads the metadata document only over HTTPS")
	}
}

func TestAzureCloudGivesTheAgentItsOwnServicePrincipal(t *testing.T) {
	a := azureSetup()
	a.AgentClientID, a.AgentClientSecret = "agent-client", "agent-s3cret"
	cloud, err := a.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}

	_, env, err := cloud.Agent(azureOutputs)

	if err != nil || env["AZURE_CLIENT_ID"] != "agent-client" || env["AZURE_CLIENT_SECRET"] != "agent-s3cret" || !slices.Contains(cloud.TofuEnv, "ARM_CLIENT_SECRET=s3cret") {
		t.Fatalf("agent env = %v, tofu env = %v; want the agent's own principal for the agent and setup's for OpenTofu", env, cloud.TofuEnv)
	}
	a.AgentClientSecret = ""
	if _, err := a.Cloud(fstest.MapFS{}); err == nil {
		t.Fatal("an agent client id without its secret was accepted")
	}
}
