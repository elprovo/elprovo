package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	testAPIKey = "TESTAPIKEY0000000000000000000000000"
	waitLimit  = 5 * time.Second

	agentKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY agent"
)

// fakeMachines is a machine backend that keeps machines in memory. hold and
// fail let a test pause or break the next call of an operation.
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
		machines: map[string]*machines.Machine{}, specs: map[string]machines.Spec{},
		firewalls: map[string][]*machines.Firewall{}, addresses: map[string][]string{}, images: map[string]bool{},
		gates: map[string]chan struct{}{}, failNext: map[string]error{},
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
	gate := f.gates[op]
	err := f.failNext[op]
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
	if spec.PublicIP != "" {
		m.PublicIP = spec.PublicIP
	}
	f.machines[m.ID], f.specs[m.ID] = m, spec
	f.firewalls[m.ID] = []*machines.Firewall{spec.Firewall}
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

func (f *fakeMachines) lastFirewall(instanceID string) (*machines.Firewall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.firewalls["c-"+instanceID]
	if len(list) == 0 {
		return nil, false
	}
	return list[len(list)-1], true
}

func (f *fakeMachines) addressesOf(instanceID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses["c-"+instanceID]
}

func (f *fakeMachines) hasMachine(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines[id]
	return ok
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
}

func testConfig() config {
	return config{
		apiKeys: []string{testAPIKey}, network: "elprovo-sim-unit", rateLimit: 1000,
		registry: registryConfig{username: "root-user", password: "root-secret"},
	}
}

func newTestEnv(t *testing.T, statePath string, fake *fakeMachines, configure ...func(*config)) *testEnv {
	t.Helper()
	cfg := testConfig()
	for _, c := range configure {
		c(&cfg)
	}
	sim, err := newSimulation(cfg, statePath, fake, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newSimulation: %v", err)
	}
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
	return &testEnv{t: t, sim: sim, machines: fake, server: server}
}

func newEnv(t *testing.T, configure ...func(*config)) *testEnv {
	t.Helper()
	return newTestEnv(t, filepath.Join(t.TempDir(), "state.json"), newFakeMachines(), configure...)
}

func (e *testEnv) call(method, path string, body, out any) int {
	e.t.Helper()
	status, _ := e.callWithKey(method, path, testAPIKey, body, out)
	return status
}

func (e *testEnv) callWithKey(method, path, key string, body, out any) (int, http.Header) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, resp.Header
}

func (e *testEnv) mustCall(want int, method, path string, body, out any) {
	e.t.Helper()
	var raw json.RawMessage
	if got := e.call(method, path, body, &raw); got != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

// createInstance creates an Ubuntu instance in ewr with extra request fields
// and waits until it is active.
func (e *testEnv) createInstance(label string, extra map[string]any) instanceJSON {
	e.t.Helper()
	req := map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "os_id": ubuntuOS.ID, "label": label, "hostname": label}
	for k, v := range extra {
		req[k] = v
	}
	var created struct {
		Instance instanceJSON `json:"instance"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/instances", req, &created)
	return e.waitActive(created.Instance.ID)
}

func (e *testEnv) instance(id string) instanceJSON {
	e.t.Helper()
	var got struct {
		Instance instanceJSON `json:"instance"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances/"+id, nil, &got)
	return got.Instance
}

func (e *testEnv) waitActive(id string) instanceJSON {
	e.t.Helper()
	var got instanceJSON
	eventually(e.t, "instance "+id+" is active", func() bool {
		got = e.instance(id)
		return got.Status != "pending"
	})
	return got
}

func (e *testEnv) createFirewallGroup(rules ...map[string]any) string {
	e.t.Helper()
	var group struct {
		FirewallGroup firewallGroupJSON `json:"firewall_group"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/firewalls", map[string]string{"description": "test"}, &group)
	for _, rule := range rules {
		e.mustCall(http.StatusCreated, http.MethodPost, "/v2/firewalls/"+group.FirewallGroup.ID+"/rules", rule, nil)
	}
	return group.FirewallGroup.ID
}

func (e *testEnv) createVPC(regionID string) vpcJSON {
	e.t.Helper()
	var created struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/vpcs", map[string]string{"region": regionID}, &created)
	return created.VPC
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

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
