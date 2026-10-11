//go:build simdocker

package digitalocean

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
// stagely-sim's DigitalOcean simulation: create a stagelet droplet and a
// neighbor, wait until cloud-init and Docker Compose are ready, and run a
// Compose project on the stagelet. As setup's module does for the agent, a
// cloud firewall on the tag lets in only the test's address, so the neighbor
// must not reach the stagelet's published port. Then it deletes both.
// It needs a running simulation, named by STAGELY_SIM_DO_URL and
// STAGELY_SIM_DO_TOKEN, and must run where droplet addresses are reachable.
// With STAGELY_SIM_DO_GOLDEN_IMAGE, the droplet boots from that snapshot.
// "sim/scripts/check-digitalocean.sh agent" and "image" set them up.
func TestProviderAgainstSimulation(t *testing.T) {
	apiURL, token := os.Getenv("STAGELY_SIM_DO_URL"), os.Getenv("STAGELY_SIM_DO_TOKEN")
	if apiURL == "" || token == "" {
		t.Fatal("STAGELY_SIM_DO_URL and STAGELY_SIM_DO_TOKEN must name a running simulation; run sim/scripts/check-digitalocean.sh agent")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{
		APIURL: apiURL, Token: token, Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu-24-04-x64",
		GoldenImage: os.Getenv("STAGELY_SIM_DO_GOLDEN_IMAGE"),
		Tag:         "stagely-sim-agent-check", HostKeyDir: t.TempDir(), ReadyTimeout: 10 * time.Minute,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	name, neighbor := "stagely-check-"+randomHex(t), "stagely-neighbor-"+randomHex(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, name)
		_ = p.Delete(ctx, neighbor)
	})
	if err := p.api.do(t.Context(), http.MethodPost, "/v2/tags", map[string]string{"name": p.cfg.Tag}, nil); err != nil {
		t.Fatalf("create the tag: %v", err)
	}

	for _, n := range []string{name, neighbor} {
		if err := p.Create(t.Context(), n); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	addr, err := p.Address(t.Context(), name)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	egress := egressAddress(t, net.JoinHostPort(addr, "22"))
	allowOnly(t, p.api, p.cfg.Tag, egress)
	runComposeProject(t, p, name)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+net.JoinHostPort(addr, "8080")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("reach the Compose service on the droplet's address: %v", err)
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
	if err := connect(egress, listen(t, egress)); err != nil {
		t.Fatalf("the neighbor can't connect out, so its blocked connection would prove nothing: %v", err)
	}
	if err := connect(addr, "8080"); err == nil {
		t.Fatal("the neighbor reached the stagelet's published port through the cloud firewall")
	}
	metadata, err := p.machines.Output(t.Context(), name, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://169.254.169.254/metadata/v1/user-data")
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

// allowOnly puts a cloud firewall on the tag that lets in TCP only from
// source, as setup's module does for the agent and the gateway, and waits
// until DigitalOcean reports it applied to every droplet with the tag.
func allowOnly(t *testing.T, a *api, tag, source string) {
	t.Helper()
	everywhere := map[string]any{"addresses": []string{"0.0.0.0/0"}}
	var created struct {
		Firewall struct {
			ID string `json:"id"`
		} `json:"firewall"`
	}
	err := a.do(t.Context(), http.MethodPost, "/v2/firewalls", map[string]any{
		"name": "agent-check-" + randomHex(t), "tags": []string{tag},
		"inbound_rules": []map[string]any{{"protocol": "tcp", "ports": "all", "sources": map[string]any{"addresses": []string{source + "/32"}}}},
		"outbound_rules": []map[string]any{
			{"protocol": "tcp", "ports": "all", "destinations": everywhere},
			{"protocol": "udp", "ports": "all", "destinations": everywhere},
		},
	}, &created)
	if err != nil {
		t.Fatalf("create the firewall: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = a.do(ctx, http.MethodDelete, "/v2/firewalls/"+created.Firewall.ID, nil, nil)
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var got struct {
			Firewall struct {
				Status string `json:"status"`
			} `json:"firewall"`
		}
		if err := a.do(t.Context(), http.MethodGet, "/v2/firewalls/"+created.Firewall.ID, nil, &got); err != nil {
			t.Fatalf("get the firewall: %v", err)
		}
		if got.Firewall.Status == "succeeded" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the firewall is still %q after two minutes", got.Firewall.Status)
		}
		time.Sleep(time.Second)
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

// egressAddress returns the address this host sends from toward hostPort.
func egressAddress(t *testing.T, hostPort string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "udp", hostPort)
	if err != nil {
		t.Fatalf("find the address toward %s: %v", hostPort, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
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
