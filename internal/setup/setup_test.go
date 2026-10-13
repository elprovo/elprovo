package setup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeAPI is the part of the control plane API that setup uses.
type fakeAPI struct {
	mu           sync.Mutex
	environments []Environment
	clients      []Client
	removed      []string
	created      []map[string]any
}

func (f *fakeAPI) Do(_ context.Context, method, path string, body, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		data, _ := json.Marshal(v)
		return json.Unmarshal(data, out)
	}
	switch {
	case method == http.MethodGet && path == "/me":
		return reply(map[string]any{"accounts": []map[string]string{{"id": "acct1"}}})
	case method == http.MethodGet && path == "/accounts/acct1/environments":
		return reply(map[string]any{"environments": f.environments})
	case method == http.MethodPost && path == "/accounts/acct1/environments":
		b := body.(map[string]any)
		f.created = append(f.created, b)
		env := Environment{ID: "env1", Name: b["name"].(string), Provider: b["provider"].(string), PreviewDomain: b["preview_domain"].(string), PreviewPort: b["preview_port"].(int)}
		f.environments = append(f.environments, env)
		return reply(env)
	case method == http.MethodPost && path == "/accounts/acct1/environments/env1/enrollment-tokens":
		return reply(map[string]string{"token": "enroll-1"})
	case method == http.MethodGet && path == "/accounts/acct1/environments/env1":
		return reply(map[string]any{"clients": f.clients})
	case method == http.MethodDelete && strings.HasPrefix(path, "/accounts/acct1/environments/env1/clients/"):
		f.removed = append(f.removed, strings.TrimPrefix(path, "/accounts/acct1/environments/env1/clients/"))
		return nil
	}
	return fmt.Errorf("unexpected %s %s", method, path)
}

func TestEnsureEnvironmentCreatesItOnce(t *testing.T) {
	api := &fakeAPI{}
	ctx := context.Background()
	want := Environment{Name: "Production", Provider: "digitalocean", PreviewDomain: "previews.acme.dev", PreviewPort: 443}

	first, err := ensureEnvironment(ctx, api, "acct1", want)
	if err != nil {
		t.Fatalf("first ensureEnvironment: %v", err)
	}
	second, err := ensureEnvironment(ctx, api, "acct1", Environment{Name: "production", Provider: "digitalocean", PreviewDomain: "previews.acme.dev", PreviewPort: 443})
	if err != nil {
		t.Fatalf("second ensureEnvironment: %v", err)
	}
	if first.ID != "env1" || second.ID != "env1" || len(api.created) != 1 {
		t.Fatalf("environments = %v then %v, created %d times; want one environment", first, second, len(api.created))
	}
	if _, err := ensureEnvironment(ctx, api, "acct1", Environment{Name: "Production", Provider: "aws", PreviewDomain: "previews.acme.dev", PreviewPort: 443}); err == nil {
		t.Fatal("an environment with another provider was accepted")
	}
	if _, err := ensureEnvironment(ctx, api, "acct1", Environment{Name: "Production", Provider: "digitalocean", PreviewDomain: "previews.acme.dev", PreviewPort: 8443}); err == nil {
		t.Fatal("an environment that serves previews on another port was accepted")
	}
}

func TestWaitConnectedPicksTheClientThatConnectedAfterTheInstall(t *testing.T) {
	before := time.Now().Add(-time.Minute)
	stale := []Client{
		{ID: "stale-agent", Role: "agent", Connected: true, LastSeenAt: before},
		{ID: "restarted-agent", Role: "agent", Connected: true, LastSeenAt: before},
	}
	api := &fakeAPI{clients: []Client{
		stale[0],
		{ID: "restarted-agent", Role: "agent", Connected: true, LastSeenAt: before.Add(30 * time.Second)},
		{ID: "gateway", Role: "gateway", Connected: true, LastSeenAt: before.Add(time.Minute)},
	}}

	got, err := waitConnected(context.Background(), api, "acct1", "env1", "agent", stale, time.Millisecond)

	if err != nil || got != "restarted-agent" {
		t.Fatalf("waitConnected = %q, %v; want the agent seen again after the install", got, err)
	}
}

func TestWaitConnectedPrefersANewlyEnrolledClient(t *testing.T) {
	seen := time.Now().Add(-time.Minute)
	before := []Client{{ID: "old-agent", Role: "agent", Connected: true, LastSeenAt: seen}}
	api := &fakeAPI{clients: []Client{
		{ID: "old-agent", Role: "agent", Connected: true, LastSeenAt: seen.Add(30 * time.Second)},
		{ID: "new-agent", Role: "agent", Connected: true, LastSeenAt: seen.Add(40 * time.Second)},
	}}

	got, err := waitConnected(context.Background(), api, "acct1", "env1", "agent", before, time.Millisecond)

	if err != nil || got != "new-agent" {
		t.Fatalf("waitConnected = %q, %v; want the client the install enrolled, not an old one that is still running", got, err)
	}
}

