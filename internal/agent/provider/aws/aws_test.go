package aws

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
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

const (
	testAccessKey = "AKIDSTAGELYTEST"
	testRegion    = "us-east-1"
)

type fakeInstance struct {
	id, name, group, state string
}

// fakeCloud is the EC2 and ECR APIs of one account, and the SSH server of
// every instance it launches. Instances present the host key from their user
// data.
type fakeCloud struct {
	t       *testing.T
	agent   ssh.PublicKey
	sshAddr string

	mu sync.Mutex
	// instances are in launch order.
	instances    []*fakeInstance
	hostKeys     map[string]ssh.Signer
	launched     []url.Values
	keysImported int
	keyKnown     bool
	// terminatePolls is how many DescribeInstances calls by id a
	// terminating instance stays shutting-down for.
	terminatePolls int
	tokensIssued   int
	// federations are the GetFederationToken requests, in order. ECR takes
	// only the credentials of the newest one.
	federations []url.Values
	scopes      []string
	commands    []string
	stdin       []string
}

func newFakeCloud(t *testing.T, agent ssh.PublicKey) *fakeCloud {
	t.Helper()
	c := &fakeCloud{t: t, agent: agent, hostKeys: map[string]ssh.Signer{}}
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
	auth := r.Header.Get("Authorization")
	credential, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), ",")
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[2] != testRegion {
		writeEC2Error(w, http.StatusUnauthorized, "AuthFailure", "AWS was not able to validate the provided access credentials")
		return
	}
	c.scopes = append(c.scopes, parts[3])
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		if n := len(c.federations); n == 0 || parts[0] != sessionAccessKey(n) || r.Header.Get("X-Amz-Security-Token") != sessionToken(n) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"UnrecognizedClientException","message":"ECR takes only the newest session's credentials"}`)
			return
		}
		c.serveECR(w, target)
		return
	}
	if parts[0] != testAccessKey {
		writeEC2Error(w, http.StatusUnauthorized, "AuthFailure", "AWS was not able to validate the provided access credentials")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeEC2Error(w, http.StatusBadRequest, "MalformedQueryString", err.Error())
		return
	}
	if parts[3] == "sts" {
		c.serveSTS(w, r.PostForm)
		return
	}
	if r.PostForm.Get("Version") != "2016-11-15" {
		writeEC2Error(w, http.StatusBadRequest, "InvalidParameterValue", "unknown version")
		return
	}
	c.serveEC2(w, r.PostForm)
}

func (c *fakeCloud) serveEC2(w http.ResponseWriter, q url.Values) {
	switch q.Get("Action") {
	case "DescribeKeyPairs":
		if !c.keyKnown {
			writeEC2Error(w, http.StatusBadRequest, "InvalidKeyPair.NotFound", "The key pair '"+q.Get("KeyName.1")+"' does not exist")
			return
		}
		writeEC2(w, "DescribeKeyPairs", "<keySet><item><keyName>"+q.Get("KeyName.1")+"</keyName></item></keySet>")
	case "ImportKeyPair":
		c.keysImported++
		c.keyKnown = true
		writeEC2(w, "ImportKeyPair", "<keyName>"+q.Get("KeyName")+"</keyName>")
	case "RunInstances":
		c.launched = append(c.launched, q)
		userData, err := base64.StdEncoding.DecodeString(q.Get("UserData"))
		if err != nil {
			writeEC2Error(w, http.StatusBadRequest, "InvalidParameterValue", "user data is not base64")
			return
		}
		i := &fakeInstance{id: fmt.Sprintf("i-%017d", len(c.launched)), state: "running"}
		for n := 1; q.Has("TagSpecification.1.Tag." + strconv.Itoa(n) + ".Key"); n++ {
			prefix := "TagSpecification.1.Tag." + strconv.Itoa(n)
			switch q.Get(prefix + ".Key") {
			case "Name":
				i.name = q.Get(prefix + ".Value")
			case groupTag:
				i.group = q.Get(prefix + ".Value")
			}
		}
		c.instances = append(c.instances, i)
		c.hostKeys[i.name] = hostKeyFromUserData(c.t, string(userData))
		writeEC2(w, "RunInstances", "<instancesSet>"+instanceXML(i)+"</instancesSet>")
	case "DescribeInstances":
		c.describeInstances(w, q)
	case "TerminateInstances":
		i := c.instance(q.Get("InstanceId.1"))
		if i == nil {
			writeEC2Error(w, http.StatusBadRequest, "InvalidInstanceID.NotFound", "The instance ID '"+q.Get("InstanceId.1")+"' does not exist")
			return
		}
		i.state = "shutting-down"
		writeEC2(w, "TerminateInstances", "<instancesSet><item><instanceId>"+i.id+"</instanceId></item></instancesSet>")
	default:
		writeEC2Error(w, http.StatusBadRequest, "InvalidAction", "The action "+q.Get("Action")+" is not valid for this web service.")
	}
}

func (c *fakeCloud) describeInstances(w http.ResponseWriter, q url.Values) {
	filters := map[string][]string{}
	for n := 1; q.Has("Filter." + strconv.Itoa(n) + ".Name"); n++ {
		prefix := "Filter." + strconv.Itoa(n)
		var values []string
		for m := 1; q.Has(prefix + ".Value." + strconv.Itoa(m)); m++ {
			values = append(values, q.Get(prefix+".Value."+strconv.Itoa(m)))
		}
		filters[q.Get(prefix+".Name")] = values
	}
	var items strings.Builder
	if id := q.Get("InstanceId.1"); id != "" {
		i := c.instance(id)
		if i == nil {
			writeEC2Error(w, http.StatusBadRequest, "InvalidInstanceID.NotFound", "The instance ID '"+id+"' does not exist")
			return
		}
		if i.state == "shutting-down" {
			if c.terminatePolls--; c.terminatePolls <= 0 {
				i.state = "terminated"
			}
		}
		items.WriteString(instanceXML(i))
	}
	for _, i := range c.instances {
		if len(filters) == 0 {
			break
		}
		if matches(filters["tag:Name"], i.name) && matches(filters["tag:"+groupTag], i.group) && matches(filters["instance-state-name"], i.state) {
			items.WriteString(instanceXML(i))
		}
	}
	writeEC2(w, "DescribeInstances", "<reservationSet><item><reservationId>r-1</reservationId><instancesSet>"+items.String()+"</instancesSet></item></reservationSet>")
}

func matches(allowed []string, value string) bool {
	return allowed == nil || slices.Contains(allowed, value)
}

func sessionAccessKey(n int) string { return fmt.Sprintf("ASIATEST%08d", n) }

func sessionToken(n int) string { return fmt.Sprintf("session-token-%d", n) }

// serveSTS answers GetFederationToken with new temporary credentials.
func (c *fakeCloud) serveSTS(w http.ResponseWriter, q url.Values) {
	if q.Get("Action") != "GetFederationToken" || q.Get("Version") != "2011-06-15" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>InvalidAction</Code><Message>unknown action</Message></Error></ErrorResponse>`)
		return
	}
	c.federations = append(c.federations, q)
	n := len(c.federations)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, `<GetFederationTokenResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetFederationTokenResult><Credentials>`+
		`<SessionToken>`+sessionToken(n)+`</SessionToken><SecretAccessKey>session-secret</SecretAccessKey>`+
		`<Expiration>2026-10-12T00:00:00.000Z</Expiration><AccessKeyId>`+sessionAccessKey(n)+`</AccessKeyId></Credentials>`+
		`<FederatedUser><Arn>arn:aws:sts::123456789012:federated-user/`+q.Get("Name")+`</Arn><FederatedUserId>123456789012:`+q.Get("Name")+`</FederatedUserId></FederatedUser>`+
		`<PackedPolicySize>20</PackedPolicySize></GetFederationTokenResult><ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></GetFederationTokenResponse>`)
}

