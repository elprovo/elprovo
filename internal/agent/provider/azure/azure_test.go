package azure

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
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	testTenant   = "tenant-1"
	testSub      = "sub-1"
	testGroup    = "env-stagely"
	testSubnet   = "/subscriptions/sub-1/resourceGroups/env-stagely/providers/Microsoft.Network/virtualNetworks/net/subnets/stagelets"
	testTag      = "env-stagelet"
	testRegistry = "/subscriptions/sub-1/resourceGroups/env-stagely/providers/Microsoft.ContainerRegistry/registries/stagelyacme"
)

type fakeToken struct {
	scopeMap string
	created  time.Time
	expiry   string
}

type fakeVM struct {
	body       map[string]any
	tags       map[string]string
	state      string
	nicID      string
	deletePoll int
}

// fakeCloud is Azure Resource Manager, its metadata document and token
// endpoint, and the SSH server of every machine it creates. Machines present
// the host key from their custom data.
type fakeCloud struct {
	t       *testing.T
	url     string
	agent   ssh.PublicKey
	sshAddr string

	mu             sync.Mutex
	tokens         int
	scopes         []string
	vms            map[string]*fakeVM
	nics           map[string]map[string]string
	hostKeys       map[string]ssh.Signer
	failVM         bool
	pageSize       int
	commands       []string
	stdin          []string
	deletedNICs    []string
	registryTokens map[string]*fakeToken
}

func newFakeCloud(t *testing.T, agent ssh.PublicKey) *fakeCloud {
	t.Helper()
	c := &fakeCloud{
		t: t, agent: agent, vms: map[string]*fakeVM{}, nics: map[string]map[string]string{}, hostKeys: map[string]ssh.Signer{}, registryTokens: map[string]*fakeToken{},
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

func (c *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/metadata/endpoints":
		writeJSON(w, http.StatusOK, map[string]any{"resourceManager": c.url + "/", "authentication": map[string]any{"loginEndpoint": c.url + "/"}})
		return
	case "/" + testTenant + "/oauth2/v2.0/token":
		_ = r.ParseForm()
		if r.PostForm.Get("client_secret") != "secret" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
			return
		}
		c.tokens++
		c.scopes = append(c.scopes, r.PostForm.Get("scope"))
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "token-1", "expires_in": 3599})
		return
	}
	if r.Header.Get("Authorization") != "Bearer token-1" || r.URL.Query().Get("api-version") == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "AuthenticationFailed"}})
		return
	}
	group := "/subscriptions/" + testSub + "/resourceGroups/" + testGroup + "/providers/"
	switch {
	case strings.HasPrefix(r.URL.Path, "/operations/"):
		status := "Succeeded"
		if strings.HasSuffix(r.URL.Path, "/failed") {
			status = "Failed"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "error": map[string]string{"code": "OSProvisioningClientError", "message": "it broke"}})
	case strings.HasPrefix(r.URL.Path, testRegistry):
		c.serveRegistry(w, r)
	case strings.HasPrefix(r.URL.Path, group+"Microsoft.Network/networkInterfaces/"):
		c.serveNIC(w, r)
	case r.URL.Path == group+"Microsoft.Compute/virtualMachines":
		c.listVMs(w, r)
	case strings.HasPrefix(r.URL.Path, group+"Microsoft.Compute/virtualMachines/"):
		c.serveVM(w, r, path.Base(r.URL.Path))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveRegistry is the container registry with its tokens and their
