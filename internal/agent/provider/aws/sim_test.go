//go:build simdocker

package aws

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProviderAgainstSimulation runs a stagelet's machine lifecycle against
// stagely-sim's AWS simulation. As setup does, it puts the instances in a
// security group that lets in only the agent, here the test's addresses. It
// launches a stagelet and a neighbor into the default VPC, waits until
// cloud-init and Docker Compose are ready, runs a Compose project on the
// stagelet and reaches it, checks that the neighbor can connect out but not
// to the stagelet and that a container can't reach the metadata service.
// It pushes an image with a build worker's registry credentials, pulls it
// with a stagelet machine's and is refused a push with them, then
// terminates both instances. It needs a running simulation that serves EC2,
// STS and ECR at STAGELY_SIM_AWS_URL for the access key in
// STAGELY_SIM_AWS_ACCESS_KEY_ID and STAGELY_SIM_AWS_SECRET_ACCESS_KEY, in
// STAGELY_SIM_AWS_REGION, the stock AMI in STAGELY_SIM_AWS_IMAGE, and an
// image prefix, <account registry host>/<repository prefix>, in
// STAGELY_SIM_AWS_REGISTRY, of a registry that uses the simulation as its
// token service. It must run where instance addresses are reachable. With
// STAGELY_SIM_AWS_GOLDEN_IMAGE, the instances boot from that AMI.
// "sim/scripts/check-aws.sh agent" and "image" set them up.
func TestProviderAgainstSimulation(t *testing.T) {
	env := map[string]string{}
	for _, name := range []string{"URL", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "REGION", "IMAGE", "REGISTRY"} {
		if env[name] = os.Getenv("STAGELY_SIM_AWS_" + name); env[name] == "" {
			t.Fatalf("STAGELY_SIM_AWS_%s must be set for a running simulation; run sim/scripts/check-aws.sh agent", name)
		}
	}
	creds := Credentials{AccessKeyID: env["ACCESS_KEY_ID"], SecretAccessKey: env["SECRET_ACCESS_KEY"]}
	ec2 := newClient(env["URL"], env["REGION"], "ec2", creds)
	group := agentOnlyGroup(t, ec2)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, repositoryPrefix, _ := strings.Cut(env["REGISTRY"], "/")
	p, err := New(Config{
		EC2URL: env["URL"], ECRURL: env["URL"], STSURL: env["URL"], Region: env["REGION"], Credentials: creds, InstanceType: "t3.medium",
		Image: env["IMAGE"], GoldenImage: os.Getenv("STAGELY_SIM_AWS_GOLDEN_IMAGE"), SecurityGroupID: group, VolumeSize: 30,
		Group: "stagely-sim-agent-check", RepositoryPrefix: repositoryPrefix, HostKeyDir: t.TempDir(), ReadyTimeout: 10 * time.Minute,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	p.pollEvery = time.Second
	stagelet, neighbor := "stagely-check-"+randomHex(t), "stagely-neighbor-"+randomHex(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.Delete(ctx, stagelet)
		_ = p.Delete(ctx, neighbor)
		_ = ec2.query(ctx, "DeleteSecurityGroup", url.Values{"GroupId": {group}}, &struct{}{})
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
		t.Fatalf("service on the stagelet's address answered %q", body)
	}
	checkNeighborIsolation(t, p, ec2, stagelet, neighbor, addr)
	metadata, err := p.machines.Output(t.Context(), stagelet, nil, "docker", "run", "--rm", "busybox:1.36", "wget", "-T", "5", "-qO-", "http://169.254.169.254/latest/user-data")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a container asked the metadata service: %q, %v; want the connection refused, so containers can't read the user data", metadata, err)
	}
	pushAndPull(t, p, stagelet, env["REGISTRY"])

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

// pushAndPull signs the machine in as the agent's runner does: with a build
// worker's credentials it pushes an image, and with a stagelet machine's it
// pulls the image and is refused a push.
func pushAndPull(t *testing.T, p *Provider, name, registry string) {
	t.Helper()
	host, _, _ := strings.Cut(registry, "/")
	login := func(push bool) {
		user, password, err := p.RegistryCredentials(t.Context(), host, push)
		if err != nil {
			t.Fatalf("registry credentials: %v", err)
		}
		if out, err := p.machines.Output(t.Context(), name, strings.NewReader(password), "docker", "login", "--username", user, "--password-stdin", host); err != nil {
			t.Fatalf("docker login %s: %v\n%s", host, err, out)
		}
	}
	ref := registry + "/busybox:check"
	login(true)
	if out, err := p.machines.Output(t.Context(), name, nil, "sh", "-c", "docker tag busybox:1.36 "+ref+" && docker push "+ref+" && docker rmi "+ref); err != nil {
		t.Fatalf("push %s with a build worker's credentials: %v\n%s", ref, err, out)
	}
	login(false)
	if out, err := p.machines.Output(t.Context(), name, nil, "docker", "pull", ref); err != nil {
		t.Fatalf("pull %s with a stagelet machine's credentials: %v\n%s", ref, err, out)
	}
	if _, err := p.machines.Output(t.Context(), name, nil, "docker", "tag", ref, registry+"/busybox:pushed-by-stagelet"); err != nil {
		t.Fatalf("tag the image: %v", err)
	}
	_, err := p.machines.Output(t.Context(), name, nil, "docker", "push", registry+"/busybox:pushed-by-stagelet")
	if err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("push with a stagelet machine's credentials: %v; want the registry to refuse it for insufficient scope", err)
	}
	t.Logf("pushed %s with a build worker's credentials and pulled it with a stagelet machine's, which may not push", ref)
}

// agentOnlyGroup creates a security group in the default VPC that lets in
// TCP from this test's IPv4 addresses only, as the stagelet group setup
// makes lets in only the agent and the gateway, and returns its id.
func agentOnlyGroup(t *testing.T, ec2 *client) string {
	t.Helper()
	var created struct {
		GroupID string `xml:"groupId"`
	}
	name := "stagely-check-" + randomHex(t)
	if err := ec2.query(t.Context(), "CreateSecurityGroup", url.Values{"GroupName": {name}, "GroupDescription": {"agent check"}}, &created); err != nil {
		t.Fatalf("create the security group: %v", err)
	}
	rule := url.Values{"GroupId": {created.GroupID}, "IpPermissions.1.IpProtocol": {"tcp"}, "IpPermissions.1.FromPort": {"0"}, "IpPermissions.1.ToPort": {"65535"}}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			n++
			rule.Set("IpPermissions.1.IpRanges."+strconv.Itoa(n)+".CidrIp", ip.IP.String()+"/32")
		}
	}
	if err := ec2.query(t.Context(), "AuthorizeSecurityGroupIngress", rule, &struct{}{}); err != nil {
		t.Fatalf("let the agent in: %v", err)
	}
	return created.GroupID
}