func (c *fakeCloud) serveECR(w http.ResponseWriter, target string) {
	if target != "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"UnknownOperationException"}`)
		return
	}
	c.tokensIssued++
	token := base64.StdEncoding.EncodeToString([]byte("AWS:ecr-password-" + strconv.Itoa(c.tokensIssued)))
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{
		"authorizationToken": token, "expiresAt": 1.7604e9, "proxyEndpoint": "https://123456789012.dkr.ecr.us-east-1.amazonaws.com",
	}}})
}

func (c *fakeCloud) instance(id string) *fakeInstance {
	for _, i := range c.instances {
		if i.id == id {
			return i
		}
	}
	return nil
}

// instanceXML renders an instance with the private address 127.0.0.2 and
// the public address 127.0.0.1, where the fake SSH server listens.
func instanceXML(i *fakeInstance) string {
	return "<item><instanceId>" + i.id + "</instanceId><instanceState><code>16</code><name>" + i.state + "</name></instanceState>" +
		"<privateIpAddress>127.0.0.2</privateIpAddress><ipAddress>127.0.0.1</ipAddress><tagSet>" +
		"<item><key>Name</key><value>" + i.name + "</value></item><item><key>" + groupTag + "</key><value>" + i.group + "</value></item>" +
		"</tagSet></item>"
}

