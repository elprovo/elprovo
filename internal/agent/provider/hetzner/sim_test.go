//go:build simdocker

package hetzner

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elprovo/elprovo/internal/cloudinit"
)

// TestProviderAgainstSimulation runs a preview's machine lifecycle against
// elprovo-sim's Hetzner Cloud simulation. As setup's module does, it makes a
// private network and a firewall without rules on the preview label, so
// servers accept nothing on their public address, and it lets only its own
// address on the network, the agent's here, connect to servers there. It
// creates a preview server and a neighbor on the network, waits until
// cloud-init and Docker Compose are ready, runs a Compose project on the
// preview and reaches it over the network. The neighbor can connect out
// over the network, but not to the preview's published port or SSH port,
// although Hetzner's firewalls do not filter networks: the preview's own
// rules refuse it. A container on the preview can't connect over the
// network or reach the metadata service. Then the test deletes both servers. It needs a running
// simulation, named by ELPROVO_SIM_HCLOUD_URL (with the /v1 path) and
// ELPROVO_SIM_HCLOUD_TOKEN, and the address range of its private network in
// ELPROVO_SIM_HCLOUD_NETWORK_RANGE, and must run on that network. With
// ELPROVO_SIM_HCLOUD_GOLDEN_IMAGE, the servers boot from that snapshot.
// "sim/scripts/check-hetzner.sh agent" and "image" set them up.
func TestProviderAgainstSimulation(t *testing.T) {
	apiURL, token := os.Getenv("ELPROVO_SIM_HCLOUD_URL"), os.Getenv("ELPROVO_SIM_HCLOUD_TOKEN")
	privateRange, err := netip.ParsePrefix(os.Getenv("ELPROVO_SIM_HCLOUD_NETWORK_RANGE"))
	if apiURL == "" || token == "" || err != nil {
		t.Fatal("ELPROVO_SIM_HCLOUD_URL, ELPROVO_SIM_HCLOUD_TOKEN and ELPROVO_SIM_HCLOUD_NETWORK_RANGE must name a running simulation; run sim/scripts/check-hetzner.sh agent")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	self := addressIn(t, privateRange)
	setup := newAPI(apiURL, token)
	label := "elprovo=check-preview"
	networkID := createNetwork(t, setup, privateRange)
	closeToTheInternet(t, setup, label)
	p, err := New(Config{
		APIURL: apiURL, Token: token, Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04",
		GoldenImage: os.Getenv("ELPROVO_SIM_HCLOUD_GOLDEN_IMAGE"), Label: label,
		NetworkID: networkID, Network: cloudinit.PrivateNetwork{Range: privateRange, Sources: []netip.Addr{self}},
		HostKeyDir: t.TempDir(), ReadyTimeout: 10 * time.Minute,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	name, neighbor := "elprovo-check-"+randomHex(t), "elprovo-neighbor-"+randomHex(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, name)
		_ = p.Delete(ctx, neighbor)
	})

	for _, n := range []string{name, neighbor} {
		if err := p.Create(t.Context(), n); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	addr, err := p.Address(t.Context(), name)
	if err != nil || !privateRange.Contains(netip.MustParseAddr(addr)) {
		t.Fatalf("address = %q, %v; want the preview's address on the network %s", addr, err, privateRange)
	}
	runComposeProject(t, p, name)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+net.JoinHostPort(addr, "8080")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("reach the Compose service on the server's private address: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "hello from the preview") {
		t.Fatalf("service answered %s: %q", resp.Status, body)
	}
	hostname, err := p.machines.Output(t.Context(), name, nil, "curl", "-fsS", "http://169.254.169.254/hetzner/v1/metadata/hostname")
	if err != nil || string(hostname) != name {
		t.Fatalf("the server read its hostname from the metadata service: %q, %v; want %s", hostname, err, name)
	}
	connect := func(host, port string) error {
		_, err := p.machines.Output(t.Context(), neighbor, nil, "timeout", "3", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, port)
		return err
	}
	port := listen(t, self.String())
	if err := connect(self.String(), port); err != nil {
		t.Fatalf("the neighbor can't connect out over the network, so its blocked connections would prove nothing: %v", err)
	}
	if out, err := p.machines.Output(t.Context(), name, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://"+net.JoinHostPort(self.String(), port)+"/"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container on the preview connected over the network: %q, %v; want the connection refused, so containers can't reach other machines there", out, err)
	}
	for _, port := range []string{"8080", "22"} {
		if err := connect(addr, port); err == nil {
			t.Fatalf("the neighbor reached the preview's port %s over the network", port)
		}
	}
	metadata, err := p.machines.Output(t.Context(), name, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://169.254.169.254/hetzner/v1/userdata")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the metadata service: %q, %v; want the connection refused, so containers can't read the user data", metadata, err)
	}

	for _, n := range []string{name, neighbor} {
		if err := p.Delete(t.Context(), n); err != nil {
			t.Fatalf("delete %s: %v", n, err)
		}
		if exists, err := p.Exists(t.Context(), n); err != nil || exists {
			t.Fatalf("Exists(%s) after delete = %v, %v; want false", n, exists, err)
		}
		if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, n+".pub")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pinned host key of %s still exists after delete: %v", n, err)
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

// createNetwork makes a network with the range and one subnet, as setup's
// module does, deletes it when the test ends, and returns its id.
func createNetwork(t *testing.T, a *api, ipRange netip.Prefix) int64 {
	t.Helper()
	var created struct {
		Network struct {
			ID int64 `json:"id"`
		} `json:"network"`
	}
	err := a.do(t.Context(), http.MethodPost, "/networks", map[string]any{
		"name": "agent-check-" + randomHex(t), "ip_range": ipRange.String(),
		"subnets": []map[string]string{{"type": "cloud", "network_zone": "eu-central", "ip_range": ipRange.String()}},
	}, &created)
	if err != nil {
		t.Fatalf("create the network: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = a.do(ctx, http.MethodDelete, "/networks/"+strconv.FormatInt(created.Network.ID, 10), nil, nil)
	})
	return created.Network.ID
}

// closeToTheInternet puts a firewall without rules on the servers with the
// label, as setup's module does, so they accept nothing on their public
// address, and waits until Hetzner has applied it.
func closeToTheInternet(t *testing.T, a *api, label string) {
	t.Helper()
	var created struct {
		Firewall struct {
			ID int64 `json:"id"`
		} `json:"firewall"`
		Actions []struct {
			ID int64 `json:"id"`
		} `json:"actions"`
	}
	err := a.do(t.Context(), http.MethodPost, "/firewalls", map[string]any{
		"name":     "agent-check-" + randomHex(t),
		"rules":    []map[string]any{},
		"apply_to": []map[string]any{{"type": "label_selector", "label_selector": map[string]string{"selector": label}}},
	}, &created)
	if err != nil {
		t.Fatalf("create the firewall: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = a.do(ctx, http.MethodPost, "/firewalls/"+strconv.FormatInt(created.Firewall.ID, 10)+"/actions/remove_from_resources", map[string]any{
			"remove_from": []map[string]any{{"type": "label_selector", "label_selector": map[string]string{"selector": label}}},
		}, nil)
		_ = a.do(ctx, http.MethodDelete, "/firewalls/"+strconv.FormatInt(created.Firewall.ID, 10), nil, nil)
	})
	deadline := time.Now().Add(2 * time.Minute)
	for _, act := range created.Actions {
		for {
			var got struct {
				Action struct {
					Status string `json:"status"`
				} `json:"action"`
			}
			if err := a.do(t.Context(), http.MethodGet, "/actions/"+strconv.FormatInt(act.ID, 10), nil, &got); err != nil {
				t.Fatalf("get the firewall's action: %v", err)
			}
			if got.Action.Status == "success" {
				break
			}
			if got.Action.Status == "error" || time.Now().After(deadline) {
				t.Fatalf("the firewall's action is %q", got.Action.Status)
			}
			time.Sleep(time.Second)
		}
	}
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