// checkNeighborIsolation checks that another instance in the stagelet group
// can't connect to the stagelet's published port, at either of its
// addresses, while it can connect out to the test.
func checkNeighborIsolation(t *testing.T, p *Provider, ec2 *client, stagelet, neighbor, stageletAddr string) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	connect := func(host, port string) error {
		_, err := p.machines.Output(t.Context(), neighbor, nil, "timeout", "3", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, port)
		return err
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if err := connect(egressAddress(t, stageletAddr), port); err != nil {
		t.Fatalf("the neighbor can't connect out, so its blocked connections would prove nothing: %v", err)
	}
	instances, err := ec2.describeInstances(t.Context(), []filter{{"tag:Name", []string{stagelet}}})
	if err != nil || len(instances) != 1 {
		t.Fatalf("find the stagelet: %v, %d instances", err, len(instances))
	}
	for _, addr := range []string{stageletAddr, instances[0].PrivateIP} {
		if err := connect(addr, "8080"); err == nil {
			t.Fatalf("the neighbor reached the stagelet's published port at %s through the security group", addr)
		}
	}
}

// egressAddress returns the address this host sends from toward host.
func egressAddress(t *testing.T, host string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "udp", net.JoinHostPort(host, "9"))
	if err != nil {
		t.Fatalf("find the address toward %s: %v", host, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
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
