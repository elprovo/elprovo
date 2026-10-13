package hetzner

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const waitLimit = 5 * time.Second

var testToken = strings.Repeat("t", tokenLength)

const agentKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY agent"

type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]*machines.Machine
	specs     map[string]machines.Spec
	firewalls map[string][]*machines.Firewall
	images    map[string]bool
	gates     map[string]chan struct{}
	failNext  map[string]error
	nextIP    int
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{
		machines: map[string]*machines.Machine{}, specs: map[string]machines.Spec{}, firewalls: map[string][]*machines.Firewall{},
		images: map[string]bool{}, gates: map[string]chan struct{}{}, failNext: map[string]error{},
	}
}

// hold blocks the next calls of op until release is called.
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
	public := spec.PublicIP
	if public == "" {
		public = fmt.Sprintf("172.31.255.%d", 255-f.nextIP)
	}
	m := &machines.Machine{
		ID: "c-" + spec.Resource, Name: spec.Name, Cloud: spec.Cloud, Account: spec.Account, Resource: spec.Resource, Running: true,
		PrivateIP: fmt.Sprintf("172.30.255.%d", 255-f.nextIP), PublicIP: public,
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
	if err := f.enter("SetFirewall"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewalls[id] = append(f.firewalls[id], fw)
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

func (f *fakeMachines) hasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[ref]
}

func (f *fakeMachines) spec(serverID int64) machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs["c-"+strconv.FormatInt(serverID, 10)]
}

func (f *fakeMachines) hasMachine(serverID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines["c-"+strconv.FormatInt(serverID, 10)]
	return ok
}

// lastFirewall returns the firewall the server's machine enforces now.
func (f *fakeMachines) lastFirewall(serverID int64) *machines.Firewall {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.firewalls["c-"+strconv.FormatInt(serverID, 10)]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

type testEnv struct {
	t        *testing.T
	sim      *Simulation
	machines *fakeMachines
	server   *httptest.Server
}

func testConfig() config {
	return config{tokens: []string{testToken}, network: "elprovo-sim-unit", rateLimit: 3600}
}

// newTestEnv starts a simulation on fake machines. Each prepare function
// changes the simulation before it starts serving.
func newTestEnv(t *testing.T, statePath string, fake *fakeMachines, prepare ...func(*Simulation)) *testEnv {
	t.Helper()
	sim, err := newSimulation(testConfig(), statePath, fake, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newSimulation: %v", err)
	}
	for _, p := range prepare {
		p(sim)
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

func newEnv(t *testing.T, prepare ...func(*Simulation)) *testEnv {
	t.Helper()
	return newTestEnv(t, filepath.Join(t.TempDir(), "state.json"), newFakeMachines(), prepare...)
}

func (e *testEnv) do(method, path, token string, body any) (int, http.Header, []byte) {
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
	return resp.StatusCode, resp.Header, data
}

// call sends the request with the test token and decodes the answer into
// out when it has a body.
func (e *testEnv) call(method, path string, body, out any) int {
	e.t.Helper()
	status, _, data := e.do(method, path, testToken, body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return status
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
	Error struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

// errorCode sends the request and returns the status and Hetzner error
// code of its answer.
func (e *testEnv) errorCode(method, path string, body any) (int, string) {
	e.t.Helper()
	var got errorBody
	status := e.call(method, path, body, &got)
	return status, got.Error.Code
}

type createServerResponse struct {
	Server      serverJSON   `json:"server"`
	Action      actionJSON   `json:"action"`
	NextActions []actionJSON `json:"next_actions"`
}

// serverRequest is a create request that the simulation accepts.
func serverRequest(name string, labels map[string]string) map[string]any {
	return map[string]any{
		"name": name, "server_type": "cx23", "image": "ubuntu-24.04", "location": "fsn1", "labels": labels,
		"public_net": map[string]any{"enable_ipv4": true, "enable_ipv6": false},
	}
}

// createServer creates a server and waits until it runs.
func (e *testEnv) createServer(req map[string]any) serverJSON {
	e.t.Helper()
	var created createServerResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/servers", req, &created)
	e.waitAction(created.Action.ID)
	return e.serverByID(created.Server.ID)
}

func (e *testEnv) serverByID(id int64) serverJSON {
	e.t.Helper()
	var got struct {
		Server serverJSON `json:"server"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/servers/"+strconv.FormatInt(id, 10), nil, &got)
	return got.Server
}

func (e *testEnv) waitAction(id int64) actionJSON {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		var got struct {
			Action actionJSON `json:"action"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v1/actions/"+strconv.FormatInt(id, 10), nil, &got)
		if got.Action.Status != "running" {
			return got.Action
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("action %d is still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *testEnv) postServerAction(id int64, name string, body any) (int, actionJSON) {
	e.t.Helper()
	var got struct {
		Action actionJSON `json:"action"`
	}
	status := e.call(http.MethodPost, fmt.Sprintf("/v1/servers/%d/actions/%s", id, name), body, &got)
	return status, got.Action
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
