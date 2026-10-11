package setup

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHetznerCloudGivesTheAgentItsFlagsAndCredentials(t *testing.T) {
	h := Hetzner{
		Token: "hcloud-secret", Endpoint: "http://sim:9400/v1", Location: "fsn1",
		Registry: "registry.example.com/acme", RegistryUsername: "robot", RegistryPassword: "registry-secret",
	}

	cloud, err := h.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	if cloud.Provider != "hetzner" || cloud.Vars["endpoint"] != "http://sim:9400/v1" || cloud.Vars["location"] != "fsn1" ||
		!slices.Equal(cloud.TofuEnv, []string{"HCLOUD_TOKEN=hcloud-secret"}) {
		t.Fatalf("cloud = %+v; want the module to get the endpoint and location, and the provider the token", cloud)
	}
	if cloud.PrivateNetwork.String() != "10.42.0.0/20" {
		t.Fatalf("private network = %s, want the range whose new connections the hosts reject", cloud.PrivateNetwork)
	}
	args, env, err := cloud.Agent(hetznerOutputs())
	want := []string{
		"-hcloud-api-url", "http://sim:9400/v1", "-hcloud-location", "fsn1", "-hcloud-label", "stagely=prod-stagelet",
		"-hcloud-network", "4242", "-hcloud-network-range", "10.42.0.0/20", "-hcloud-network-allow", "10.42.0.2,10.42.0.3",
		"-machine-prefix", "stagely-prod-", "-registry", "registry.example.com/acme",
	}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("agent args = %v, %v; want %v", args, err, want)
	}
	if env["HCLOUD_TOKEN"] != "hcloud-secret" || env["STAGELY_REGISTRY_USERNAME"] != "robot" || env["STAGELY_REGISTRY_PASSWORD"] != "registry-secret" {
		t.Fatalf("agent env = %v; want the token and the registry credentials", env)
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "hcloud-secret") || strings.Contains(joined, "registry-secret") {
		t.Fatal("a secret is in the agent's arguments, which other users can read")
	}
}

func hetznerOutputs() map[string]any {
	return map[string]any{
		"stagelet_label": "stagely=prod-stagelet", "machine_prefix": "stagely-prod-",
		"network_id": "4242", "agent_private_ip": "10.42.0.2", "gateway_private_ip": "10.42.0.3",
	}
}

func TestHetznerCloudNeedsEveryModuleOutput(t *testing.T) {
	cloud, err := Hetzner{Token: "t", Endpoint: "https://api.hetzner.cloud/v1", Location: "fsn1", Registry: "registry.example.com/acme"}.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	for name := range hetznerOutputs() {
		outputs := hetznerOutputs()
		delete(outputs, name)
		if _, _, err := cloud.Agent(outputs); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestHetznerGoldenImageReachesTheAgent(t *testing.T) {
	h := Hetzner{Token: "t", Endpoint: "https://api.hetzner.cloud/v1", Location: "fsn1", Registry: "registry.example.com/acme", GoldenImage: "4711"}
	cloud, err := h.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}

	args, _, err := cloud.Agent(hetznerOutputs())

	if err != nil || !slices.Equal(args[len(args)-2:], []string{"-hcloud-golden-image", "4711"}) {
		t.Fatalf("agent args with a golden image = %v, %v; want -hcloud-golden-image 4711", args, err)
	}
}

func TestHetznerSetupNeedsARegistry(t *testing.T) {
	h := Hetzner{Token: "t", Endpoint: "https://api.hetzner.cloud/v1", Location: "fsn1"}

	if _, err := h.Cloud(fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("Cloud without a registry = %v, want it refused", err)
	}
}
