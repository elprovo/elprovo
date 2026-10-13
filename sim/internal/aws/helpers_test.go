package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	testAccessKey = "AKIAUNITTESTKEY00000"
	testSecretKey = "unit-test-secret"
	testRegion    = "us-east-1"
	waitLimit     = 5 * time.Second

	agentKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY agent"
)

// fakeMachines is a machine backend that keeps machines in memory. hold
// makes an operation wait until it is released; fail makes its next call
// fail.
type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]*machines.Machine
	specs     map[string]machines.Spec
	firewalls map[string][]*machines.Firewall
	addresses map[string][]string
	images    map[string]bool
	gates     map[string]chan struct{}
	failNext  map[string]error
	nextIP    int
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{
		machines: map[string]*machines.Machine{}, specs: map[string]machines.Spec{}, firewalls: map[string][]*machines.Firewall{},
		addresses: map[string][]string{}, images: map[string]bool{}, gates: map[string]chan struct{}{}, failNext: map[string]error{},
	}
}

func (f *fakeMachines) hold(op string) (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.gates[op] = gate
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func (f *fakeMachines) fail(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[op] = err
}

func (f *fakeMachines) enter(op string) error {
	f.mu.Lock()
	gate, err := f.gates[op], f.failNext[op]
	delete(f.failNext, op)
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (f *fakeMachines) Create(_ context.Context, spec machines.Spec) (machines.Machine, error) {
	if err := f.enter("Create"); err != nil {
		return machines.Machine{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextIP++
	m := &machines.Machine{
		ID: "c-" + spec.Resource, Name: spec.Name, Cloud: spec.Cloud, Account: spec.Account, Resource: spec.Resource, Running: true,
		PrivateIP: fmt.Sprintf("172.30.255.%d", 255-f.nextIP), PublicIP: fmt.Sprintf("172.31.255.%d", 255-f.nextIP),
	}
	f.machines[m.ID], f.specs[m.ID] = m, spec
	f.firewalls[m.ID] = []*machines.Firewall{spec.Firewall}
	return *m, nil
}

func (f *fakeMachines) Inspect(_ context.Context, id string) (machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.Machine{}, machines.ErrNotFound
	}
	return *m, nil
}

func (f *fakeMachines) List(_ context.Context, cloud, account string) ([]machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []machines.Machine
	for _, m := range f.machines {
		if m.Cloud == cloud && m.Account == account {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *fakeMachines) setRunning(id string, running bool) (machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.Machine{}, machines.ErrNotFound
	}
	m.Running = running
	return *m, nil
}

func (f *fakeMachines) Start(_ context.Context, id string) (machines.Machine, error) {
	if err := f.enter("Start"); err != nil {
		return machines.Machine{}, err
	}
	return f.setRunning(id, true)
}

func (f *fakeMachines) Stop(_ context.Context, id string, _ bool) error {
	if err := f.enter("Stop"); err != nil {
		return err
	}
	_, err := f.setRunning(id, false)
	return err
}

func (f *fakeMachines) Delete(_ context.Context, id string) error {
	if err := f.enter("Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.machines, id)
	return nil
}

func (f *fakeMachines) SetFirewall(_ context.Context, id string, fw *machines.Firewall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewalls[id] = append(f.firewalls[id], fw)
	return nil
}

func (f *fakeMachines) SetAddresses(_ context.Context, id string, addrs []string) error {
	if err := f.enter("SetAddresses"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[id] = append([]string{}, addrs...)
	return nil
}

func (f *fakeMachines) Commit(_ context.Context, _ string, ref string) (int64, error) {
	if err := f.enter("Commit"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = true
	return 2_340_000_000, nil
}

func (f *fakeMachines) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, ref)
	return nil
}

func (f *fakeMachines) Networks(context.Context) (machines.Networks, error) {
	return machines.Networks{
		Private: machines.Subnet{Prefix: netip.MustParsePrefix("172.30.0.0/16"), Gateway: netip.MustParseAddr("172.30.0.1")},
		Public:  machines.Subnet{Prefix: netip.MustParsePrefix("172.31.0.0/16"), Gateway: netip.MustParseAddr("172.31.0.1")},
	}, nil
}

func (f *fakeMachines) spec(instanceID string) machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs["c-"+instanceID]
}

func (f *fakeMachines) hasMachine(instanceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines["c-"+instanceID]
	return ok
}

func (f *fakeMachines) lastFirewall(instanceID string) *machines.Firewall {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.firewalls["c-"+instanceID]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

func (f *fakeMachines) addressesOf(instanceID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses["c-"+instanceID]
}

func (f *fakeMachines) hasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[ref]
}

type testEnv struct {
	t        *testing.T
	sim      *Simulation
	machines *fakeMachines
	server   *httptest.Server
	clock    *testClock
}

// testClock is a clock tests can move forward.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testConfig() config {
	return config{
		accessKeyID: testAccessKey, secretAccessKey: testSecretKey, regions: []string{testRegion, "eu-central-1"},
		network: "elprovo-sim-unit",
	}
}

func newTestEnv(t *testing.T, statePath string, fake *fakeMachines) *testEnv {
	t.Helper()
	sim, err := newSimulation(testConfig(), statePath, fake, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newSimulation: %v", err)
	}
	clock := &testClock{now: time.Now().UTC()}
	sim.now = clock.Now
	if err := sim.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { sim.syncFirewalls(sim.bg) })
	server := httptest.NewServer(sim.handler())
	t.Cleanup(func() {
		server.Close()
		sim.close()
		wg.Wait()
	})
	return &testEnv{t: t, sim: sim, machines: fake, server: server, clock: clock}
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv(t, filepath.Join(t.TempDir(), "state.json"), newFakeMachines())
}

// testCredentials are an access key and, for a federated session, its
// session token.
type testCredentials struct {
	accessKeyID, secretAccessKey, sessionToken string
}

var userCredentials = testCredentials{accessKeyID: testAccessKey, secretAccessKey: testSecretKey}

// signedRequest returns a request signed for service in region with the
// test credentials at the simulation's current time.
func (e *testEnv) signedRequest(service, region string, header http.Header, body string) *http.Request {
	e.t.Helper()
	return e.signedRequestAs(userCredentials, service, region, header, body)
}

// signedRequestAs returns a request signed with creds, with the session
// token, if any, in X-Amz-Security-Token.
func (e *testEnv) signedRequestAs(creds testCredentials, service, region string, header http.Header, body string) *http.Request {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.server.URL+"/", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	amzDate := e.clock.Now().Format(amzDateFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	auth := authorization{
		accessKeyID: creds.accessKeyID, date: amzDate[:8], region: region, service: service,
		signedHeaders: []string{"content-type", "host", "x-amz-date"},
	}
	if creds.sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.sessionToken)
		auth.signedHeaders = append(auth.signedHeaders, "x-amz-security-token")
	}
	if req.Header.Get("X-Amz-Target") != "" {
		auth.signedHeaders = append(auth.signedHeaders, "x-amz-target")
	}
	req.Host = req.URL.Host
	auth.signature = signature(creds.secretAccessKey, auth, canonicalRequest(req, auth.signedHeaders, []byte(body)), amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, creds.accessKeyID, auth.date, region, service, strings.Join(auth.signedHeaders, ";"), auth.signature))
	return req
}

