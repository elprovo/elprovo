//go:build simdocker

package azure

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProviderAgainstSimulation runs a stagelet's machine lifecycle against
// stagely-sim's Azure simulation. As the bootstrap module does, it puts the
// machines in a subnet whose network security group lets in only the agent,
// here the test, and denies the rest of the virtual network. It creates a
// stagelet and a neighbor there, and a third machine in a subnet without
// default outbound access. It waits until cloud-init and Docker Compose are
// ready, runs a Compose project on the stagelet and reaches it on its
// private address, and checks that the neighbor can connect out but not to
// the stagelet, that the third machine can't connect out, and that a
// container can't reach the metadata service or the wireserver except its
// DNS port. It gets registry credentials and deletes the machines. It needs
// a running simulation, named by STAGELY_SIM_AZURE_URL, the service
// principal in ARM_TENANT_ID, ARM_CLIENT_ID, ARM_CLIENT_SECRET and
// ARM_SUBSCRIPTION_ID, and must run where machine addresses are reachable.
// With STAGELY_SIM_AZURE_GOLDEN_IMAGE, the machines boot from that managed
// image. "sim/scripts/check-azure.sh agent" and "image" set them up.
func TestProviderAgainstSimulation(t *testing.T) {
	armURL := os.Getenv("STAGELY_SIM_AZURE_URL")
	if armURL == "" || os.Getenv("ARM_CLIENT_SECRET") == "" {
		t.Fatal("STAGELY_SIM_AZURE_URL and ARM_CLIENT_SECRET must name a running simulation; run sim/scripts/check-azure.sh agent")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	group := "stagely-check-" + randomHex(t)
	p, err := New(Config{
		ARMURL: armURL, TenantID: os.Getenv("ARM_TENANT_ID"), ClientID: os.Getenv("ARM_CLIENT_ID"), ClientSecret: os.Getenv("ARM_CLIENT_SECRET"),
		SubscriptionID: os.Getenv("ARM_SUBSCRIPTION_ID"), ResourceGroup: group, Location: "westeurope",
		SubnetID: "/subscriptions/" + os.Getenv("ARM_SUBSCRIPTION_ID") + "/resourceGroups/" + group + "/providers/Microsoft.Network/virtualNetworks/net/subnets/stagelets",
		Size:     "Standard_B2s", Image: "Canonical:ubuntu-24_04-lts:server:latest", GoldenImage: os.Getenv("STAGELY_SIM_AZURE_GOLDEN_IMAGE"),
		Tag: "stagely-sim-agent-check", AdminUsername: "stagely", HostKeyDir: t.TempDir(), ReadyTimeout: 10 * time.Minute,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	makeNetwork(t, p)
	stagelet, neighbor, isolated := "stagely-check-"+randomHex(t), "stagely-neighbor-"+randomHex(t), "stagely-private-"+randomHex(t)
	private := *p
	private.cfg.SubnetID = privateSubnet(p)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, stagelet)
		_ = p.Delete(ctx, neighbor)
		_ = private.Delete(ctx, isolated)
		if resp, err := p.api.do(ctx, http.MethodDelete, p.groupPath(), "2023-07-01", nil, nil); err == nil && resp.operation != "" {
			_ = p.api.wait(ctx, resp.operation, time.Second)
		}
	})

	for _, name := range []string{stagelet, neighbor} {
		if err := p.Create(t.Context(), name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := private.Create(t.Context(), isolated); err != nil {
		t.Fatalf("create %s: %v", isolated, err)
	}
	addr, err := p.Address(t.Context(), stagelet)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	runComposeProject(t, p, stagelet)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+net.JoinHostPort(addr, "8080")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("reach the Compose service on the machine's address: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "hello from the stagelet") {
		t.Fatalf("service answered %s: %q", resp.Status, body)
	}
	connect := func(host, port string) error {
		_, err := p.machines.Output(t.Context(), neighbor, nil, "timeout", "3", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, port)
		return err
	}
	if err := connect("registry-1.docker.io", "443"); err != nil {
		t.Fatalf("the neighbor can't connect out, so its blocked connection would prove nothing: %v", err)
	}
	if err := connect(addr, "8080"); err == nil {
		t.Fatal("the neighbor reached the stagelet's published port through the network security group")
	}
	if _, err := private.machines.Output(t.Context(), isolated, nil, "timeout", "3", "bash", "-c", `exec 3<>/dev/tcp/registry-1.docker.io/443`); err == nil {
		t.Fatal("a machine without a public IP in a subnet without default outbound access connected to the internet")
	}
	metadata, err := p.machines.Output(t.Context(), stagelet, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "--header", "Metadata: true", "http://169.254.169.254/metadata/instance?api-version=2021-02-01")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the metadata service: %q, %v; want the connection refused", metadata, err)
	}
	wire, err := p.machines.Output(t.Context(), stagelet, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "3", "-qO-", "http://168.63.129.16/machine?comp=goalstate")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the wireserver: %q, %v; want the connection refused", wire, err)
	}
	if _, err := p.machines.Output(t.Context(), stagelet, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "3", "-qO-", "http://168.63.129.16:53/"); err != nil && strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container's connection to the wireserver's DNS port was refused, so its DNS would break: %v", err)
	}
	server, err := p.registryLoginServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, push := range []bool{false, true} {
		if user, password, err := p.RegistryCredentials(t.Context(), server, push); err != nil || user == "" || password == "" {
			t.Fatalf("registry credentials (push %v) = %q, %v", push, user, err)
		}
	}

	if err := private.Delete(t.Context(), isolated); err != nil {
		t.Fatalf("delete %s: %v", isolated, err)
	}
	for _, name := range []string{stagelet, neighbor} {
		if err := p.Delete(t.Context(), name); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
		if exists, err := p.Exists(t.Context(), name); err != nil || exists {
			t.Fatalf("Exists(%s) after delete = %v, %v; want false", name, exists, err)
		}
		if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, name+".pub")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pinned host key of %s still exists after delete: %v", name, err)
		}
	}
}