func writeEC2(w http.ResponseWriter, action, inner string) {
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<`+action+`Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req-1</requestId>`+inner+`</`+action+`Response>`)
}

func writeEC2Error(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<Response><Errors><Error><Code>`+code+`</Code><Message>`+message+`</Message></Error></Errors><RequestID>req-1</RequestID></Response>`)
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
// Every instance shares this one server, so it presents the host key of
// every launched instance.
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

func newProvider(t *testing.T, subnet string) (*Provider, *fakeCloud) {
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
		EC2URL: api.URL, ECRURL: api.URL, STSURL: api.URL, Region: testRegion, Credentials: Credentials{AccessKeyID: testAccessKey, SecretAccessKey: "secret"},
		RepositoryPrefix: "acme",
		InstanceType:     "t3.medium", Image: "ami-0123456789abcdef0", SubnetID: subnet, SecurityGroupID: "sg-0stagelets", VolumeSize: 30,
		Group: "stagely-stagelet", HostKeyDir: t.TempDir(), ReadyTimeout: 2 * time.Second,
	}, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, p.machines.Port, _ = net.SplitHostPort(cloud.sshAddr)
	p.pollEvery = 10 * time.Millisecond
	return p, cloud
}

func launchedUserData(t *testing.T, q url.Values) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(q.Get("UserData"))
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	return string(data)
}