func TestWaitConnectedIgnoresClientsThatDidNotReconnect(t *testing.T) {
	seen := time.Now().Add(-time.Minute)
	stale := []Client{{ID: "stale-agent", Role: "agent", Connected: true, LastSeenAt: seen}}
	api := &fakeAPI{clients: stale}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := waitConnected(ctx, api, "acct1", "env1", "agent", stale, time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitConnected = %v; want it to keep waiting until the deadline", err)
	}
}

func TestKeepOneRemovesEveryOtherClientWithTheRole(t *testing.T) {
	api := &fakeAPI{clients: []Client{
		{ID: "old-agent", Role: "agent", Connected: false},
		{ID: "other-agent", Role: "agent", Connected: true},
		{ID: "new-agent", Role: "agent", Connected: true},
		{ID: "gateway", Role: "gateway", Connected: true},
	}}

	if err := keepOne(context.Background(), api, "acct1", "env1", "agent", "new-agent"); err != nil {
		t.Fatalf("keepOne: %v", err)
	}
	if !reflect.DeepEqual(api.removed, []string{"old-agent", "other-agent"}) {
		t.Fatalf("removed = %v, want every agent but the kept one", api.removed)
	}
}

func TestEnrollmentForReenrollsAKeyTheEnvironmentDoesNotKnow(t *testing.T) {
	tests := []struct {
		name    string
		hasKey  bool
		clients []Client
		want    string
	}{
		{"no key", false, []Client{{ID: "gw", Role: "gateway"}}, "enroll-1"},
		{"key the environment knows", true, []Client{{ID: "gw", Role: "gateway"}}, ""},
		{"key from a deleted environment", true, []Client{{ID: "agent", Role: "agent"}}, "enroll-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{clients: tt.clients}

			got, err := enrollmentFor(context.Background(), api, "acct1", "env1", "gateway", tt.hasKey)

			if err != nil || got != tt.want {
				t.Fatalf("enrollmentFor = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestHostsOnAPrivateNetworkRejectEveryNewConnectionFromIt(t *testing.T) {
	dir := t.TempDir()

	onNetwork, err := Run{WorkDir: dir, Cloud: Cloud{PrivateNetwork: netip.MustParsePrefix("10.42.0.0/20")}}.keys()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	for host, data := range map[string]string{"agent": onNetwork.agentData, "gateway": onNetwork.gatewayData} {
		if !strings.Contains(data, "INPUT -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE") || strings.Contains(data, "RETURN") {
			t.Fatalf("%s user data does not reject every new connection from the network:\n%s", host, data)
		}
	}
	plain, err := Run{WorkDir: dir}.keys()
	if err != nil || strings.Contains(plain.agentData, "bootcmd") || string(onNetwork.agentHost.Marshal()) != string(plain.agentHost.Marshal()) {
		t.Fatalf("keys without a private network = %v, %q; want no rules and the same host key", err, plain.agentData)
	}
}

func TestUnitRunsAsItsOwnUserWithoutSecretsInIt(t *testing.T) {
	s := service{name: "elprovo-gateway", args: []string{"-listen", ":443", "-acme"}, env: map[string]string{"TOKEN": `se"cret`}, bindLowPorts: true}

	unit, err := s.unit()
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	for _, want := range []string{
		"User=elprovo-gateway\n", "ExecStart=/usr/local/bin/elprovo-gateway run -listen :443 -acme\n", "StateDirectory=elprovo-gateway\n",
		"EnvironmentFile=/etc/elprovo/elprovo-gateway.env\n", "AmbientCapabilities=CAP_NET_BIND_SERVICE\n", "ProtectSystem=strict\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "secret") {
		t.Fatal("the unit holds a secret value")
	}
	if got := s.envFile(); got != "TOKEN=\"se\\\"cret\"\n" {
		t.Fatalf("env file = %q", got)
	}
	if _, err := (service{name: "x", args: []string{"has space"}}).unit(); err == nil {
		t.Fatal("an argument with a space was accepted")
	}
}

func TestCloudSetupGivesTheCloudTokenToTheAgentHostAndNeverToTheGatewayHost(t *testing.T) {
	const token = "dop_v1_4c1d0e5f00dfeed"
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	agentListener, gatewayListener, sshPort := listenOnBothLoopbacks(t)
	api := &fakeAPI{}
	connect := func(role string) func() {
		return func() {
			api.mu.Lock()
			defer api.mu.Unlock()
			api.clients = append(api.clients, Client{ID: role + "-1", Role: role, EnrolledAt: time.Now(), Connected: true, LastSeenAt: time.Now()})
		}
	}
	agentHost := serveSSHHost(t, agentListener, writeHostKey(t, filepath.Join(work, "agent_host_ed25519")), connect("agent"))
	gatewayHost := serveSSHHost(t, gatewayListener, writeHostKey(t, filepath.Join(work, "gateway_host_ed25519")), connect("gateway"))
	outputs := `{"agent_address":{"value":"127.0.0.1"},"gateway_address":{"value":"::1"},"gateway_ip":{"value":"203.0.113.7"},` +
		`"vpc_id":{"value":"vpc-1"},"preview_tag":{"value":"elprovo-preview"},"registry":{"value":"registry.digitalocean.com/acme"}}`
	files := map[string]string{
		filepath.Join(dir, "tofu"):                   "#!/bin/sh\nif [ \"$1\" = output ]; then printf '" + outputs + "'; fi\n",
		filepath.Join(dir, "bin", "elprovo-agent"):   "agent binary",
		filepath.Join(dir, "bin", "elprovo-gateway"): "gateway binary",
	}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	do := DigitalOcean{Token: token, APIEndpoint: "https://api.digitalocean.com", Region: "fra1", RegistryName: "acme"}
	cloud, err := do.Cloud(fstest.MapFS{"main.tf": {Data: []byte("#\n")}}, "previews.acme.dev")
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	run := Run{
		API: api, Server: "https://elprovo.example.com", Environment: "Production", PreviewDomain: "previews.acme.dev",
		OperatorCIDRs: []string{"198.51.100.0/24"}, Binaries: filepath.Join(dir, "bin"), Tofu: filepath.Join(dir, "tofu"), WorkDir: work,
		SSHPort: sshPort, ReadyTimeout: 10 * time.Second, Out: io.Discard, Cloud: cloud,
	}

	if _, err := run.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if toAgent := agentHost.received(); !strings.Contains(toAgent, `DIGITALOCEAN_TOKEN="`+token+`"`) {
		t.Fatalf("the agent host did not get the token in its environment file:\n%s", toAgent)
	}
	toGateway := gatewayHost.received()
	if !strings.Contains(toGateway, "ExecStart=/usr/local/bin/elprovo-gateway run") {
		t.Fatalf("the gateway host did not get its service:\n%s", toGateway)
	}
	if strings.Contains(toGateway, token) {
		t.Fatalf("the gateway host got the cloud token:\n%s", toGateway)
	}
}

func listenOnBothLoopbacks(t *testing.T) (ipv4, ipv6 net.Listener, port string) {
	t.Helper()
	ipv4, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on the IPv4 loopback: %v", err)
	}
	t.Cleanup(func() { _ = ipv4.Close() })
	_, port, _ = net.SplitHostPort(ipv4.Addr().String())
	ipv6, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("::1", port))
	if err != nil {
		t.Skipf("setup reaches both hosts on one SSH port, and port %s is not free on the IPv6 loopback: %v", port, err)
	}
	t.Cleanup(func() { _ = ipv6.Close() })
	return ipv4, ipv6, port
}

func writeHostKey(t *testing.T, path string) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatalf("encode host key: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}
	return signer
}

type sshHost struct {
	hostKey ssh.Signer
	started func()

	mu       sync.Mutex
	commands []string
}

func serveSSHHost(t *testing.T, ln net.Listener, hostKey ssh.Signer, started func()) *sshHost {
	t.Helper()
	h := &sshHost{hostKey: hostKey, started: started}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serveConn(conn)
		}
	}()
	return h
}