// makeNetwork creates the provider's resource group, a virtual network
// whose subnet stagelets has a network security group that lets in only
// this test's addresses and default outbound access, as the bootstrap module
// makes it, a private subnet beside it, and a container registry.
func makeNetwork(t *testing.T, p *Provider) {
	t.Helper()
	nsg := p.groupPath() + "/providers/Microsoft.Network/networkSecurityGroups/stagelets"
	p.cfg.Registry = p.groupPath() + "/providers/Microsoft.ContainerRegistry/registries/stagely" + randomHex(t)
	rule := func(name string, priority int, access, protocol string, sources []string) map[string]any {
		return map[string]any{"name": name, "properties": map[string]any{
			"priority": priority, "direction": "Inbound", "access": access, "protocol": protocol, "sourcePortRange": "*",
			"destinationPortRange": "*", "sourceAddressPrefixes": sources, "destinationAddressPrefix": "*",
		}}
	}
	steps := []struct {
		path, version string
		body          map[string]any
	}{
		{p.groupPath(), "2023-07-01", map[string]any{"location": p.cfg.Location}},
		{nsg, networkAPIVersion, map[string]any{"location": p.cfg.Location, "properties": map[string]any{"securityRules": []any{
			rule("from-agent", 100, "Allow", "Tcp", ownAddresses(t)), rule("deny-other-inbound", 4096, "Deny", "*", []string{"*"}),
		}}}},
		{p.groupPath() + "/providers/Microsoft.Network/virtualNetworks/net", networkAPIVersion, map[string]any{
			"location": p.cfg.Location, "properties": map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"10.42.0.0/16"}}},
		}},
		{p.cfg.SubnetID, networkAPIVersion, map[string]any{"properties": map[string]any{
			"addressPrefix": "10.42.16.0/20", "networkSecurityGroup": map[string]string{"id": nsg}, "defaultOutboundAccess": true,
		}}},
		{privateSubnet(p), networkAPIVersion, map[string]any{"properties": map[string]any{
			"addressPrefix": "10.42.32.0/20", "networkSecurityGroup": map[string]string{"id": nsg}, "defaultOutboundAccess": false,
		}}},
		{p.cfg.Registry, registryAPIVersion, map[string]any{"location": p.cfg.Location, "sku": map[string]string{"name": "Basic"}, "properties": map[string]any{"adminUserEnabled": false}}},
	}
	for _, s := range steps {
		resp, err := p.api.do(t.Context(), http.MethodPut, s.path, s.version, s.body, nil)
		if err == nil && resp.operation != "" {
			err = p.api.wait(t.Context(), resp.operation, time.Second)
		}
		if err != nil {
			t.Fatalf("create %s: %v", s.path, err)
		}
	}
}

// privateSubnet is a subnet beside the stagelet subnet with default
// outbound access off.
func privateSubnet(p *Provider) string {
	return strings.TrimSuffix(p.cfg.SubnetID, "/stagelets") + "/private"
}

// ownAddresses returns this host's IPv4 addresses, which the agent's
// connections come from.
func ownAddresses(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			out = append(out, ip.IP.String())
		}
	}
	return out
}

func runComposeProject(t *testing.T, p *Provider, name string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"compose.yaml": `services:
  web:
    build: .
    ports: ["8080:8080"]
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"]
      interval: 1s
      retries: 30
`,
		"Dockerfile": "FROM busybox:1.36\nRUN mkdir /www && echo 'hello from the stagelet' > /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/www\"]\n",
	}
	if _, err := p.machines.Output(t.Context(), name, nil, "mkdir", "-p", "/root/app"); err != nil {
		t.Fatalf("make the project directory: %v", err)
	}
	for file, content := range files {
		local := filepath.Join(dir, file)
		if err := os.WriteFile(local, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := p.CopyIn(t.Context(), name, local, "/root/app/"+file); err != nil {
			t.Fatalf("copy %s: %v", file, err)
		}
	}
	if out, err := p.machines.Output(t.Context(), name, nil, "docker", "compose", "--project-directory", "/root/app", "up", "--build", "--wait", "--detach"); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, out)
	}
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
