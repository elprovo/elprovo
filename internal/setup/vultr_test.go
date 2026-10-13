package setup

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func vultrOutputs() map[string]any {
	return map[string]any{
		"preview_firewall_group_id": "fw-1", "preview_tag": "prod-preview",
		"registry": "fra.vultrcr.com/acme", "registry_id": "reg-1",
		"vpc_id": "vpc-1", "agent_private_ip": "10.42.0.2", "gateway_private_ip": "10.42.0.3",
	}
}

func TestVultrCloudGivesTheAgentItsFlagsAndCredentials(t *testing.T) {
	v := Vultr{APIKey: "VULTRKEY", Region: "fra", RegistryName: "acme"}

	cloud, err := v.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	if cloud.Provider != "vultr" || cloud.TofuEnv[0] != "VULTR_API_KEY=VULTRKEY" || cloud.Vars["region"] != "fra" || cloud.Vars["registry_name"] != "acme" ||
		cloud.PrivateNetwork.String() != "10.42.0.0/20" {
		t.Fatalf("cloud = %+v", cloud)
	}
	args, env, err := cloud.Agent(vultrOutputs())
	want := []string{
		"-vultr-region", "fra", "-vultr-firewall-group", "fw-1", "-vultr-tag", "prod-preview",
		"-vultr-vpc", "vpc-1", "-vultr-vpc-range", "10.42.0.0/20", "-vultr-vpc-allow", "10.42.0.2,10.42.0.3",
		"-vultr-registry", "reg-1", "-registry", "fra.vultrcr.com/acme",
	}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("agent args = %v, %v; want %v", args, err, want)
	}
	if len(env) != 1 || env["VULTR_API_KEY"] != "VULTRKEY" {
		t.Fatalf("agent env = %v, want only the API key, so no static registry credentials reach machines", env)
	}
	if strings.Contains(strings.Join(args, " "), "VULTRKEY") {
		t.Fatal("the API key is in the agent's arguments, which other users can read")
	}
}

func TestVultrCloudNeedsEveryModuleOutput(t *testing.T) {
	cloud, err := Vultr{APIKey: "VULTRKEY", Region: "fra", RegistryName: "acme"}.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	for name := range vultrOutputs() {
		outputs := vultrOutputs()
		delete(outputs, name)
		if _, _, err := cloud.Agent(outputs); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestVultrCloudPassesTheGoldenImage(t *testing.T) {
	cloud, err := Vultr{APIKey: "VULTRKEY", Region: "fra", RegistryName: "acme", GoldenImage: "snap-1"}.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}

	args, _, err := cloud.Agent(vultrOutputs())

	if err != nil || !slices.Equal(args[len(args)-2:], []string{"-vultr-golden-image", "snap-1"}) {
		t.Fatalf("agent args = %v, %v; want -vultr-golden-image snap-1", args, err)
	}
}

func TestVultrCloudNeedsAKeyARegionAndARegistry(t *testing.T) {
	for _, v := range []Vultr{{Region: "fra", RegistryName: "acme"}, {APIKey: "K", RegistryName: "acme"}, {APIKey: "K", Region: "fra"}} {
		if _, err := v.Cloud(fstest.MapFS{}); err == nil {
			t.Errorf("Cloud(%+v) succeeded", v)
		}
	}
}
