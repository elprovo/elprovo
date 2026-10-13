//go:build simdocker

package vultr

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elprovo/elprovo/internal/cloudinit"
)

// TestProviderAgainstSimulation runs a preview's machine lifecycle against
// elprovo-sim's Vultr simulation. As setup's module does, it makes a VPC and
// a firewall group without rules, so instances accept nothing on their
// public interface, and it lets only its own address in the VPC, the
// agent's here, connect to instances there. It creates a preview and a
// neighbor instance in both, waits until cloud-init and Docker Compose are
// ready, runs a Compose project on the preview and reaches it over the VPC.
// The neighbor can connect out, to the internet and over the VPC, but not to
// the preview's published port or SSH port, although Vultr's firewall does
// not filter the VPC: the preview's own rules refuse it. A container on the
// preview can't connect over the VPC or reach the metadata service. Then
// the test deletes both instances. It calls https://api.vultr.com with the key in VULTR_API_KEY,
// so it must run where that name reaches a running simulation, on the
// simulation's private network, whose address range is in
// ELPROVO_SIM_VULTR_NETWORK_RANGE. With ELPROVO_SIM_VULTR_GOLDEN_IMAGE, the
// instances boot from that snapshot. "sim/scripts/check-vultr.sh agent" and
// "image" set them up.
func TestProviderAgainstSimulation(t *testing.T) {
	key := os.Getenv("VULTR_API_KEY")
	privateRange, err := netip.ParsePrefix(os.Getenv("ELPROVO_SIM_VULTR_NETWORK_RANGE"))
	if key == "" || err != nil {
		t.Fatal("VULTR_API_KEY must name a key of a running simulation at api.vultr.com, and ELPROVO_SIM_VULTR_NETWORK_RANGE its private network; run sim/scripts/check-vultr.sh agent")
	}
	_, sshKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	self := addressIn(t, privateRange)
	cfg := Config{
		APIURL: "https://api.vultr.com", APIKey: key, Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284,
		GoldenImage: os.Getenv("ELPROVO_SIM_VULTR_GOLDEN_IMAGE"),
		Tag:         "elprovo-sim-agent-check", HostKeyDir: t.TempDir(), ReadyTimeout: 10 * time.Minute,
		Network: cloudinit.PrivateNetwork{Range: privateRange, Sources: []netip.Addr{self}},
	}
	setup := newAPI(cfg.APIURL, key)
	cfg.FirewallGroupID = createSimResource(t, setup, "/v2/firewalls", map[string]any{"description": "agent check"}, "firewall_group")
	cfg.VPCID = createSimResource(t, setup, "/v2/vpcs", map[string]any{
		"region": "ewr", "description": "agent check", "v4_subnet": privateRange.Addr().String(), "v4_subnet_mask": privateRange.Bits(),
	}, "vpc")
	var registry struct {
		ID  string `json:"id"`
		URN string `json:"urn"`
	}
	err = setup.do(t.Context(), http.MethodPost, "/v2/registry", map[string]any{"name": "agentcheck" + randomHex(t), "region": "ewr", "plan": "start_up", "public": false}, &registry)
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	cfg.RegistryID = registry.ID
	p, err := New(cfg, sshKey)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	preview, neighbor := "elprovo-check-"+randomHex(t), "elprovo-neighbor-"+randomHex(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, preview)
		_ = p.Delete(ctx, neighbor)
		_ = setup.do(ctx, http.MethodDelete, "/v2/firewalls/"+cfg.FirewallGroupID, nil, nil)
		_ = setup.do(ctx, http.MethodDelete, "/v2/vpcs/"+cfg.VPCID, nil, nil)
		_ = setup.do(ctx, http.MethodDelete, "/v2/registry/"+cfg.RegistryID, nil, nil)
	})
	host, _, _ := strings.Cut(registry.URN, "/")
	for _, push := range []bool{true, false} {
		if user, password, err := p.RegistryCredentials(t.Context(), host, push); err != nil || user == "" || password == "" {
			t.Fatalf("registry credentials for %s (push %v) = %q, %v", host, push, user, err)
		}
	}

	for _, name := range []string{preview, neighbor} {
		if err := p.Create(t.Context(), name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	addr, err := p.Address(t.Context(), preview)
	if err != nil || !privateRange.Contains(netip.MustParseAddr(addr)) {
		t.Fatalf("address = %q, %v; want the preview's address in the VPC %s", addr, err, privateRange)
	}
	runComposeProject(t, p, preview)
	if body := get(t, "http://"+net.JoinHostPort(addr, "8080")+"/"); !strings.Contains(body, "hello from the preview") {
		t.Fatalf("service on the private address answered %q", body)
	}
	connect := func(host, port string) error {
		_, err := p.machines.Output(t.Context(), neighbor, nil, "timeout", "3", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, port)
		return err
	}
	if err := connect("api.vultr.com", "443"); err != nil {
		t.Fatalf("the neighbor can't connect out, so its blocked connections would prove nothing: %v", err)
	}
	port := listen(t, self.String())
	if err := connect(self.String(), port); err != nil {
		t.Fatalf("the neighbor can't connect out over the VPC, so its blocked connections would prove nothing: %v", err)
	}
	if out, err := p.machines.Output(t.Context(), preview, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://"+net.JoinHostPort(self.String(), port)+"/"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container on the preview connected over the VPC: %q, %v; want the connection refused, so containers can't reach other machines there", out, err)
	}
	for _, port := range []string{"8080", "22"} {
		if err := connect(addr, port); err == nil {
			t.Fatalf("the neighbor reached the preview's port %s over the VPC", port)
		}
	}
	metadata, err := p.machines.Output(t.Context(), preview, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://169.254.169.254/v1.json")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the metadata service: %q, %v; want the connection refused, so containers can't read the user data", metadata, err)
	}

	for _, name := range []string{preview, neighbor} {
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

// addressIn returns this host's address in the range.
func addressIn(t *testing.T, prefix netip.Prefix) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if addr, ok := netip.AddrFromSlice(n.IP); ok && prefix.Contains(addr.Unmap()) {
				return addr.Unmap()
			}
		}
	}
	t.Fatalf("this host has no address in %s", prefix)
	return netip.Addr{}
}

// listen accepts TCP connections on addr until the test ends and returns
// the port.
func listen(t *testing.T, addr string) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// createSimResource creates a resource the agent expects setup to have made
// and returns its id.
func createSimResource(t *testing.T, a *api, path string, body map[string]any, key string) string {
	t.Helper()
	var created map[string]struct {
		ID string `json:"id"`
	}
	if err := a.do(t.Context(), http.MethodPost, path, body, &created); err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
	return created[key].ID
}

func get(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
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
		"Dockerfile": "FROM busybox:1.36\nRUN mkdir /www && echo 'hello from the preview' > /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/www\"]\n",
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