func (h *sshHost) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	config.AddHostKey(h.hostKey)
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
			h.mu.Lock()
			h.commands = append(h.commands, command+"\n"+string(input))
			h.mu.Unlock()
			if strings.Contains(command, "systemctl restart") {
				h.started()
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{}))
			_ = ch.Close()
		}
	}
}

func (h *sshHost) received() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.commands, "\n")
}

func TestTofuAppliesTheModuleAndReadsItsOutputs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tofu")
	script := "#!/bin/sh\necho \"$@\" >> calls.log\nif [ \"$1\" = output ]; then printf '{\"vpc_id\":{\"value\":\"vpc-1\"}}'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	module := fstest.MapFS{"digitalocean/main.tf": {Data: []byte("# module\n")}}

	out, err := Tofu{Binary: bin, Dir: work, Out: &strings.Builder{}}.Apply(context.Background(), module, map[string]any{"region": "fra1"})

	if err != nil || out["vpc_id"] != "vpc-1" {
		t.Fatalf("Apply = %v, %v; want the vpc output", out, err)
	}
	calls, _ := os.ReadFile(filepath.Join(work, "calls.log"))
	if string(calls) != "init -input=false -no-color\napply -auto-approve -input=false -no-color\noutput -json\n" {
		t.Fatalf("tofu calls = %q", calls)
	}
	vars, _ := os.ReadFile(filepath.Join(work, "terraform.tfvars.json"))
	if !strings.Contains(string(vars), `"region": "fra1"`) {
		t.Fatalf("variables = %s", vars)
	}
	if info, _ := os.Stat(filepath.Join(work, "terraform.tfvars.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("variables mode = %v, want 0600: they hold host keys", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(work, "main.tf")); err != nil {
		t.Fatalf("module file not written: %v", err)
	}
}