// passwords.
func (c *fakeCloud) serveRegistry(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, testRegistry)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"properties": map[string]string{"loginServer": "stagelyacme.azurecr.io"}})
	case rest == "/tokens" && r.Method == http.MethodGet:
		var list []any
		for name, tok := range c.registryTokens {
			list = append(list, map[string]any{"name": name, "properties": map[string]string{"creationDate": tok.created.Format(time.RFC3339)}})
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": list})
	case strings.HasPrefix(rest, "/tokens/") && r.Method == http.MethodPut:
		var body struct {
			Properties struct {
				ScopeMapID string `json:"scopeMapId"`
			} `json:"properties"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.registryTokens[path.Base(rest)] = &fakeToken{scopeMap: path.Base(body.Properties.ScopeMapID), created: time.Now()}
		w.Header().Set("Azure-AsyncOperation", c.url+"/operations/token?api-version=x")
		writeJSON(w, http.StatusCreated, map[string]any{})
	case strings.HasPrefix(rest, "/tokens/") && r.Method == http.MethodDelete:
		delete(c.registryTokens, path.Base(rest))
		w.WriteHeader(http.StatusAccepted)
	case rest == "/generateCredentials" && r.Method == http.MethodPost:
		var body struct {
			TokenID string `json:"tokenId"`
			Expiry  string `json:"expiry"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tok := c.registryTokens[path.Base(body.TokenID)]
		if tok == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tok.expiry = body.Expiry
		writeJSON(w, http.StatusOK, map[string]any{"username": path.Base(body.TokenID), "passwords": []any{map[string]string{"name": "password1", "value": "token-secret"}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (c *fakeCloud) serveNIC(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Tags map[string]string `json:"tags"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.nics[id] = body.Tags
		w.Header().Set("Azure-AsyncOperation", c.url+"/operations/nic?api-version=x")
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	case http.MethodGet:
		tags, ok := c.nics[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "properties": map[string]any{
			"ipConfigurations": []any{map[string]any{"properties": map[string]string{"privateIPAddress": "127.0.0.1"}}},
		}})
	case http.MethodDelete:
		delete(c.nics, id)
		c.deletedNICs = append(c.deletedNICs, id)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (c *fakeCloud) serveVM(w http.ResponseWriter, r *http.Request, name string) {
	vm := c.vms[name]
	switch r.Method {
	case http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		props := body["properties"].(map[string]any)
		customData, _ := base64.StdEncoding.DecodeString(props["osProfile"].(map[string]any)["customData"].(string))
		nicID := props["networkProfile"].(map[string]any)["networkInterfaces"].([]any)[0].(map[string]any)["id"].(string)
		tags := map[string]string{}
		for k, v := range body["tags"].(map[string]any) {
			tags[k] = v.(string)
		}
		c.vms[name] = &fakeVM{body: body, tags: tags, state: "Succeeded", nicID: nicID}
		c.hostKeys[name] = hostKeyFromUserData(c.t, string(customData))
		op := c.url + "/operations/vm?api-version=x"
		if c.failVM {
			op = c.url + "/operations/failed?api-version=x"
		}
		w.Header().Set("Azure-AsyncOperation", op)
		writeJSON(w, http.StatusCreated, map[string]any{"name": name})
	case http.MethodGet:
		if vm == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if vm.state == "Deleting" {
			if vm.deletePoll--; vm.deletePoll < 0 {
				delete(c.vms, name)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		writeJSON(w, http.StatusOK, c.render(name, vm))
	case http.MethodDelete:
		if vm != nil {
			vm.state = "Deleting"
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (c *fakeCloud) render(name string, vm *fakeVM) map[string]any {
	return map[string]any{
		"name": name, "tags": vm.tags,
		"properties": map[string]any{
			"provisioningState": vm.state,
			"networkProfile":    map[string]any{"networkInterfaces": []any{map[string]string{"id": vm.nicID}}},
		},
	}
}

// listVMs answers pageSize virtual machines a page, with nextLink.
func (c *fakeCloud) listVMs(w http.ResponseWriter, r *http.Request) {
	var all []map[string]any
	for _, name := range slices.Sorted(maps.Keys(c.vms)) {
		all = append(all, c.render(name, c.vms[name]))
	}
	start := 0
	if r.URL.Query().Get("page") == "2" {
		start = c.pageSize
	}
	end := len(all)
	if c.pageSize > 0 && start == 0 {
		end = min(c.pageSize, len(all))
	}
	body := map[string]any{"value": all[start:end]}
	if end < len(all) {
		body["nextLink"] = c.url + r.URL.Path + "?api-version=x&page=2"
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
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
		t.Fatalf("custom data has no usable host key: %v", err)
	}
	return signer
}

func (c *fakeCloud) serveSSH(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go c.serveConn(conn)
	}
}

// serveConn accepts the agent's key and records each command and its stdin.
// Every machine shares this server, so it presents every machine's host key.
func (c *fakeCloud) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "root" || !bytes.Equal(key.Marshal(), c.agent.Marshal()) {
			return nil, errors.New("unknown user or key")
		}
		return nil, nil
	}}
	c.mu.Lock()
	for _, k := range c.hostKeys {
		config.AddHostKey(k)
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

func newProvider(t *testing.T) (*Provider, *fakeCloud) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(key)
	cloud := newFakeCloud(t, signer.PublicKey())
	server := httptest.NewServer(cloud)
	t.Cleanup(server.Close)
	cloud.url = server.URL
	p, err := New(Config{
		ARMURL: server.URL, TenantID: testTenant, ClientID: "client", ClientSecret: "secret", SubscriptionID: testSub, ResourceGroup: testGroup,
		Location: "westeurope", SubnetID: testSubnet, Size: "Standard_B2s", Image: "Canonical:ubuntu-24_04-lts:server:latest", Tag: testTag,
		AdminUsername: "stagely", Registry: testRegistry, HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
	}, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func (c *fakeCloud) vm(name string) *fakeVM {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vms[name]
}

func TestCreateMakesAMachineTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()

	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	vm := cloud.vm("stagely-a1")
	props := vm.body["properties"].(map[string]any)
	os := props["osProfile"].(map[string]any)
	customData, _ := base64.StdEncoding.DecodeString(os["customData"].(string))
	image := props["storageProfile"].(map[string]any)["imageReference"].(map[string]any)
	if vm.tags["stagely"] != testTag || os["adminUsername"] != "stagely" || image["offer"] != "ubuntu-24_04-lts" || props["hardwareProfile"].(map[string]any)["vmSize"] != "Standard_B2s" {
		t.Fatalf("virtual machine request = %+v", vm.body)
	}
	if !strings.Contains(string(customData), "docker-compose-v2") || !strings.HasSuffix(string(customData), "disable_root: false\n") {
		t.Fatalf("custom data does not install Docker Compose or keep root logins:\n%s", customData)
	}
	for _, rule := range []string{"-d 169.254.169.254 -m conntrack", "-d 168.63.129.16 -p tcp ! --dport 53", "-d 168.63.129.16 -p udp ! --dport 53"} {
		if !strings.Contains(string(customData), "iptables -I DOCKER-USER "+rule) {
			t.Fatalf("custom data does not keep containers from %q:\n%s", rule, customData)
		}
	}
	if !strings.HasSuffix(vm.nicID, "/networkInterfaces/stagely-a1-nic") || cloud.nics[vm.nicID][tagKey] != testTag {
		t.Fatalf("network interface %s with tags %v", vm.nicID, cloud.nics[vm.nicID])
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "stagely-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
	if cloud.scopes[0] != cloud.url+"/.default" || cloud.tokens != 1 {
		t.Fatalf("token requests = %d with scopes %v, want one for the metadata document's Resource Manager", cloud.tokens, cloud.scopes)
	}
}

func TestNewMachineGetsOnlyItsHostKeyDockerAndFirewallRules(t *testing.T) {
	p, cloud := newProvider(t)

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	osProfile := cloud.vm("stagely-a1").body["properties"].(map[string]any)["osProfile"].(map[string]any)
	customData, _ := base64.StdEncoding.DecodeString(osProfile["customData"].(string))
	want := []string{"ssh_deletekeys", "ssh_genkeytypes", "ssh_keys", "package_update", "packages", "bootcmd", "disable_root"}
	if got := cloudConfigSections(string(customData)); !slices.Equal(got, want) {
		t.Fatalf("custom data sections = %v, want %v", got, want)
	}
	if !strings.Contains(string(customData), "packages:\n  - docker.io\n  - docker-compose-v2\n  - docker-buildx\nbootcmd:\n") {
		t.Fatalf("custom data installs packages other than Docker:\n%s", customData)
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
	p.cfg.GoldenImage = "/subscriptions/sub-1/resourceGroups/images/providers/Microsoft.Compute/images/stagely"

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	props := cloud.vm("stagely-a1").body["properties"].(map[string]any)
	image := props["storageProfile"].(map[string]any)["imageReference"].(map[string]any)
	customData, _ := base64.StdEncoding.DecodeString(props["osProfile"].(map[string]any)["customData"].(string))
	if image["id"] != p.cfg.GoldenImage || image["offer"] != nil || strings.Contains(string(customData), "packages:") {
		t.Fatalf("image = %v, custom data installs packages = %v; want the golden image and no packages", image, strings.Contains(string(customData), "packages:"))
	}
}

func TestCreateFailsWhenAzureFailsTheVirtualMachine(t *testing.T) {
	p, cloud := newProvider(t)
	cloud.failVM = true

	err := p.Create(context.Background(), "stagely-a1")

	if err == nil || !strings.Contains(err.Error(), "OSProvisioningClientError") {
		t.Fatalf("Create = %v, want the operation's error", err)
	}
}

func TestAddressIsThePrivateAddressOfTheInterface(t *testing.T) {
	p, _ := newProvider(t)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatal(err)
	}

	addr, err := p.Address(ctx, "stagely-a1")

	if err != nil || addr != "127.0.0.1" {
		t.Fatalf("Address = %q, %v", addr, err)
	}
}

func TestListReturnsOnlyTaggedMachinesAcrossPages(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()
	for _, name := range []string{"stagely-a1", "stagely-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	cloud.mu.Lock()
	cloud.vms["other"] = &fakeVM{tags: map[string]string{tagKey: "another-env"}, state: "Succeeded"}
	cloud.pageSize = 1
	cloud.mu.Unlock()

	names, err := p.List(ctx)

	if err != nil || len(names) != 2 || !strings.HasPrefix(names[0], "stagely-a") || !strings.HasPrefix(names[1], "stagely-a") {
		t.Fatalf("List = %v, %v; want the two tagged machines", names, err)
	}
}

func TestDeleteWaitsUntilTheMachineIsGone(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatal(err)
	}
	cloud.vm("stagely-a1").deletePoll = 3

	if err := p.Delete(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if exists, err := p.Exists(ctx, "stagely-a1"); err != nil || exists {
		t.Fatalf("Exists = %v, %v; want gone", exists, err)
	}
	if len(cloud.deletedNICs) != 1 {
		t.Fatalf("deleted interfaces = %v, want the machine's", cloud.deletedNICs)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, "stagely-a1"+".pub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host key file err = %v, want it removed", err)
	}
}

func TestDeleteFailsWhileTheMachineStillExists(t *testing.T) {
	p, cloud := newProvider(t)
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatal(err)
	}
	cloud.vm("stagely-a1").deletePoll = 1 << 30
	p.cfg.ReadyTimeout = 100 * time.Millisecond

	if err := p.Delete(ctx, "stagely-a1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("Delete err = %v, want still exists", err)
	}
}

func TestDeleteLeavesAMachineWithoutTheTag(t *testing.T) {
	p, cloud := newProvider(t)
	cloud.vms["stagely-a1"] = &fakeVM{tags: map[string]string{tagKey: "another-env"}, state: "Succeeded"}

	if err := p.Delete(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if vm := cloud.vm("stagely-a1"); vm == nil || vm.state != "Succeeded" {
		t.Fatal("Delete touched a machine of another environment")
	}
}

func TestDeletingAMissingMachineSucceeds(t *testing.T) {
	p, _ := newProvider(t)

	if err := p.Delete(context.Background(), "stagely-none"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestNewRefusesAnImageThatIsNotAURN(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	_, err := New(Config{
		ARMURL: "https://arm", TenantID: "t", ClientID: "c", ClientSecret: "s", SubscriptionID: "sub", ResourceGroup: "g", Location: "l",
		SubnetID: "subnet", Size: "s", Image: "ubuntu-24-04", Tag: "t", AdminUsername: "stagely", HostKeyDir: t.TempDir(),
	}, key)

	if err == nil || !strings.Contains(err.Error(), "publisher:offer:sku:version") {
		t.Fatalf("New = %v, want the image refused", err)
	}
}

func TestPullCredentialsComeFromANewPullOnlyToken(t *testing.T) {
	p, cloud := newProvider(t)

	user, password, err := p.RegistryCredentials(context.Background(), "stagelyacme.azurecr.io", false)
	if err != nil {
		t.Fatalf("RegistryCredentials: %v", err)
	}
	tok := cloud.registryTokens[user]
	if !strings.HasPrefix(user, jobTokenPrefix) || password != "token-secret" || tok == nil || tok.scopeMap != "_repositories_pull" {
		t.Fatalf("credentials %s/%s from token %+v, want a new pull-only token's password", user, password, tok)
	}
	expiry, err := time.Parse(time.RFC3339, tok.expiry)
	if err != nil || time.Until(expiry) < registryCredentialsExpiry-time.Minute || time.Until(expiry) > registryCredentialsExpiry {
		t.Fatalf("password expiry = %q, want %v from now", tok.expiry, registryCredentialsExpiry)
	}
}

func TestPushCredentialsComeFromAPushToken(t *testing.T) {
	p, cloud := newProvider(t)

	user, _, err := p.RegistryCredentials(context.Background(), "stagelyacme.azurecr.io", true)

	if err != nil || cloud.registryTokens[user].scopeMap != "_repositories_push" {
		t.Fatalf("RegistryCredentials = %s, %v; want a push token", user, err)
	}
}

func TestEachCredentialGetsItsOwnToken(t *testing.T) {
	p, _ := newProvider(t)
	ctx := context.Background()

	first, _, err1 := p.RegistryCredentials(ctx, "stagelyacme.azurecr.io", true)
	second, _, err2 := p.RegistryCredentials(ctx, "stagelyacme.azurecr.io", true)

	if err1 != nil || err2 != nil || first == second {
		t.Fatalf("credentials from tokens %s and %s (%v, %v), want two tokens so neither replaces the other's password", first, second, err1, err2)
	}
}

func TestCredentialsForAnotherRegistryAreRefused(t *testing.T) {
	p, cloud := newProvider(t)

	_, _, err := p.RegistryCredentials(context.Background(), "evil.example.com", true)

	if err == nil || len(cloud.registryTokens) != 0 {
		t.Fatalf("RegistryCredentials = %v with %d tokens, want a refusal and no token", err, len(cloud.registryTokens))
	}
}

func TestExpiredJobTokensAreRemoved(t *testing.T) {
	p, cloud := newProvider(t)
	old := time.Now().Add(-registryCredentialsExpiry - time.Minute)
	cloud.registryTokens[jobTokenPrefix+"old"] = &fakeToken{created: old}
	cloud.registryTokens["operator-token"] = &fakeToken{created: old}
	cloud.registryTokens[jobTokenPrefix+"fresh"] = &fakeToken{created: time.Now()}

	if _, _, err := p.RegistryCredentials(context.Background(), "stagelyacme.azurecr.io", false); err != nil {
		t.Fatal(err)
	}

	if cloud.registryTokens[jobTokenPrefix+"old"] != nil || cloud.registryTokens["operator-token"] == nil || cloud.registryTokens[jobTokenPrefix+"fresh"] == nil {
		t.Fatalf("tokens left = %v, want only the expired job token gone", slices.Collect(maps.Keys(cloud.registryTokens)))
	}
}

func TestCreateRefusesToTakeOverAResourceWithoutTheTag(t *testing.T) {
	p, cloud := newProvider(t)
	nic := p.nicPath("stagely-a1")
	cloud.nics[nic] = map[string]string{"owner": "operator"}

	err := p.Create(context.Background(), "stagely-a1")

	if err == nil || !strings.Contains(err.Error(), "does not take it over") {
		t.Fatalf("Create = %v, want a refusal", err)
	}
	if cloud.nics[nic]["owner"] != "operator" || cloud.vm("stagely-a1") != nil {
		t.Fatal("Create changed the operator's network interface or made a virtual machine")
	}
}

func TestTokenGoesOnlyToALoginEndpointOverHTTPSOrTheResourceManager(t *testing.T) {
	a := newAPI("https://arm.example", "tenant", "client", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"resourceManager": "https://arm.example/", "authentication": map[string]string{"loginEndpoint": "http://login.example/"}})
	}))
	defer server.Close()
	a.armURL = server.URL

	_, err := a.accessToken(context.Background())

	if err == nil || !strings.Contains(err.Error(), "in the clear") {
		t.Fatalf("accessToken = %v, want the plain HTTP login endpoint on another host refused", err)
	}
}

func TestResourcePathsCannotChangeTheHost(t *testing.T) {
	p, _ := newProvider(t)

	_, err := p.api.do(context.Background(), http.MethodGet, "@evil.example/x", computeAPIVersion, nil, nil)

	if err == nil || !strings.Contains(err.Error(), "must start with a single /") {
		t.Fatalf("do = %v, want the path refused", err)
	}
}
