package digitalocean

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/elprovo/elprovo/internal/cloudinit"
)

// fakeCloud is a DigitalOcean API and the SSH server of every droplet it
// creates. Droplets present the host key from their user data.
type fakeCloud struct {
	t       *testing.T
	agent   ssh.PublicKey
	sshAddr string

	mu         sync.Mutex
	droplets   map[int64]droplet
	hostKeys   map[string]ssh.Signer
	created    []createDroplet
	keysAdded  int
	keyKnown   bool
	deletePoll int
	commands   []string
	stdin      []string

	credentialQueries []string
}

func newFakeCloud(t *testing.T, agent ssh.PublicKey) *fakeCloud {
	t.Helper()
	c := &fakeCloud{t: t, agent: agent, droplets: map[int64]droplet{}, hostKeys: map[string]ssh.Signer{}}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c.sshAddr = ln.Addr().String()
	go c.serveSSH(ln)
	return c
}

func (c *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer do-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/account/keys/"):
		if !c.keyKnown {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v2/account/keys":
		c.keysAdded++
		c.keyKnown = true
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets":
		var req createDroplet
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.created = append(c.created, req)
		d := droplet{ID: int64(len(c.created)), Name: req.Name, Status: "active"}
		d.Networks.V4 = append(d.Networks.V4,
			struct {
				IPAddress string `json:"ip_address"`
				Type      string `json:"type"`
			}{"127.0.0.1", "public"},
			struct {
				IPAddress string `json:"ip_address"`
				Type      string `json:"type"`
			}{"127.0.0.2", "private"})
		c.droplets[d.ID] = d
		c.hostKeys[req.Name] = hostKeyFromUserData(c.t, req.UserData)
		writeJSONBody(w, http.StatusAccepted, map[string]droplet{"droplet": d})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/registry/docker-credentials":
		c.credentialQueries = append(c.credentialQueries, r.URL.RawQuery)
		user := "pull-only"
		if r.URL.Query().Get("read_write") == "true" {
			user = "read-write"
		}
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":scoped-" + user))
		writeJSONBody(w, http.StatusOK, map[string]any{"auths": map[string]any{"registry.digitalocean.com": map[string]string{"auth": auth}}})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
		var list []droplet
		for _, d := range c.droplets {
			list = append(list, d)
		}
		writeJSONBody(w, http.StatusOK, map[string][]droplet{"droplets": list})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/droplets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v2/droplets/"), 10, 64)
		d, ok := c.droplets[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if d.Status == "deleting" {
			if c.deletePoll--; c.deletePoll <= 0 {
				delete(c.droplets, id)
			}
		}
		writeJSONBody(w, http.StatusOK, map[string]droplet{"droplet": d})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/droplets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v2/droplets/"), 10, 64)
		d := c.droplets[id]
		d.Status = "deleting"
		c.droplets[id] = d
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func hostKeyFromUserData(t *testing.T, userData string) ssh.Signer {
	t.Helper()
	_, after, _ := strings.Cut(userData, "ed25519_private: |\n")
	pemText, _, _ := strings.Cut(after, "  ed25519_public:")
	var lines []string
	for _, line := range strings.Split(pemText, "\n") {
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	signer, err := ssh.ParsePrivateKey([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("user data has no usable host key: %v", err)
	}
	return signer
}

// serveSSH accepts the agent's key and records each command and its stdin.
// Every droplet shares this one server, so it presents the host key of the
// most recently created droplet.
func (c *fakeCloud) serveSSH(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go c.serveConn(conn)
	}
}

func (c *fakeCloud) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), c.agent.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	c.mu.Lock()
	for _, req := range c.created {
		config.AddHostKey(c.hostKeys[req.Name])
	}
	c.mu.Unlock()
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		for req := range requests {
			if req.Type != "exec" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			command := string(req.Payload[4:])
			input, _ := io.ReadAll(ch)
			c.mu.Lock()
			c.commands = append(c.commands, command)
			c.stdin = append(c.stdin, string(input))
			c.mu.Unlock()
			_, _ = io.WriteString(ch, "ran\n")
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 0)
			_, _ = ch.SendRequest("exit-status", false, status)
			_ = ch.Close()
		}
	}
}

func newProvider(t *testing.T, vpc string) (*Provider, *fakeCloud) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, _ := ssh.NewSignerFromKey(key)
	cloud := newFakeCloud(t, signer.PublicKey())
	api := httptest.NewServer(cloud)
	t.Cleanup(api.Close)
	p, err := New(Config{
		APIURL: api.URL, Token: "do-token", Region: "fra1", Size: "s-2vcpu-4gb", Image: "ubuntu-24-04-x64",
		Tag: "elprovo-preview", VPCUUID: vpc, HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
	}, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func TestCreateMakesADropletTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()

	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	if req.Region != "fra1" || req.Size != "s-2vcpu-4gb" || req.Tags[0] != "elprovo-preview" || len(req.SSHKeys) != 1 {
		t.Fatalf("create request = %+v", req)
	}
	if !strings.Contains(req.UserData, "docker-compose-v2") {
		t.Fatal("user data does not install Docker Compose")
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "elprovo-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
}

func TestRunRefusesADropletWithAnotherHostKey(t *testing.T) {
	p, _ := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherPublic, _ := ssh.NewPublicKey(other.Public())
	if err := os.WriteFile(filepath.Join(p.cfg.HostKeyDir, "elprovo-a1"+".pub"), ssh.MarshalAuthorizedKey(otherPublic), 0o600); err != nil {
		t.Fatalf("pin: %v", err)
	}

	if err := p.Run(ctx, "elprovo-a1", nil, nil, nil, "true"); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("Run err = %v, want a failed handshake", err)
	}
}