func (e *testEnv) do(req *http.Request) (int, []byte) {
	e.t.Helper()
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// ec2 calls an EC2 action in the test region with params, given as
// alternating names and values.
func (e *testEnv) ec2(action string, params ...string) (int, []byte) {
	e.t.Helper()
	return e.ec2In(testRegion, action, params...)
}

func (e *testEnv) ec2In(region, action string, params ...string) (int, []byte) {
	e.t.Helper()
	form := url.Values{"Action": {action}, "Version": {"2016-11-15"}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Add(params[i], params[i+1])
	}
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=utf-8"}}
	return e.do(e.signedRequest("ec2", region, header, form.Encode()))
}

// mustEC2 calls an EC2 action that must succeed and decodes its answer
// into out.
func (e *testEnv) mustEC2(out any, action string, params ...string) {
	e.t.Helper()
	status, body := e.ec2(action, params...)
	if status != http.StatusOK {
		e.t.Fatalf("%s = %d: %s", action, status, body)
	}
	if out != nil {
		if err := xml.Unmarshal(body, out); err != nil {
			e.t.Fatalf("decode %s: %v: %s", action, err, body)
		}
	}
}

// ec2Error calls an EC2 action that must fail and returns its error code.
func (e *testEnv) ec2Error(action string, params ...string) string {
	e.t.Helper()
	status, body := e.ec2(action, params...)
	var resp struct {
		Code string `xml:"Errors>Error>Code"`
	}
	if status < 400 || xml.Unmarshal(body, &resp) != nil {
		e.t.Fatalf("%s = %d %s, want an error", action, status, body)
	}
	return resp.Code
}

// ecr calls an ECR operation with a JSON request and returns the status and
// decoded body.
func (e *testEnv) ecr(operation string, in any) (int, map[string]any) {
	e.t.Helper()
	return e.ecrAs(userCredentials, operation, in)
}

func (e *testEnv) ecrAs(creds testCredentials, operation string, in any) (int, map[string]any) {
	e.t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		e.t.Fatal(err)
	}
	header := http.Header{"Content-Type": {"application/x-amz-json-1.1"}, "X-Amz-Target": {ecrTargetPrefix + operation}}
	status, body := e.do(e.signedRequestAs(creds, "ecr", testRegion, header, string(data)))
	var out map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &out); err != nil {
		e.t.Fatalf("decode %s: %v: %s", operation, err, body)
	}
	return status, out
}

