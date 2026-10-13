package hetzner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

const (
	testToken   = "hcloud-token"
	testNetwork = 4242
)

// testNetworkAccess lets the agent at 10.42.0.2 and the gateway at
// 10.42.0.3 connect to servers.
var testNetworkAccess = cloudinit.PrivateNetwork{
	Range: netip.MustParsePrefix("10.42.0.0/20"), Sources: []netip.Addr{netip.MustParseAddr("10.42.0.2"), netip.MustParseAddr("10.42.0.3")},
}

type fakeServer struct {
	server
	labels map[string]string
}

// fakeCloud is a Hetzner Cloud API and the SSH server of every server it
// creates. Servers present the host key from their user data.
type fakeCloud struct {
	t       *testing.T
	agent   ssh.PublicKey
	sshAddr string

	mu         sync.Mutex
	nextID     int64
	servers    map[int64]*fakeServer
	deleting   map[int64]int
	hostKeys   map[string]ssh.Signer
	created    []createServer
	keyIDs     map[string]int64
	keysAdded  int
	deletePoll int
	// lockedDeletes is how many deletes answer 423 locked first.
	lockedDeletes int
	commands      []string
	stdin         []string
}

func newFakeCloud(t *testing.T, agent ssh.PublicKey) *fakeCloud {
	t.Helper()
	c := &fakeCloud{
		t: t, agent: agent, nextID: 100, servers: map[int64]*fakeServer{}, deleting: map[int64]int{},
		hostKeys: map[string]ssh.Signer{}, keyIDs: map[string]int64{},
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c.sshAddr = ln.Addr().String()
	go c.serveSSH(ln)
	return c
}

// addServer puts a running server with the labels into the project, as
// another client could. Its address on the test network is the fake SSH
// server's.
func (c *fakeCloud) addServer(name string, labels map[string]string) *fakeServer {
	c.nextID++
	s := &fakeServer{server: server{ID: c.nextID, Name: name, Status: "running", PrivateNet: []privateNet{{Network: testNetwork, IP: "127.0.0.1"}}}, labels: labels}
	c.servers[s.ID] = s
	return s
}

func (c *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		writeJSONBody(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthorized", "message": "unable to authenticate"}})
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/ssh_keys":
		var keys []sshKey
		if id, ok := c.keyIDs[q.Get("fingerprint")]; ok {
			keys = append(keys, sshKey{ID: id})
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"ssh_keys": keys})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/ssh_keys":
		var req struct {
			PublicKey string `json:"public_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		c.keysAdded++
		c.keyIDs[ssh.FingerprintLegacyMD5(key)] = int64(900 + c.keysAdded)
		writeJSONBody(w, http.StatusCreated, map[string]any{"ssh_key": sshKey{ID: int64(900 + c.keysAdded)}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/servers":
		var req createServer
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.created = append(c.created, req)
		s := c.addServer(req.Name, req.Labels)
		c.hostKeys[req.Name] = hostKeyFromUserData(c.t, req.UserData)
		writeJSONBody(w, http.StatusCreated, map[string]any{"server": s.server})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/servers":
		c.listServers(w, q)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/servers/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v1/servers/"), 10, 64)
		s, ok := c.servers[id]
		if !ok {
			writeJSONBody(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "server not found"}})
			return
		}
		if _, deleting := c.deleting[id]; deleting {
			if c.deleting[id]--; c.deleting[id] <= 0 {
				delete(c.servers, id)
			}
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"server": s.server})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/servers/") && c.lockedDeletes > 0:
		c.lockedDeletes--
		writeJSONBody(w, http.StatusLocked, map[string]any{"error": map[string]string{"code": "locked", "message": "server is locked"}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/servers/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v1/servers/"), 10, 64)
		s := c.servers[id]
		s.Status = "deleting"
		c.deleting[id] = c.deletePoll
		writeJSONBody(w, http.StatusOK, map[string]any{"action": map[string]any{"id": 1, "command": "delete_server", "status": "running"}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// listServers answers GET /servers with the servers whose label matches
// the label_selector key==value or key=value, filtered by name, one page
// at a time.
func (c *fakeCloud) listServers(w http.ResponseWriter, q map[string][]string) {
	selector := strings.Replace(first(q["label_selector"]), "==", "=", 1)
	key, value, _ := strings.Cut(selector, "=")
	var matched []server
	for id := int64(101); id <= c.nextID; id++ {
		s, ok := c.servers[id]
		if ok && s.labels[key] == value && (first(q["name"]) == "" || s.Name == first(q["name"])) {
			matched = append(matched, s.server)
		}
	}
	page, _ := strconv.Atoi(first(q["page"]))
	perPage, _ := strconv.Atoi(first(q["per_page"]))
	start := min((page-1)*perPage, len(matched))
	end := min(start+perPage, len(matched))
	var next *int
	if end < len(matched) {
		n := page + 1
		next = &n
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"servers": matched[start:end], "meta": map[string]any{"pagination": map[string]any{"page": page, "next_page": next}},
	})
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
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
// Every server shares this one SSH server, which presents the host keys of
// all servers created so far.
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

func newProvider(t *testing.T) (*Provider, *fakeCloud) {
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
		APIURL: api.URL + "/v1", Token: testToken, Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04",
		Label: "elprovo=prod-preview", NetworkID: testNetwork, Network: testNetworkAccess, HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
	}, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func TestCreateMakesAServerTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()

	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	if req.Location != "fsn1" || req.ServerType != "cx23" || req.Image != "ubuntu-24.04" || req.Labels["elprovo"] != "prod-preview" || len(req.SSHKeys) != 1 {
		t.Fatalf("create request = %+v", req)
	}
	if !req.PublicNet.EnableIPv4 || req.PublicNet.EnableIPv6 {
		t.Fatalf("public net = %+v, want IPv4 only", req.PublicNet)
	}
	if len(req.Networks) != 1 || req.Networks[0] != testNetwork {
		t.Fatalf("networks = %v, want the server on network %d", req.Networks, testNetwork)
	}
	if !strings.Contains(req.UserData, "docker-compose-v2") || !strings.Contains(req.UserData, "DOCKER-USER -d 169.254.169.254") {
		t.Fatal("user data does not install Docker Compose or keep containers off the metadata service")
	}
	for _, rule := range []string{"ELPROVO-PRIVATE -s 10.42.0.2 -j RETURN", "ELPROVO-PRIVATE -s 10.42.0.3 -j RETURN", "DOCKER-USER -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE"} {
		if !strings.Contains(req.UserData, rule) {
			t.Fatalf("user data lacks %q, so the network's other servers could connect to it:\n%s", rule, req.UserData)
		}
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "elprovo-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
}

func TestAddressIsTheServersAddressOnTheEnvironmentsNetwork(t *testing.T) {
	p, cloud := newProvider(t)
	s := cloud.addServer("elprovo-a1", map[string]string{"elprovo": "prod-preview"})
	s.PrivateNet = []privateNet{{Network: 7, IP: "10.7.0.5"}, {Network: testNetwork, IP: "10.42.0.9"}}
	cloud.addServer("elprovo-a2", map[string]string{"elprovo": "prod-preview"}).PrivateNet = nil

	addr, err := p.Address(context.Background(), "elprovo-a1")
	if err != nil || addr != "10.42.0.9" {
		t.Fatalf("Address = %q, %v; want the address on network %d", addr, err, testNetwork)
	}
	if addr, err := p.Address(context.Background(), "elprovo-a2"); err == nil {
		t.Fatalf("Address of a server off the network = %q; want an error, not its public address", addr)
	}
}

func TestNewRefusesToRunWithoutThePrivateNetwork(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for name, change := range map[string]func(*Config){
		"no network":         func(c *Config) { c.NetworkID = 0 },
		"no range":           func(c *Config) { c.Network.Range = netip.Prefix{} },
		"nobody may connect": func(c *Config) { c.Network.Sources = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Token: "t", Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04", Label: "elprovo=x", NetworkID: testNetwork, Network: testNetworkAccess, HostKeyDir: t.TempDir()}
			change(&cfg)

			if _, err := New(cfg, key); err == nil || !strings.Contains(err.Error(), "private network") {
				t.Fatalf("New = %v, want it refused", err)
			}
		})
	}
}

func TestRunRefusesAServerWithAnotherHostKey(t *testing.T) {
	p, _ := newProvider(t)
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
	p, cloud := newProvider(t)
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

func TestSSHKeyIsRegisteredOnce(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()

	for _, name := range []string{"elprovo-a1", "elprovo-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if cloud.keysAdded != 1 || cloud.created[0].SSHKeys[0] != cloud.created[1].SSHKeys[0] {
		t.Fatalf("keys added = %d, server keys %v and %v; want one key on both", cloud.keysAdded, cloud.created[0].SSHKeys, cloud.created[1].SSHKeys)
	}
}

func TestKnownSSHKeyIsReused(t *testing.T) {
	p, cloud := newProvider(t)
	cloud.keyIDs[ssh.FingerprintLegacyMD5(p.machines.Signer.PublicKey())] = 4711

	if err := p.Create(context.Background(), "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cloud.keysAdded != 0 || cloud.created[0].SSHKeys[0] != 4711 {
		t.Fatalf("keys added = %d, server keys %v; want the known key 4711", cloud.keysAdded, cloud.created[0].SSHKeys)
	}
}

func TestProviderSeesOnlyServersWithItsLabel(t *testing.T) {
	p, cloud := newProvider(t)
	cloud.addServer("elprovo-a1", map[string]string{"elprovo": "prod-preview"})
	cloud.addServer("elprovo-a2", map[string]string{"elprovo": "dev-preview"})
	cloud.addServer("elprovo-a3", nil)

	names, err := p.List(context.Background())
	if err != nil || len(names) != 1 || names[0] != "elprovo-a1" {
		t.Fatalf("List = %v, %v; want only the server with the provider's label", names, err)
	}
	if exists, err := p.Exists(context.Background(), "elprovo-a2"); err != nil || exists {
		t.Fatalf("Exists(another environment's server) = %v, %v; want false", exists, err)
	}
	if err := p.Delete(context.Background(), "elprovo-a3"); err != nil || len(cloud.deleting) != 0 {
		t.Fatalf("Delete(an unlabeled server) = %v, deleting %v; want nothing deleted", err, cloud.deleting)
	}
}

func TestListFollowsEveryPage(t *testing.T) {
	p, cloud := newProvider(t)
	for i := range pageSize + 3 {
		cloud.addServer(fmt.Sprintf("elprovo-build-%d", i+1), map[string]string{"elprovo": "prod-preview"})
	}

	names, err := p.List(context.Background())

	if err != nil || len(names) != pageSize+3 {
		t.Fatalf("List = %d names, %v; want %d across two pages", len(names), err, pageSize+3)
	}
}

func TestDeleteWaitsUntilTheServerIsGone(t *testing.T) {
	p, cloud := newProvider(t)
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

func TestDeleteFailsWhileTheServerStillExists(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.deletePoll = 1 << 30
	p.cfg.ReadyTimeout = 100 * time.Millisecond

	if err := p.Delete(ctx, "elprovo-a1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("Delete err = %v, want still exists", err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, "elprovo-a1"+".pub")); err != nil {
		t.Fatalf("host key of a server that still exists: %v; want it kept", err)
	}
}

func TestDeleteWaitsWhileAnActionLocksTheServer(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()
	if err := p.Create(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.lockedDeletes = 2

	if err := p.Delete(ctx, "elprovo-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, err := p.Exists(ctx, "elprovo-a1"); err != nil || exists {
		t.Fatalf("Exists = %v, %v; want the server deleted once its lock cleared", exists, err)
	}
}

func TestDeletingAMissingServerSucceeds(t *testing.T) {
	p, _ := newProvider(t)

	if err := p.Delete(context.Background(), "elprovo-none"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestNewMachineGetsOnlyItsHostKeyDockerAndFirewallRules(t *testing.T) {
	p, cloud := newProvider(t)

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
	p, cloud := newProvider(t)
	p.cfg.GoldenImage = "176543210"

	if err := p.Create(context.Background(), "elprovo-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	if req.Image != "176543210" || strings.Contains(req.UserData, "packages:") {
		t.Fatalf("create request image = %v, user data installs packages = %v; want the golden image and no packages", req.Image, strings.Contains(req.UserData, "packages:"))
	}
}

func TestAPIErrorsCarryHetznersMessage(t *testing.T) {
	p, _ := newProvider(t)
	p.api.token = "wrong"

	_, err := p.List(context.Background())

	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("List with a wrong token = %v, want Hetzner's message and code", err)
	}
}

func TestNewRefusesALabelThatIsNotKeyValue(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, label := range []string{"elprovo", "elprovo!=x", "elprovo=", "elprovo=a b", "Bad_Prefix/elprovo=x"} {
		t.Run(label, func(t *testing.T) {
			_, err := New(Config{Token: "t", Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04", Label: label, NetworkID: testNetwork, Network: testNetworkAccess, HostKeyDir: t.TempDir()}, key)

			if err == nil || !strings.Contains(err.Error(), "key=value") {
				t.Fatalf("New with label %q = %v, want it refused, so the agent never selects other servers", label, err)
			}
		})
	}
}

func TestNewAcceptsAPrefixedLabel(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)

	p, err := New(Config{Token: "t", Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04", Label: "elprovo.com/env=prod-preview", NetworkID: testNetwork, Network: testNetworkAccess, HostKeyDir: t.TempDir()}, key)

	if err != nil || p.selector != "elprovo.com/env==prod-preview" || p.labels["elprovo.com/env"] != "prod-preview" {
		t.Fatalf("New with a prefixed label = %+v, %v", p, err)
	}
}