func TestCopyInStreamsTheFile(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	local := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(local, []byte("tarball"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.CopyIn(ctx, "elprovo-a1", local, "/tmp/source.tar.gz"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	last := len(cloud.commands) - 1
	if cloud.commands[last] != `'sh' '-c' 'cat > "$1"' 'sh' '/tmp/source.tar.gz'` || cloud.stdin[last] != "tarball" {
		t.Fatalf("command = %s with stdin %q", cloud.commands[last], cloud.stdin[last])
	}
}

func TestAgentUsesThePrivateAddressInsideAVPC(t *testing.T) {
	p, _ := newProvider(t, "vpc-1")
	ctx := context.Background()
	if _, err := p.api.createDroplet(ctx, createDroplet{Name: "elprovo-a1", Tags: []string{"elprovo-preview"}, UserData: mustCloudConfig(t)}); err != nil {
		t.Fatal(err)
	}

	addr, err := p.Address(ctx, "elprovo-a1")
	if err != nil || addr != "127.0.0.2" {
		t.Fatalf("Address = %q, %v; want the private address", addr, err)
	}
}

func TestSSHKeyIsRegisteredOnce(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()

	for _, name := range []string{"elprovo-a1", "elprovo-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if cloud.keysAdded != 1 {
		t.Fatalf("keys added = %d, want 1", cloud.keysAdded)
	}
}

func TestRegistryCredentialsAreScopedAndExpire(t *testing.T) {
	p, cloud := newProvider(t, "")

	pushUser, pushPassword, err := p.RegistryCredentials(t.Context(), "registry.digitalocean.com", true)
	if err != nil {
		t.Fatalf("push credentials: %v", err)
	}
	pullUser, pullPassword, err := p.RegistryCredentials(t.Context(), "registry.digitalocean.com", false)
	if err != nil {
		t.Fatalf("pull credentials: %v", err)
	}

	if pushUser != "read-write" || pushPassword != "scoped-read-write" || pullUser != "pull-only" || pullPassword != "scoped-pull-only" {
		t.Fatalf("credentials = %s:%s and %s:%s; want read-write ones to push and pull-only ones otherwise", pushUser, pushPassword, pullUser, pullPassword)
	}
	want := []string{"expiry_seconds=7200&read_write=true", "expiry_seconds=7200&read_write=false"}
	if !slices.Equal(cloud.credentialQueries, want) {
		t.Fatalf("queries = %v, want %v", cloud.credentialQueries, want)
	}
	if _, _, err := p.RegistryCredentials(t.Context(), "registry.example.com", false); err == nil {
		t.Fatal("credentials for another registry host were accepted")
	}
}

func TestDeleteWaitsUntilTheDropletIsGone(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.deletePoll = 3

	if err := p.Delete(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, err := p.Exists(ctx, "elprovo-a1"); err != nil || exists {
		t.Fatalf("Exists = %v, %v; want gone", exists, err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, "elprovo-a1"+".pub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host key file err = %v, want it removed", err)
	}
}

func TestDeleteFailsWhileTheDropletStillExists(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.deletePoll = 1 << 30
	p.cfg.ReadyTimeout = 100 * time.Millisecond

	if err := p.Delete(ctx, "elprovo-a1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("Delete err = %v, want still exists", err)
	}
}

func TestDeletingAMissingDropletSucceeds(t *testing.T) {
	p, _ := newProvider(t, "")

	if err := p.Delete(context.Background(), "elprovo-none"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func mustCloudConfig(t *testing.T) string {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	userData, _, err := cloudinit.HostKeyConfig(string(pem.EncodeToMemory(block)), machinePackages...)
	if err != nil {
		t.Fatal(err)
	}
	return userData
}

func TestNewMachineGetsOnlyItsHostKeyDockerAndFirewallRules(t *testing.T) {
	p, cloud := newProvider(t, "")

	if err := p.Create(context.Background(), "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	userData := cloud.created[0].UserData
	if got, want := cloudConfigSections(userData), []string{"ssh_deletekeys", "ssh_genkeytypes", "ssh_keys", "package_update", "packages", "bootcmd"}; !slices.Equal(got, want) {
		t.Fatalf("user data sections = %v, want %v", got, want)
	}
	if !strings.Contains(userData, "packages:\n  - docker.io\n  - docker-compose-v2\n  - docker-buildx\nbootcmd:\n") {
		t.Fatalf("user data installs packages other than Docker:\n%s", userData)
	}
}

func cloudConfigSections(userData string) []string {
	var sections []string
	for line := range strings.Lines(userData) {
		if key, _, found := strings.Cut(line, ":"); found && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			sections = append(sections, key)
		}
	}
	return sections
}

func TestGoldenImageReplacesTheStockImageAndItsPackages(t *testing.T) {
	p, cloud := newProvider(t, "")
	p.cfg.GoldenImage = "176543210"

	if err := p.Create(context.Background(), "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	if req.Image != "176543210" || strings.Contains(req.UserData, "packages:") {
		t.Fatalf("create request image = %v, user data installs packages = %v; want the golden image and no packages", req.Image, strings.Contains(req.UserData, "packages:"))
	}
}
