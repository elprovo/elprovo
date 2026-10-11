package vultr

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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

	"github.com/stagely-dev/stagely/internal/cloudinit"
)

// testNetworkAccess lets the agent at 10.42.0.2 and the gateway at
// 10.42.0.3 connect to instances.
var testNetworkAccess = cloudinit.PrivateNetwork{
	Range: netip.MustParsePrefix("10.42.0.0/20"), Sources: []netip.Addr{netip.MustParseAddr("10.42.0.2"), netip.MustParseAddr("10.42.0.3")},
}

// fakeCloud is a Vultr API and the SSH server of every instance it creates.
// Instances present the host key from their user data. Lists come in pages
// of two, so callers must follow the cursor.
type fakeCloud struct {
	t       *testing.T
	agent   ssh.PublicKey
	sshAddr string

	mu         sync.Mutex
	instances  []instance
	hostKeys   map[string]ssh.Signer
	created    []createInstance
	keys       []sshKey
	keysAdded  int
	deletePoll map[string]int
	commands   []string
	stdin      []string

	credentialQueries []string
}

func newFakeCloud(t *testing.T, agent ssh.PublicKey) *fakeCloud {
	t.Helper()
	c := &fakeCloud{t: t, agent: agent, hostKeys: map[string]ssh.Signer{}, deletePoll: map[string]int{}}
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
	if r.Header.Get("Authorization") != "Bearer vultr-key" {
		writeJSONBody(w, http.StatusUnauthorized, map[string]any{"error": "Invalid API token.", "status": 401})
		return
	}
	id, hasID := strings.CutPrefix(r.URL.Path, "/v2/instances/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/ssh-keys":
		writePage(w, r, "ssh_keys", c.keys)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/ssh-keys":
		var req struct {
			SSHKey string `json:"ssh_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.keysAdded++
		k := sshKey{ID: "key-" + strconv.Itoa(c.keysAdded), SSHKey: req.SSHKey}
		c.keys = append(c.keys, k)
		writeJSONBody(w, http.StatusCreated, map[string]sshKey{"ssh_key": k})
	case r.Method == http.MethodPost && r.URL.Path == "/v2/instances":
		var req createInstance
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.created = append(c.created, req)
		c.addInstance(instance{ID: "i-" + strconv.Itoa(len(c.created)), Label: req.Label, Tags: req.Tags}, req.UserData)
		writeJSONBody(w, http.StatusAccepted, map[string]instance{"instance": c.instances[len(c.instances)-1]})
	case r.Method == http.MethodOptions && r.URL.Path == "/v2/registry/reg-1/docker-credentials":
		c.credentialQueries = append(c.credentialQueries, r.URL.RawQuery)
		user := "pull-only"
		if r.URL.Query().Get("read_write") == "true" {
			user = "read-write"
		}
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":scoped-" + user))
		writeJSONBody(w, http.StatusOK, map[string]any{"auths": map[string]any{"ewr.vultrcr.com": map[string]string{"auth": auth}}})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/instances":
		label := r.URL.Query().Get("label")
		list := slices.DeleteFunc(slices.Clone(c.instances), func(i instance) bool { return label != "" && i.Label != label })
		writePage(w, r, "instances", list)
	case r.Method == http.MethodGet && hasID:
		i := slices.IndexFunc(c.instances, func(i instance) bool { return i.ID == id })
		if i < 0 {
			writeJSONBody(w, http.StatusNotFound, map[string]any{"error": "Invalid instance ID.", "status": 404})
			return
		}
		if polls, deleting := c.deletePoll[id]; deleting {
			if c.deletePoll[id] = polls - 1; polls <= 1 {
				c.instances = slices.Delete(c.instances, i, i+1)
			}
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "Server is pending destruction.", "status": 400})
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]instance{"instance": c.instances[i]})
	case r.Method == http.MethodDelete && hasID:
		if _, deleting := c.deletePoll[id]; deleting {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "Server is pending destruction.", "status": 400})
			return
		}
		c.deletePoll[id] = 3
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONBody(w, http.StatusNotFound, map[string]any{"error": "Not found.", "status": 404})
	}
}

// addInstance adds a running instance whose host key comes from its user
// data, when it has some. Its private address is 127.0.0.1, where the SSH
// server listens; nothing listens on its main address.
func (c *fakeCloud) addInstance(i instance, userData string) {
	i.Status, i.PowerStatus, i.MainIP, i.InternalIP = "active", "running", "192.0.2.10", "127.0.0.1"
	c.instances = append(c.instances, i)
	if userData != "" {
		c.hostKeys[i.Label] = hostKeyFromUserData(c.t, userData)
	}
}

// writePage answers with a page of two items and a cursor to the next.
func writePage[T any](w http.ResponseWriter, r *http.Request, key string, items []T) {
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := min(start+2, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	page := items[start:end]
	if page == nil {
		page = []T{}
	}
	writeJSONBody(w, http.StatusOK, map[string]any{key: page, "meta": map[string]any{"total": len(items), "links": map[string]string{"next": next, "prev": ""}}})
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func hostKeyFromUserData(t *testing.T, encoded string) ssh.Signer {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	_, after, _ := strings.Cut(string(raw), "ed25519_private: |\n")
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
// Every instance shares this one server, so it presents the host keys of all
// instances created so far.
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
		config.AddHostKey(c.hostKeys[req.Label])
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

func newProvider(t *testing.T, configure func(*Config)) (*Provider, *fakeCloud) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, _ := ssh.NewSignerFromKey(key)
	cloud := newFakeCloud(t, signer.PublicKey())
	server := httptest.NewServer(cloud)
	t.Cleanup(server.Close)
	cfg := Config{
		APIURL: server.URL, APIKey: "vultr-key", Region: "fra", Plan: "vc2-2c-4gb", OSID: 2284, Tag: "stagely-stagelet",
		FirewallGroupID: "fw-stagelets", VPCID: "vpc-1", Network: testNetworkAccess, RegistryID: "reg-1", HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
	}
	if configure != nil {
		configure(&cfg)
	}
	p, err := New(cfg, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func TestCreateMakesAnInstanceTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()

	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	if req.Region != "fra" || req.Plan != "vc2-2c-4gb" || req.OSID != 2284 || req.SnapshotID != "" || req.Label != "stagely-a1" || req.Hostname != "stagely-a1" {
		t.Fatalf("create request = %+v", req)
	}
	if !slices.Equal(req.Tags, []string{"stagely-stagelet"}) || req.FirewallGroupID != "fw-stagelets" || len(req.SSHKeyIDs) != 1 || !slices.Equal(req.AttachVPC, []string{"vpc-1"}) {
		t.Fatalf("create request = %+v", req)
	}
	userData, _ := base64.StdEncoding.DecodeString(req.UserData)
	if !strings.Contains(string(userData), "docker-compose-v2") || !strings.Contains(string(userData), "DOCKER-USER -d 169.254.169.254") {
		t.Fatalf("user data does not install Docker Compose or keep containers away from the metadata service:\n%s", userData)
	}
	for _, rule := range []string{"STAGELY-PRIVATE -s 10.42.0.2 -j RETURN", "STAGELY-PRIVATE -s 10.42.0.3 -j RETURN", "DOCKER-USER -s 10.42.0.0/20 -m conntrack --ctstate NEW -j STAGELY-PRIVATE"} {
		if !strings.Contains(string(userData), rule) {
			t.Fatalf("user data lacks %q, so the VPC's other instances could connect to it:\n%s", rule, userData)
		}
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "stagely-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
}

func TestRunRefusesAnInstanceWithAnotherHostKey(t *testing.T) {
	p, _ := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherPublic, _ := ssh.NewPublicKey(other.Public())
	if err := os.WriteFile(filepath.Join(p.cfg.HostKeyDir, "stagely-a1"+".pub"), ssh.MarshalAuthorizedKey(otherPublic), 0o600); err != nil {
		t.Fatalf("pin: %v", err)
	}

	if err := p.Run(ctx, "stagely-a1", nil, nil, nil, "true"); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("Run err = %v, want a failed handshake", err)
	}
}

func TestCopyInStreamsTheFile(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	local := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(local, []byte("tarball"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.CopyIn(ctx, "stagely-a1", local, "/tmp/source.tar.gz"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	last := len(cloud.commands) - 1
	if cloud.commands[last] != `'sh' '-c' 'cat > "$1"' 'sh' '/tmp/source.tar.gz'` || cloud.stdin[last] != "tarball" {
		t.Fatalf("command = %s with stdin %q", cloud.commands[last], cloud.stdin[last])
	}
}

func TestAddressIsTheInstancesPrivateAddress(t *testing.T) {
	p, _ := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	addr, err := p.Address(ctx, "stagely-a1")

	if err != nil || addr != "127.0.0.1" {
		t.Fatalf("Address = %q, %v; want the address in the VPC", addr, err)
	}
}

func TestNewRefusesToRunWithoutTheVPC(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for name, change := range map[string]func(*Config){
		"no VPC":             func(c *Config) { c.VPCID = "" },
		"no range":           func(c *Config) { c.Network.Range = netip.Prefix{} },
		"nobody may connect": func(c *Config) { c.Network.Sources = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{APIKey: "k", Region: "fra", Plan: "vc2-2c-4gb", OSID: 2284, Tag: "t", VPCID: "vpc-1", Network: testNetworkAccess, HostKeyDir: t.TempDir()}
			change(&cfg)

			if _, err := New(cfg, key); err == nil || !strings.Contains(err.Error(), "VPC") {
				t.Fatalf("New = %v, want it refused", err)
			}
		})
	}
}

func TestAgentWaitsForAnAddressWhileTheInstanceIsPending(t *testing.T) {
	p, cloud := newProvider(t, nil)
	cloud.mu.Lock()
	cloud.instances = append(cloud.instances, instance{ID: "i-9", Label: "stagely-a1", Tags: []string{"stagely-stagelet"}, Status: "pending", PowerStatus: "running", MainIP: "0.0.0.0"})
	cloud.mu.Unlock()

	if addr, err := p.Address(context.Background(), "stagely-a1"); err == nil {
		t.Fatalf("Address of a pending instance = %q, want an error", addr)
	}
}

func TestSSHKeyIsAddedOnceAndFoundAgainByItsKeyData(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
	cloud.keys = []sshKey{{ID: "other-1", SSHKey: agentLikeKey(t)}, {ID: "other-2", SSHKey: agentLikeKey(t)}, {ID: "mine", SSHKey: public + " added by hand"}}

	for _, name := range []string{"stagely-a1", "stagely-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if cloud.keysAdded != 0 || cloud.created[1].SSHKeyIDs[0] != "mine" {
		t.Fatalf("keys added = %d, key used = %q; want the existing key from the second page", cloud.keysAdded, cloud.created[1].SSHKeyIDs)
	}
}

func agentLikeKey(t *testing.T) string {
	t.Helper()
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestAgentClaimsOnlyItsTaggedInstancesWithTheName(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()
	cloud.mu.Lock()
	cloud.addInstance(instance{ID: "foreign", Label: "stagely-a1", Tags: []string{"web"}}, "")
	cloud.addInstance(instance{ID: "mine", Label: "stagely-a1", Tags: []string{"web", "stagely-stagelet"}}, "")
	cloud.addInstance(instance{ID: "worker", Label: "stagely-build-1", Tags: []string{"stagely-stagelet"}}, "")
	cloud.mu.Unlock()

	names, err := p.List(ctx)
	if err != nil || !slices.Equal(names, []string{"stagely-a1", "stagely-build-1"}) {
		t.Fatalf("List = %q, %v; want only the tagged instances", names, err)
	}
	if err := p.Delete(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if _, deleted := cloud.deletePoll["foreign"]; deleted {
		t.Fatal("Delete removed an instance without the agent's tag")
	}
}

func TestRegistryCredentialsAreScopedAndExpire(t *testing.T) {
	p, cloud := newProvider(t, nil)

	pushUser, pushPassword, err := p.RegistryCredentials(t.Context(), "ewr.vultrcr.com", true)
	if err != nil {
		t.Fatalf("push credentials: %v", err)
	}
	pullUser, pullPassword, err := p.RegistryCredentials(t.Context(), "ewr.vultrcr.com", false)
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
	if _, _, err := p.RegistryCredentials(t.Context(), "sjc.vultrcr.com", false); err == nil {
		t.Fatal("credentials for another registry host were accepted")
	}
}

func TestDeleteWaitsUntilTheInstanceIsGone(t *testing.T) {
	p, _ := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := p.Delete(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, err := p.Exists(ctx, "stagely-a1"); err != nil || exists {
		t.Fatalf("Exists = %v, %v; want gone", exists, err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, "stagely-a1"+".pub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host key file err = %v, want it removed", err)
	}
}

func TestDeleteOfAnInstanceBeingDestroyedWaitsForIt(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.mu.Lock()
	cloud.deletePoll["i-1"] = 2
	cloud.mu.Unlock()

	if err := p.Delete(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Delete of an instance Vultr is destroying: %v", err)
	}
}

func TestDeleteFailsWhileTheInstanceStillExists(t *testing.T) {
	p, cloud := newProvider(t, nil)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.mu.Lock()
	cloud.deletePoll["i-1"] = 1 << 30
	cloud.mu.Unlock()
	p.cfg.ReadyTimeout = 100 * time.Millisecond

	if err := p.Delete(ctx, "stagely-a1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("Delete err = %v, want still exists", err)
	}
}

func TestDeletingAMissingInstanceSucceeds(t *testing.T) {
	p, _ := newProvider(t, nil)

	if err := p.Delete(context.Background(), "stagely-none"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestNewMachineGetsOnlyItsHostKeyDockerAndFirewallRules(t *testing.T) {
	p, cloud := newProvider(t, nil)

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	userData, _ := base64.StdEncoding.DecodeString(cloud.created[0].UserData)
	if got, want := cloudConfigSections(string(userData)), []string{"ssh_deletekeys", "ssh_genkeytypes", "ssh_keys", "package_update", "packages", "bootcmd"}; !slices.Equal(got, want) {
		t.Fatalf("user data sections = %v, want %v", got, want)
	}
	if !strings.Contains(string(userData), "packages:\n  - docker.io\n  - docker-compose-v2\n  - docker-buildx\nbootcmd:\n") {
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

func TestGoldenImageReplacesTheOperatingSystemAndItsPackages(t *testing.T) {
	p, cloud := newProvider(t, func(c *Config) { c.GoldenImage = "snap-1" })

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := cloud.created[0]
	userData, _ := base64.StdEncoding.DecodeString(req.UserData)
	if req.SnapshotID != "snap-1" || req.OSID != 0 || strings.Contains(string(userData), "packages:") {
		t.Fatalf("create request snapshot %q os %d, user data installs packages = %v; want the golden image and no packages", req.SnapshotID, req.OSID, strings.Contains(string(userData), "packages:"))
	}
}

func TestAPIErrorsCarryVultrsMessage(t *testing.T) {
	p, _ := newProvider(t, func(c *Config) { c.APIKey = "wrong" })

	_, err := p.Exists(context.Background(), "stagely-a1")

	if err == nil || !strings.Contains(err.Error(), "Invalid API token.") {
		t.Fatalf("err = %v, want Vultr's message", err)
	}
}
