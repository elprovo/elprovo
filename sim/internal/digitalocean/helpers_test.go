package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	testToken = "dop_v1_unit"
	waitLimit = 5 * time.Second

	agentKey           = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY agent"
	testKeyFingerprint = "d9:75:02:7a:8d:57:ab:eb:d0:d4:0d:46:4f:fe:24:c4"
)

type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]*machines.Machine
	specs     map[string]machines.Spec
	firewalls map[string][]*machines.Firewall
	addresses map[string][]string
	images    map[string]bool
	gates     map[string]chan struct{}
	nextIP    int
	failNext  map[string]error
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

func (f *fakeMachines) hasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[ref]
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

func (f *fakeMachines) Restart(_ context.Context, id string, _ bool) (machines.Machine, error) {
	if err := f.enter("Restart"); err != nil {
		return machines.Machine{}, err
	}
	return f.setRunning(id, true)
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

// addressesOf returns the extra addresses that the droplet's machine has.
func (f *fakeMachines) addressesOf(dropletID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses["c-"+strconv.FormatInt(dropletID, 10)]
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

func (f *fakeMachines) spec(dropletID int64) machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs["c-"+strconv.FormatInt(dropletID, 10)]
}

func (f *fakeMachines) lastFirewall(dropletID int64) (*machines.Firewall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.firewalls["c-"+strconv.FormatInt(dropletID, 10)]
	if len(list) == 0 {
		return nil, false
	}
	return list[len(list)-1], true
}

func (f *fakeMachines) hasMachine(dropletID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines["c-"+strconv.FormatInt(dropletID, 10)]
	return ok
}

type testEnv struct {
	t        *testing.T
	sim      *Simulation
	machines *fakeMachines
	server   *httptest.Server
}

func testConfig() config {
	return config{
		tokens: []string{testToken}, regions: []string{"nyc3", "fra1"}, network: "elprovo-sim-unit",
		registry: registryConfig{endpoint: "registry.test:5000", apiURL: "http://registry.test:5000", username: "user", password: "secret"},
		limits:   rateLimits{perHour: 5000, perMinute: 250},
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
	status, _ := e.callWithHeaders(method, path, testToken, body, out)
	return status
}

func (e *testEnv) callWithHeaders(method, path, token string, body, out any) (int, http.Header) {
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
	req.Header.Set("Authorization", "Bearer "+token)
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

type errorBody struct {
	ID        string `json:"id"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func (e *testEnv) createDroplet(name string, tags ...string) dropletJSON {
	e.t.Helper()
	var created struct {
		Droplet dropletJSON `json:"droplet"`
		Links   links       `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{
		"name": name, "region": "nyc3", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64", "tags": tags,
	}, &created)
	e.waitAction(created.Links.Actions[0].ID)
	return e.droplet(created.Droplet.ID)
}

func (e *testEnv) droplet(id int64) dropletJSON {
	e.t.Helper()
	var got struct {
		Droplet dropletJSON `json:"droplet"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/droplets/"+strconv.FormatInt(id, 10), nil, &got)
	return got.Droplet
}

func (e *testEnv) waitAction(id int64) actionJSON {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		var got struct {
			Action actionJSON `json:"action"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v2/actions/"+strconv.FormatInt(id, 10), nil, &got)
		if got.Action.Status != "in-progress" {
			return got.Action
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("action %d is still in progress", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func (e *testEnv) postAction(dropletID int64, body map[string]any) (int, actionJSON) {
	e.t.Helper()
	var got struct {
		Action actionJSON `json:"action"`
	}
	status := e.call(http.MethodPost, "/v2/droplets/"+strconv.FormatInt(dropletID, 10)+"/actions", body, &got)
	return status, got.Action
}