type testInstance struct {
	ID          string `xml:"instanceId"`
	State       string `xml:"instanceState>name"`
	PrivateIP   string `xml:"privateIpAddress"`
	PublicIP    string `xml:"ipAddress"`
	SubnetID    string `xml:"subnetId"`
	VPCID       string `xml:"vpcId"`
	StateReason string `xml:"stateReason>code"`
	Groups      []struct {
		ID string `xml:"groupId"`
	} `xml:"groupSet>item"`
	Tags []tag `xml:"tagSet>item"`
}

type describeInstances struct {
	Reservations []struct {
		Instances []testInstance `xml:"instancesSet>item"`
	} `xml:"reservationSet>item"`
}

func (d describeInstances) all() []testInstance {
	var out []testInstance
	for _, r := range d.Reservations {
		out = append(out, r.Instances...)
	}
	return out
}

// instance returns the instance as DescribeInstances reports it.
func (e *testEnv) instance(id string) testInstance {
	e.t.Helper()
	var got describeInstances
	e.mustEC2(&got, "DescribeInstances", "InstanceId.1", id)
	if all := got.all(); len(all) == 1 {
		return all[0]
	}
	e.t.Fatalf("DescribeInstances %s found %d instances", id, len(got.all()))
	return testInstance{}
}

func (e *testEnv) stockImage() string {
	e.t.Helper()
	return stockImageID(stockImages[0], testRegion)
}

// runInstance launches one instance with params and returns its id.
func (e *testEnv) runInstance(params ...string) string {
	e.t.Helper()
	var run struct {
		Instances []testInstance `xml:"instancesSet>item"`
	}
	base := []string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "MinCount", "1", "MaxCount", "1"}
	e.mustEC2(&run, "RunInstances", append(base, params...)...)
	if len(run.Instances) != 1 || run.Instances[0].State != "pending" {
		e.t.Fatalf("RunInstances = %+v, want one pending instance", run.Instances)
	}
	return run.Instances[0].ID
}

// waitState waits until DescribeInstances reports the instance in state.
func (e *testEnv) waitState(id, state string) testInstance {
	e.t.Helper()
	var last testInstance
	eventually(e.t, "instance "+id+" is "+state, func() bool {
		last = e.instance(id)
		return last.State == state
	})
	return last
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