func TestModuleName(t *testing.T) {
	for in, want := range map[string]string{"Production": "production", "Acme Prod 2": "acme-prod-2", "1st": "env-1st", "--x--": "x"} {
		if got := moduleName(in); got != want {
			t.Fatalf("moduleName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimaSetupRunsTheAgentUnderLaunchdAndTheGatewayInAVM(t *testing.T) {
	dir := t.TempDir()
	tofu := filepath.Join(dir, "tofu")
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	outputs := `{"registry":{"value":"host.lima.internal:5050"},"certificate_file":{"value":"` + cert + `"},"key_file":{"value":"` + key + `"}}`
	files := map[string]string{
		tofu: "#!/bin/sh\nif [ \"$1\" = output ]; then printf '" + outputs + "'; fi\n",
		cert: "CERT",
		key:  "KEY",
		filepath.Join(dir, "bin", "elprovo-agent"):     "agent binary",
		filepath.Join(dir, "linux", "elprovo-gateway"): "gateway binary",
	}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	api := &fakeAPI{}
	connect := func(role string) {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.clients = append(api.clients, Client{ID: role + "-1", Role: role, EnrolledAt: time.Now(), Connected: true, LastSeenAt: time.Now()})
	}
	var started, limactl []string
	written := map[string]string{}
	l := Lima{
		API: api, Server: "http://localhost:8080", Environment: "Local", PreviewDomain: "previews.localhost", GatewayPort: 8443,
		HostAddress: "host.lima.internal", RegistryPort: 5050, Binaries: filepath.Join(dir, "bin"), GatewayBinary: filepath.Join(dir, "linux", "elprovo-gateway"),
		Tofu: tofu, WorkDir: filepath.Join(dir, "work"), Module: fstest.MapFS{"main.tf": {Data: []byte("#\n")}},
		LaunchAgents: filepath.Join(dir, "LaunchAgents"), UID: 501, Path: "/usr/bin", ReadyTimeout: time.Second, Out: &strings.Builder{},
		Launchctl: func(_ context.Context, args ...string) error {
			if args[0] == "print" {
				return errors.New("not loaded")
			}
			if args[0] == "bootstrap" {
				started = append(started, filepath.Base(args[2]))
				connect("agent")
			}
			return nil
		},
		Limactl: func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
			if args[0] != "shell" {
				limactl = append(limactl, strings.Join(args, " "))
				return nil, nil
			}
			command := args[len(args)-1]
			if stdin != nil {
				data, _ := io.ReadAll(stdin)
				_, target, _ := strings.Cut(command, "cat > ")
				written[strings.Trim(strings.Fields(target)[0], "'")] = string(data)
			}
			if strings.Contains(command, "systemctl restart") {
				connect("gateway")
			}
			return nil, nil
		},
	}

	env, err := l.Run(context.Background())

	if err != nil || env.ID != "env1" || env.Provider != "lima" {
		t.Fatalf("Run = %+v, %v; want the lima environment", env, err)
	}
	if !reflect.DeepEqual(started, []string{"dev.elprovo.local.agent.plist"}) {
		t.Fatalf("launchd started %v, want only the agent", started)
	}
	plist, _ := os.ReadFile(filepath.Join(dir, "LaunchAgents", "dev.elprovo.local.agent.plist"))
	for _, want := range []string{"<string>-provider</string>", "<string>lima</string>", "<string>-lima-insecure-registry</string>", "<string>host.lima.internal:5050/local</string>", "<key>ELPROVO_ENROLLMENT_TOKEN</key>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("agent plist lacks %s:\n%s", want, plist)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, "LaunchAgents", "dev.elprovo.local.agent.plist")); info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode = %v, want 0600: it holds the enrollment token", info.Mode().Perm())
	}
	if len(limactl) != 3 || !strings.HasPrefix(limactl[1], "create --tty=false --name=elprovo-local-gateway --vm-type=vz --network=lima:user-v2") ||
		limactl[2] != "start --tty=false elprovo-local-gateway" {
		t.Fatalf("limactl calls = %q, want the gateway VM created on the user-v2 network, then started", limactl)
	}
	unit := written["/etc/systemd/system/elprovo-gateway.service"]
	if !strings.Contains(unit, "-control-plane ws://host.lima.internal:8080/v1/connect") || !strings.Contains(unit, "-listen :8443 -tls-cert /etc/elprovo/tls.crt -tls-key /etc/elprovo/tls.key") {
		t.Fatalf("gateway unit does not reach the control plane from the VM or serve the certificate:\n%s", unit)
	}
	if written["/usr/local/bin/elprovo-gateway.new"] != "gateway binary" || written["/etc/elprovo/tls.key"] != "KEY" || !strings.Contains(written["/etc/elprovo/elprovo-gateway.env"], "ELPROVO_ENROLLMENT_TOKEN") {
		t.Fatalf("gateway VM got %v, want the Linux binary, the key and an enrollment token", written)
	}
}

func TestDigitalOceanCloudGivesTheAgentItsFlagsAndCredentials(t *testing.T) {
	do := DigitalOcean{Token: "dop_v1_x", APIEndpoint: "http://sim:9300", Region: "fra1", RegistryName: "acme", CreateRegistry: true, DNSDomain: "acme.dev"}

	cloud, err := do.Cloud(fstest.MapFS{}, "previews.acme.dev")
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	if cloud.Vars["dns_domain"] != "acme.dev" || cloud.Vars["preview_subdomain"] != "previews" || cloud.TofuEnv[0] != "DIGITALOCEAN_TOKEN=dop_v1_x" {
		t.Fatalf("cloud = %+v", cloud)
	}
	args, env, err := cloud.Agent(map[string]any{"vpc_id": "vpc-1", "preview_tag": "elprovo-preview", "registry": "registry.digitalocean.com/acme"})
	want := []string{"-do-api-url", "http://sim:9300", "-do-region", "fra1", "-do-vpc", "vpc-1", "-do-tag", "elprovo-preview", "-registry", "registry.digitalocean.com/acme"}
	if err != nil || !reflect.DeepEqual(args, want) || !reflect.DeepEqual(env, map[string]string{"DIGITALOCEAN_TOKEN": "dop_v1_x"}) {
		t.Fatalf("agent = %v %v %v; want the token only in DIGITALOCEAN_TOKEN, so machines get short-lived registry credentials instead", args, env, err)
	}
	if strings.Contains(strings.Join(args, " "), "dop_v1_x") {
		t.Fatal("the token is in the agent's arguments, which other users can read")
	}
	if _, _, err := cloud.Agent(map[string]any{}); err == nil {
		t.Fatal("missing outputs were accepted")
	}
	do.GoldenImage = "1234"
	golden, err := do.Cloud(fstest.MapFS{}, "previews.acme.dev")
	if err != nil {
		t.Fatalf("Cloud with a golden image: %v", err)
	}
	args, _, err = golden.Agent(map[string]any{"vpc_id": "vpc-1", "preview_tag": "elprovo-preview", "registry": "registry.digitalocean.com/acme"})
	if err != nil || !slices.Equal(args[len(args)-2:], []string{"-do-golden-image", "1234"}) {
		t.Fatalf("agent args with a golden image = %v, %v; want -do-golden-image 1234", args, err)
	}
}

func TestLimaStopWaitsUntilLaunchdDropsTheJob(t *testing.T) {
	prints := 0
	var calls []string
	l := Lima{Launchctl: func(_ context.Context, args ...string) error {
		calls = append(calls, args[0])
		if args[0] == "print" {
			prints++
			if prints > 3 {
				return errors.New("not loaded")
			}
		}
		return nil
	}}

	if err := l.stop(context.Background(), "gui/501/dev.elprovo.local.agent"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"print", "bootout", "print", "print", "print"}) {
		t.Fatalf("launchctl calls = %v, want bootout and then printing until the job is gone", calls)
	}
}
