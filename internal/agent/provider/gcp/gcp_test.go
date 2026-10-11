package gcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
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
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const testZonePath = "/compute/v1/projects/acme/zones/us-central1-a/"

// fakeCompute is Google's token endpoint, a Compute Engine API and the SSH
// server of every instance it creates. Instances present the host key from
// their user data.
type fakeCompute struct {
	t       *testing.T
	agent   ssh.PublicKey
	sshAddr string

	mu          sync.Mutex
	instances   map[string]instance
	hostKeys    map[string]ssh.Signer
	inserted    []insertInstance
	tokens      int
	assertions  []string
	deletePolls int
	commands    []string
	stdin       []string
	filters     []string
	impersonate []string
}

func newFakeCompute(t *testing.T, agent ssh.PublicKey) *fakeCompute {
	t.Helper()
	c := &fakeCompute{t: t, agent: agent, instances: map[string]instance{}, hostKeys: map[string]ssh.Signer{}}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c.sshAddr = ln.Addr().String()
	go c.serveSSH(ln)
	return c
}

func (c *fakeCompute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		c.tokens++
		c.assertions = append(c.assertions, r.PostForm.Get("assertion"))
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "ya29.fake", "expires_in": 3599, "token_type": "Bearer"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer ya29.fake" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if account, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"); ok && r.Method == http.MethodPost {
		var req struct {
			Scope    []string `json:"scope"`
			Lifetime string   `json:"lifetime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		account = strings.TrimSuffix(account, ":generateAccessToken")
		c.impersonate = append(c.impersonate, account+" "+req.Lifetime)
		writeJSON(w, http.StatusOK, map[string]string{"accessToken": "ya29.token-of-" + account, "expireTime": "2026-10-11T05:00:00Z"})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, testZonePath)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPost && rest == "instances":
		var req insertInstance
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.inserted = append(c.inserted, req)
		inst := instance{Name: req.Name, Status: "RUNNING", Labels: req.Labels}
		inst.NetworkInterfaces = append(inst.NetworkInterfaces, struct {
			NetworkIP string `json:"networkIP"`
		}{"127.0.0.1"})
		c.instances[req.Name] = inst
		c.hostKeys[req.Name] = hostKeyFromUserData(c.t, req.Metadata.Items[0].Value)
		writeJSON(w, http.StatusOK, operation{Name: "operation-insert", Status: "RUNNING"})
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "operations/") && strings.HasSuffix(rest, "/wait"):
		writeJSON(w, http.StatusOK, operation{Name: strings.TrimSuffix(strings.TrimPrefix(rest, "operations/"), "/wait"), Status: "DONE"})
	case r.Method == http.MethodGet && rest == "instances":
		c.filters = append(c.filters, r.URL.Query().Get("filter"))
		var items []instance
		for _, inst := range c.instances {
			if strings.Contains(r.URL.Query().Get("filter"), `"`+inst.Labels[ownerLabel]+`"`) {
				items = append(items, inst)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "instances/"):
		name := strings.TrimPrefix(rest, "instances/")
		inst, ok := c.instances[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if inst.Status == "STOPPING" {
			if c.deletePolls--; c.deletePolls <= 0 {
				delete(c.instances, name)
			}
		}
		writeJSON(w, http.StatusOK, inst)
	case r.Method == http.MethodDelete && strings.HasPrefix(rest, "instances/"):
		name := strings.TrimPrefix(rest, "instances/")
		inst := c.instances[name]
		inst.Status = "STOPPING"
		c.instances[name] = inst
		writeJSON(w, http.StatusOK, operation{Name: "operation-delete", Status: "RUNNING"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
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

func (c *fakeCompute) serveSSH(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go c.serveConn(conn)
	}
}

// serveConn accepts the agent's key and records each command and its stdin.
// Every instance shares this one server, so it presents the host keys of all
// instances created so far.
func (c *fakeCompute) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), c.agent.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	c.mu.Lock()
	for _, req := range c.inserted {
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
			input, _ := io.ReadAll(ch)
			c.mu.Lock()
			c.commands = append(c.commands, string(req.Payload[4:]))
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

func testKeyFile(t *testing.T, tokenURI string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	data, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme", "private_key_id": "key-1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "stagely@acme.iam.gserviceaccount.com", "token_uri": tokenURI,
	})
	return data
}

func newProvider(t *testing.T, subnetwork string) (*Provider, *fakeCompute) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, _ := ssh.NewSignerFromKey(key)
	cloud := newFakeCompute(t, signer.PublicKey())
	server := httptest.NewServer(cloud)
	t.Cleanup(server.Close)
	p, err := New(Config{
		ComputeURL: server.URL + "/compute/v1/", Credentials: testKeyFile(t, server.URL+"/token"), Zone: "us-central1-a",
		MachineType: "e2-medium", Image: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
		Subnetwork: subnetwork, Tag: "acme-stagelet", HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
		IAMCredentialsURL: server.URL + "/v1/", RegistryPushAccount: "push@acme.iam.gserviceaccount.com", RegistryPullAccount: "pull@acme.iam.gserviceaccount.com",
	}, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func TestCreateMakesAnInstanceTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t, "projects/acme/regions/us-central1/subnetworks/stagely")
	ctx := context.Background()

	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := cloud.inserted[0]
	items := req.Metadata.Items
	if req.MachineType != "zones/us-central1-a/machineTypes/e2-medium" || req.Tags.Items[0] != "acme-stagelet" || req.Labels[ownerLabel] != "acme-stagelet" {
		t.Fatalf("insert request = %+v", req)
	}
	if nic := req.NetworkInterfaces[0]; nic.Subnetwork != "projects/acme/regions/us-central1/subnetworks/stagely" || nic.Network != "" || len(nic.AccessConfigs) != 1 {
		t.Fatalf("network interface = %+v", nic)
	}
	if items[1].Key != "ssh-keys" || !strings.HasPrefix(items[1].Value, "root:ssh-ed25519 ") || items[2].Key != "block-project-ssh-keys" {
		t.Fatalf("metadata = %+v", items)
	}
	if userData := items[0].Value; !strings.Contains(userData, "docker-compose-v2") || !strings.Contains(userData, "set_host_keys = false") {
		t.Fatalf("user data = %s; want Docker Compose and the guest agent kept off the host keys", userData)
	}
	userData := items[0].Value
	reject := strings.Index(userData, "iptables -I DOCKER-USER -d 169.254.169.254 -m conntrack --ctstate NEW -j REJECT")
	dns := strings.Index(userData, "iptables -I DOCKER-USER -d 169.254.169.254 -p udp --dport 53 -j RETURN")
	if reject < 0 || dns < reject {
		t.Fatalf("user data = %s; want containers kept off the metadata server, with DNS inserted above that rule", userData)
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "stagely-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
}

func TestProviderSignsInOnceWithTheKeysJWT(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()

	for _, name := range []string{"stagely-a1", "stagely-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	if cloud.tokens != 1 {
		t.Fatalf("token requests = %d, want 1 while the token is valid", cloud.tokens)
	}
	parts := strings.Split(cloud.assertions[0], ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if len(parts) != 3 || claims["iss"] != "stagely@acme.iam.gserviceaccount.com" || claims["scope"] != cloudPlatformScope || !strings.HasSuffix(claims["aud"].(string), "/token") {
		t.Fatalf("assertion claims = %v", claims)
	}
}

func TestDefaultNetworkWhenNoSubnetworkIsSet(t *testing.T) {
	p, cloud := newProvider(t, "")

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if nic := cloud.inserted[0].NetworkInterfaces[0]; nic.Network != "global/networks/default" || nic.Subnetwork != "" {
		t.Fatalf("network interface = %+v", nic)
	}
}

func TestRunRefusesAnInstanceWithAnotherHostKey(t *testing.T) {
	p, _ := newProvider(t, "")
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

func TestListAndExistsSeeOnlyTheProvidersInstances(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.mu.Lock()
	cloud.instances["stagely-other"] = instance{Name: "stagely-other", Status: "RUNNING", Labels: map[string]string{ownerLabel: "someone-else"}}
	cloud.mu.Unlock()

	names, err := p.List(ctx)

	if err != nil || len(names) != 1 || names[0] != "stagely-a1" {
		t.Fatalf("List = %v, %v; want only the provider's instance", names, err)
	}
	if cloud.filters[0] != `labels.stagely = "acme-stagelet"` {
		t.Fatalf("list filter = %q", cloud.filters[0])
	}
	if exists, err := p.Exists(ctx, "stagely-other"); err != nil || exists {
		t.Fatalf("Exists for another owner's instance = %v, %v; want false", exists, err)
	}
	if err := p.Delete(ctx, "stagely-other"); err != nil {
		t.Fatalf("Delete of another owner's instance: %v", err)
	}
	if _, ok := cloud.instances["stagely-other"]; !ok {
		t.Fatal("Delete removed an instance the provider did not create")
	}
}

func TestDeleteWaitsUntilTheInstanceIsGone(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.deletePolls = 3

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

func TestDeleteFailsWhileTheInstanceStillExists(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.deletePolls = 1 << 30
	p.cfg.ReadyTimeout = 100 * time.Millisecond

	if err := p.Delete(ctx, "stagely-a1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("Delete err = %v, want still exists", err)
	}
}

func TestDeletingAMissingInstanceSucceeds(t *testing.T) {
	p, _ := newProvider(t, "")

	if err := p.Delete(context.Background(), "stagely-none"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestGoldenImageReplacesTheStockImageAndItsPackages(t *testing.T) {
	p, cloud := newProvider(t, "")
	p.cfg.GoldenImage = "stagely-20261001"

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := cloud.inserted[0]
	userData := req.Metadata.Items[0].Value
	if image := req.Disks[0].InitializeParams["sourceImage"]; image != "projects/acme/global/images/stagely-20261001" || strings.Contains(userData, "packages:") {
		t.Fatalf("image = %s, user data installs packages = %v; want the golden image and no packages", image, strings.Contains(userData, "packages:"))
	}
}

func TestRegistryCredentialsComeFromTheRegistryAccounts(t *testing.T) {
	p, cloud := newProvider(t, "")

	pushUser, pushToken, err := p.RegistryCredentials(t.Context(), "us-central1-docker.pkg.dev", true)
	if err != nil {
		t.Fatalf("push credentials: %v", err)
	}
	pullUser, pullToken, err := p.RegistryCredentials(t.Context(), "us-central1-docker.pkg.dev", false)
	if err != nil {
		t.Fatalf("pull credentials: %v", err)
	}

	if pushUser != "oauth2accesstoken" || pushToken != "ya29.token-of-push@acme.iam.gserviceaccount.com" ||
		pullUser != "oauth2accesstoken" || pullToken != "ya29.token-of-pull@acme.iam.gserviceaccount.com" {
		t.Fatalf("push = %s %s, pull = %s %s; want each account's own token", pushUser, pushToken, pullUser, pullToken)
	}
	want := []string{"push@acme.iam.gserviceaccount.com 3600s", "pull@acme.iam.gserviceaccount.com 3600s"}
	if !slices.Equal(cloud.impersonate, want) {
		t.Fatalf("impersonation requests = %v, want %v", cloud.impersonate, want)
	}
	p.cfg.RegistryPullAccount = ""
	if _, _, err := p.RegistryCredentials(t.Context(), "us-central1-docker.pkg.dev", false); err == nil {
		t.Fatal("credentials without a registry account were handed out")
	}
}

func TestCredentialsMayBeBase64(t *testing.T) {
	key := testKeyFile(t, "https://oauth2.googleapis.com/token")

	parsed, err := parseKey([]byte(base64.StdEncoding.EncodeToString(key)))

	if err != nil || parsed.ClientEmail != "stagely@acme.iam.gserviceaccount.com" || parsed.ProjectID != "acme" {
		t.Fatalf("parseKey(base64) = %+v, %v", parsed, err)
	}
	if _, err := parseKey([]byte(`{"type":"authorized_user"}`)); err == nil {
		t.Fatal("parseKey accepted credentials that are not a service account key")
	}
}
