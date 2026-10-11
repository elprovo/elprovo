//go:build simdocker

package gcp

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
// stagely-sim's Google Cloud simulation. As setup does, it makes a custom
// mode network whose only firewall rule lets the agent, here the test, reach
// machines with the stagelet tag, and registry service accounts that may
// only push or only pull and that the agent's account may impersonate. It
// creates a stagelet and a neighbor with the tag, waits until cloud-init and
// Docker Compose are ready, runs a Compose project on the stagelet and
// reaches it on the internal address, checks that the neighbor can't reach
// it and that a container can't reach the metadata server, pushes and pulls
// an image with the registry accounts' tokens, and deletes both instances.
//
// STAGELY_SIM_GCP_ENDPOINT names the simulation's base URL and
// STAGELY_SIM_GCP_CREDENTIALS a service account key file whose token_uri
// reaches it; STAGELY_SIM_GCP_REGISTRY names an image prefix,
// <host>/<project>/<repository>, in a registry that uses the simulation as
// its token service. The test must run where instance addresses are
// reachable. With STAGELY_SIM_GCP_GOLDEN_IMAGE, the instances boot from
// that image. "sim/scripts/check-gcp.sh agent" and "image" set this up.
func TestProviderAgainstSimulation(t *testing.T) {
	endpoint, credentialsFile, registry := os.Getenv("STAGELY_SIM_GCP_ENDPOINT"), os.Getenv("STAGELY_SIM_GCP_CREDENTIALS"), os.Getenv("STAGELY_SIM_GCP_REGISTRY")
	if endpoint == "" || credentialsFile == "" || registry == "" {
		t.Fatal("STAGELY_SIM_GCP_ENDPOINT, STAGELY_SIM_GCP_CREDENTIALS and STAGELY_SIM_GCP_REGISTRY must name a running simulation; run sim/scripts/check-gcp.sh agent")
	}
	credentials, err := os.ReadFile(credentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := parseKey(credentials)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	suffix := randomHex(t)
	p, err := New(Config{
		ComputeURL: endpoint + "compute/v1/", Credentials: credentials, Zone: "us-central1-a", MachineType: "e2-medium",
		Image: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64", GoldenImage: os.Getenv("STAGELY_SIM_GCP_GOLDEN_IMAGE"),
		Subnetwork: "projects/" + sa.ProjectID + "/regions/us-central1/subnetworks/check-" + suffix, Tag: "stagely-sim-agent-check",
		IAMCredentialsURL:   endpoint + "v1/",
		RegistryPushAccount: "push-" + suffix + "@" + sa.ProjectID + ".iam.gserviceaccount.com",
		RegistryPullAccount: "pull-" + suffix + "@" + sa.ProjectID + ".iam.gserviceaccount.com",
		HostKeyDir:          t.TempDir(), ReadyTimeout: 10 * time.Minute,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	env := checkEnvironment{t: t, p: p, endpoint: endpoint, suffix: suffix, project: sa.ProjectID, owner: sa.ClientEmail}
	env.setUp(registry)
	stagelet, neighbor := "stagely-check-"+suffix, "stagely-neighbor-"+suffix
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, stagelet)
		_ = p.Delete(ctx, neighbor)
	})

	for _, name := range []string{stagelet, neighbor} {
		if err := p.Create(t.Context(), name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	addr, err := p.Address(t.Context(), stagelet)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	runComposeProject(t, p, stagelet)
	if body := get(t, "http://"+net.JoinHostPort(addr, "8080")+"/"); !strings.Contains(body, "hello from the stagelet") {
		t.Fatalf("service on the internal address answered %q", body)
	}
	registryHost, _, _ := strings.Cut(registry, "/")
	connect := func(host, port string) error {
		_, err := p.machines.Output(t.Context(), neighbor, nil, "timeout", "3", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, port)
		return err
	}
	if err := connect(registryHost, "443"); err != nil {
		t.Fatalf("the neighbor can't connect out, so its blocked connection would prove nothing: %v", err)
	}
	if err := connect(addr, "8080"); err == nil {
		t.Fatal("the neighbor reached the stagelet's published port through the firewall")
	}
	checkMetadataServer(t, p, stagelet)
	pushAndPull(t, p, stagelet, registry)
	names, err := p.List(t.Context())
	if err != nil || len(names) != 2 {
		t.Fatalf("List = %v, %v; want the stagelet and the neighbor", names, err)
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

// checkEnvironment makes, through the simulation's APIs and with the
// agent's own key, what setup's module makes for the agent.
type checkEnvironment struct {
	t        *testing.T
	p        *Provider
	endpoint string
	suffix   string
	project  string
	owner    string
}

func (e checkEnvironment) call(method, path string, body any) {
	e.t.Helper()
	var op operation
	if err := e.p.api.doURL(e.t.Context(), method, e.endpoint+path, body, &op); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	if err := op.failure(); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
}

func (e checkEnvironment) setUp(registry string) {
	e.t.Helper()
	compute := "compute/v1/projects/" + e.project + "/"
	name := "check-" + e.suffix
	repository := registry[strings.LastIndex(registry, "/")+1:]
	repositoryPath := "v1/projects/" + e.project + "/locations/us-central1/repositories/" + repository
	e.call(http.MethodPost, compute+"global/networks", map[string]any{"name": name, "autoCreateSubnetworks": false})
	e.call(http.MethodPost, compute+"regions/us-central1/subnetworks", map[string]any{
		"name": name, "network": "global/networks/" + name, "ipCidrRange": "10.42.0.0/24",
	})
	e.call(http.MethodPost, compute+"global/firewalls", map[string]any{
		"name": name + "-agent", "network": "global/networks/" + name, "sourceRanges": ownAddresses(e.t),
		"allowed": []map[string]any{{"IPProtocol": "tcp"}}, "targetTags": []string{e.p.cfg.Tag},
	})
	e.call(http.MethodPost, "v1/projects/"+e.project+"/locations/us-central1/repositories?repositoryId="+repository, map[string]any{"format": "DOCKER"})
	var bindings []map[string]any
	for _, account := range []struct{ id, role string }{{"push-" + e.suffix, "roles/artifactregistry.writer"}, {"pull-" + e.suffix, "roles/artifactregistry.reader"}} {
		e.call(http.MethodPost, "v1/projects/"+e.project+"/serviceAccounts", map[string]any{"accountId": account.id})
		email := account.id + "@" + e.project + ".iam.gserviceaccount.com"
		e.call(http.MethodPost, "v1/projects/"+e.project+"/serviceAccounts/"+email+":setIamPolicy", map[string]any{"policy": map[string]any{
			"bindings": []map[string]any{{"role": "roles/iam.serviceAccountTokenCreator", "members": []string{"serviceAccount:" + e.owner}}},
		}})
		bindings = append(bindings, map[string]any{"role": account.role, "members": []string{"serviceAccount:" + email}})
	}
	e.call(http.MethodPost, repositoryPath+":setIamPolicy", map[string]any{"policy": map[string]any{"bindings": bindings}})
	e.t.Cleanup(func() {
		for _, path := range []string{
			"v1/projects/" + e.project + "/serviceAccounts/push-" + e.suffix + "@" + e.project + ".iam.gserviceaccount.com",
			"v1/projects/" + e.project + "/serviceAccounts/pull-" + e.suffix + "@" + e.project + ".iam.gserviceaccount.com",
			repositoryPath, compute + "global/firewalls/" + name + "-agent", compute + "regions/us-central1/subnetworks/" + name,
			compute + "global/networks/" + name,
		} {
			_ = e.p.api.doURL(context.Background(), http.MethodDelete, e.endpoint+path, nil, nil)
		}
	})
}

// ownAddresses returns this host's IPv4 addresses, as /32 source ranges.
func ownAddresses(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var ranges []string
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			ranges = append(ranges, ip.IP.String()+"/32")
		}
	}
	if len(ranges) == 0 {
		t.Fatal("this host has no IPv4 address")
	}
	return ranges
}

// checkMetadataServer checks that the machine resolves the metadata server's
// name to its address and that containers can't connect to it.
func checkMetadataServer(t *testing.T, p *Provider, name string) {
	t.Helper()
	hosts, err := p.machines.Output(t.Context(), name, nil, "getent", "hosts", "metadata.google.internal")
	if err != nil || !strings.HasPrefix(string(hosts), "169.254.169.254 ") {
		t.Fatalf("metadata.google.internal on the machine = %q, %v; want 169.254.169.254", hosts, err)
	}
	metadata, err := p.machines.Output(t.Context(), name, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-",
		"http://169.254.169.254/computeMetadata/v1/instance/attributes/user-data")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the metadata server: %q, %v; want the connection refused, so containers can't read the user data", metadata, err)
	}
}

// pushAndPull signs the machine in as the agent's runner does: with the push
// account's token it pushes an image, and with the pull account's token it
// pulls the image and is refused a push.
func pushAndPull(t *testing.T, p *Provider, name, registry string) {
	t.Helper()
	host, _, _ := strings.Cut(registry, "/")
	login := func(push bool) {
		user, token, err := p.RegistryCredentials(t.Context(), host, push)
		if err != nil {
			t.Fatalf("registry credentials: %v", err)
		}
		if out, err := p.machines.Output(t.Context(), name, strings.NewReader(token), "docker", "login", "--username", user, "--password-stdin", host); err != nil {
			t.Fatalf("docker login %s: %v\n%s", host, err, out)
		}
	}
	ref := registry + "/busybox:check"
	login(true)
	if out, err := p.machines.Output(t.Context(), name, nil, "sh", "-c", "docker tag busybox:1.36 "+ref+" && docker push "+ref+" && docker rmi "+ref); err != nil {
		t.Fatalf("push %s with the push account: %v\n%s", ref, err, out)
	}
	login(false)
	if out, err := p.machines.Output(t.Context(), name, nil, "docker", "pull", ref); err != nil {
		t.Fatalf("pull %s with the pull account: %v\n%s", ref, err, out)
	}
	if _, err := p.machines.Output(t.Context(), name, nil, "docker", "tag", ref, registry+"/busybox:pushed-by-puller"); err != nil {
		t.Fatalf("tag the image: %v", err)
	}
	_, err := p.machines.Output(t.Context(), name, nil, "docker", "push", registry+"/busybox:pushed-by-puller")
	if err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("push with the pull account: %v; want the registry to refuse it for insufficient scope", err)
	}
	t.Logf("pushed %s with the push account and pulled it with the pull account, which may not push", ref)
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