func TestCreateLaunchesAnInstanceTheAgentCanDrive(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()

	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	q := cloud.launched[0]
	want := map[string]string{
		"ImageId": "ami-0123456789abcdef0", "InstanceType": "t3.medium", "MinCount": "1", "MaxCount": "1",
		"SecurityGroupId.1": "sg-0stagelets", "TagSpecification.1.ResourceType": "instance",
		"TagSpecification.1.Tag.1.Key": "Name", "TagSpecification.1.Tag.1.Value": "stagely-a1",
		"TagSpecification.1.Tag.2.Key": "stagely-group", "TagSpecification.1.Tag.2.Value": "stagely-stagelet",
		"MetadataOptions.HttpTokens": "required", "MetadataOptions.HttpPutResponseHopLimit": "1",
		"BlockDeviceMapping.1.DeviceName": "/dev/sda1", "BlockDeviceMapping.1.Ebs.VolumeSize": "30", "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("RunInstances %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Has("SubnetId") || !strings.HasPrefix(q.Get("KeyName"), "stagely-agent-") {
		t.Errorf("RunInstances SubnetId = %q, KeyName = %q; want no subnet and the agent's key pair", q.Get("SubnetId"), q.Get("KeyName"))
	}
	userData := launchedUserData(t, q)
	if !strings.Contains(userData, "docker-compose-v2") || !strings.HasSuffix(userData, "\ndisable_root: false\n") {
		t.Fatalf("user data does not install Docker Compose and let root sign in:\n%s", userData)
	}
	var out bytes.Buffer
	if err := p.Run(ctx, "stagely-a1", nil, &out, nil, "echo", "it's here"); err != nil || out.String() != "ran\n" {
		t.Fatalf("Run = %q, %v", out.String(), err)
	}
	if got := cloud.commands[len(cloud.commands)-1]; got != `'echo' 'it'\''s here'` {
		t.Fatalf("command = %s, want each argument quoted", got)
	}
	if names, err := p.List(ctx); err != nil || !slices.Equal(names, []string{"stagely-a1"}) {
		t.Fatalf("List = %v, %v; want the new instance", names, err)
	}
}

func TestRequestsAreSignedForEachService(t *testing.T) {
	p, cloud := newProvider(t, "")

	if _, err := p.Exists(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if _, _, err := p.RegistryCredentials(context.Background(), testRegistry, true); err != nil {
		t.Fatalf("RegistryCredentials: %v", err)
	}
	if !slices.Equal(cloud.scopes, []string{"ec2", "sts", "ecr"}) {
		t.Fatalf("signed for services %v, want ec2, sts and ecr", cloud.scopes)
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

func TestCopyInStreamsTheFile(t *testing.T) {
	p, cloud := newProvider(t, "")
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

func TestAgentUsesThePrivateAddressInsideASubnet(t *testing.T) {
	p, cloud := newProvider(t, "subnet-0stagelets")
	ctx := context.Background()
	if _, err := p.ec2.runInstance(ctx, runInstance{
		ImageID: "ami-0123456789abcdef0", UserData: mustMachineConfig(t), SubnetID: p.cfg.SubnetID,
		Tags: []tag{{"Name", "stagely-a1"}, {groupTag, "stagely-stagelet"}},
	}); err != nil {
		t.Fatal(err)
	}

	addr, err := p.Address(ctx, "stagely-a1")
	if err != nil || addr != "127.0.0.2" {
		t.Fatalf("Address = %q, %v; want the private address", addr, err)
	}
	if cloud.launched[0].Get("SubnetId") != "subnet-0stagelets" {
		t.Fatalf("RunInstances SubnetId = %q, want the subnet", cloud.launched[0].Get("SubnetId"))
	}
}

func TestKeyPairIsImportedOnce(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()

	for _, name := range []string{"stagely-a1", "stagely-a2"} {
		if err := p.Create(ctx, name); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if cloud.keysImported != 1 || cloud.launched[0].Get("KeyName") != cloud.launched[1].Get("KeyName") {
		t.Fatalf("key pairs imported = %d, key names %q and %q; want one key pair for both", cloud.keysImported,
			cloud.launched[0].Get("KeyName"), cloud.launched[1].Get("KeyName"))
	}
}

func TestDeleteWaitsUntilTheInstanceIsTerminated(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.terminatePolls = 3

	if err := p.Delete(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if state := cloud.instances[0].state; state != "terminated" {
		t.Fatalf("instance state after Delete = %s, want terminated", state)
	}
	if exists, err := p.Exists(ctx, "stagely-a1"); err != nil || exists {
		t.Fatalf("Exists = %v, %v; want gone", exists, err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.HostKeyDir, "stagely-a1"+".pub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host key file err = %v, want it removed", err)
	}
}

func TestDeleteFailsWhileTheInstanceIsStillShuttingDown(t *testing.T) {
	p, cloud := newProvider(t, "")
	ctx := context.Background()
	if err := p.Create(ctx, "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloud.terminatePolls = 1 << 30
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
	p.cfg.GoldenImage = "ami-0golden000000000"

	if err := p.Create(context.Background(), "stagely-a1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	q := cloud.launched[0]
	userData := launchedUserData(t, q)
	if q.Get("ImageId") != "ami-0golden000000000" || strings.Contains(userData, "packages:") {
		t.Fatalf("RunInstances ImageId = %q, user data installs packages = %v; want the golden image and no packages",
			q.Get("ImageId"), strings.Contains(userData, "packages:"))
	}
}

const testRegistry = "123456789012.dkr.ecr.us-east-1.amazonaws.com"

func TestRegistryCredentialsAreAFreshECRTokenForEachJob(t *testing.T) {
	p, _ := newProvider(t, "")

	var passwords []string
	for _, push := range []bool{true, false} {
		user, password, err := p.RegistryCredentials(context.Background(), testRegistry, push)
		if err != nil || user != "AWS" {
			t.Fatalf("RegistryCredentials = %q, %v; want the user AWS", user, err)
		}
		passwords = append(passwords, password)
	}
	if !slices.Equal(passwords, []string{"ecr-password-1", "ecr-password-2"}) {
		t.Fatalf("passwords = %v, want a new token for each job", passwords)
	}
}

func TestRegistryCredentialsGoOnlyToTheAccountsRegistry(t *testing.T) {
	p, cloud := newProvider(t, "")

	_, _, err := p.RegistryCredentials(context.Background(), "registry.example.com", false)

	if err == nil || !strings.Contains(err.Error(), "registry.example.com") || len(cloud.federations) != 0 {
		t.Fatalf("credentials for another registry: err = %v after %d federation tokens, want a refusal before any", err, len(cloud.federations))
	}
}

func TestRegistryCredentialsForAStageletMachineCanOnlyPull(t *testing.T) {
	p, cloud := newProvider(t, "")

	if _, _, err := p.RegistryCredentials(context.Background(), testRegistry, false); err != nil {
		t.Fatalf("RegistryCredentials: %v", err)
	}

	federation := onlyFederation(t, cloud)
	if federation.Get("Name") != "stagely-registry-pull" || federation.Get("DurationSeconds") != "43200" {
		t.Fatalf("GetFederationToken = %v, want the federated user stagely-registry-pull for 12 hours", federation)
	}
	want := map[string][]string{
		"*": {"ecr:GetAuthorizationToken"},
		"arn:aws:ecr:us-east-1:123456789012:repository/acme/*": {"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"},
	}
	if got := allowedByPolicy(t, federation.Get("Policy")); !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("session policy allows %v, want only the token and pulls under the prefix", got)
	}
}

func TestRegistryCredentialsForABuildWorkerCanPushUnderThePrefix(t *testing.T) {
	p, cloud := newProvider(t, "")

	if _, _, err := p.RegistryCredentials(context.Background(), testRegistry, true); err != nil {
		t.Fatalf("RegistryCredentials: %v", err)
	}

	federation := onlyFederation(t, cloud)
	want := map[string][]string{
		"*": {"ecr:GetAuthorizationToken"},
		"arn:aws:ecr:us-east-1:123456789012:repository/acme/*": {
			"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:CompleteLayerUpload", "ecr:CreateRepository", "ecr:GetDownloadUrlForLayer",
			"ecr:InitiateLayerUpload", "ecr:PutImage", "ecr:UploadLayerPart",
		},
	}
	if got := allowedByPolicy(t, federation.Get("Policy")); federation.Get("Name") != "stagely-registry-push" || !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("federated user %q with a session policy that allows %v, want stagely-registry-push with pushes and pulls under the prefix", federation.Get("Name"), got)
	}
}

func onlyFederation(t *testing.T, cloud *fakeCloud) url.Values {
	t.Helper()
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.federations) != 1 {
		t.Fatalf("%d GetFederationToken requests, want 1", len(cloud.federations))
	}
	return cloud.federations[0]
}

// allowedByPolicy returns, for each resource that an IAM policy names, the
// actions it allows there, sorted. It fails on any statement that is not a
// plain Allow.
func allowedByPolicy(t *testing.T, doc string) map[string][]string {
	t.Helper()
	var p struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect   string          `json:"Effect"`
			Action   json.RawMessage `json:"Action"`
			Resource string          `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil || p.Version != "2012-10-17" {
		t.Fatalf("session policy %s: %v", doc, err)
	}
	allowed := map[string][]string{}
	for _, s := range p.Statement {
		var actions []string
		if json.Unmarshal(s.Action, &actions) != nil {
			var one string
			if err := json.Unmarshal(s.Action, &one); err != nil {
				t.Fatalf("statement action %s: %v", s.Action, err)
			}
			actions = []string{one}
		}
		if s.Effect != "Allow" {
			t.Fatalf("statement effect %q, want Allow", s.Effect)
		}
		allowed[s.Resource] = append(allowed[s.Resource], actions...)
		slices.Sort(allowed[s.Resource])
	}
	return allowed
}

func TestProviderNeedsASecurityGroup(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)

	_, err := New(Config{
		Region: testRegion, Credentials: Credentials{AccessKeyID: testAccessKey, SecretAccessKey: "secret"}, InstanceType: "t3.medium",
		Image: "ami-0123456789abcdef0", Group: "stagely-stagelet", HostKeyDir: t.TempDir(),
	}, key)

	if err == nil || !strings.Contains(err.Error(), "security group") {
		t.Fatalf("New without a security group: err = %v; want a refusal, so no instance falls back to the VPC's default group", err)
	}
}

func mustMachineConfig(t *testing.T) string {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	userData, err := cloudinit.MachineConfig(string(pem.EncodeToMemory(block)))
	if err != nil {
		t.Fatal(err)
	}
	return userData
}
