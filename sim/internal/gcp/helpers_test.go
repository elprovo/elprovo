package gcp

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	testToken   = "ya29.unit"
	testProject = "elprovo-unit"
	testZone    = "us-central1-a"
	testTimeout = 5 * time.Second
	agentKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY agent"
	computePath = "/compute/v1/projects/" + testProject
)

type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]*machines.Machine
	specs     map[string]machines.Spec
	firewalls map[string][]*machines.Firewall
	addresses map[string][]string
	images    map[string]bool
	removed   []string
	gates     map[string]chan struct{}
	failures  map[string]error
	nextIP    int
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{
		machines: map[string]*machines.Machine{}, specs: map[string]machines.Spec{},
		firewalls: map[string][]*machines.Firewall{}, addresses: map[string][]string{}, images: map[string]bool{},
		gates: map[string]chan struct{}{}, failures: map[string]error{},
	}
}

// fail makes the next call of op fail with err.
func (f *fakeMachines) fail(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[op] = err
}

// hold makes the next calls of op wait until release is called.
func (f *fakeMachines) hold(op string) (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.gates[op] = gate
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func (f *fakeMachines) enter(op string) error {
	f.mu.Lock()
	gate, err := f.gates[op], f.failures[op]
	delete(f.failures, op)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[id] = append([]string{}, addrs...)
	return nil
}

func (f *fakeMachines) Commit(_ context.Context, _ string, ref string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = true
	return 2_340_000_000, nil
}

func (f *fakeMachines) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, ref)
	f.removed = append(f.removed, ref)
	return nil
}

func (f *fakeMachines) Networks(context.Context) (machines.Networks, error) {
	return machines.Networks{
		Private: machines.Subnet{Prefix: netip.MustParsePrefix("172.30.0.0/16"), Gateway: netip.MustParseAddr("172.30.0.1")},
		Public:  machines.Subnet{Prefix: netip.MustParsePrefix("172.31.0.0/16"), Gateway: netip.MustParseAddr("172.31.0.1")},
	}, nil
}

func (f *fakeMachines) machineOf(instanceID string) (machines.Machine, machines.Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines["c-"+instanceID]
	if !ok {
		return machines.Machine{}, f.specs["c-"+instanceID], false
	}
	return *m, f.specs["c-"+instanceID], true
}

func (f *fakeMachines) add(m machines.Machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machines[m.ID] = &m
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
	dataDir  string
}

func testConfig() config {
	return config{
		project: testProject, accessTokens: []string{testToken}, zones: []string{testZone, "us-central1-b", "europe-west1-b"},
		network: "elprovo-sim-unit", tokenURI: "http://sim.test/token",
	}
}

func newTestEnv(t *testing.T, dataDir string, fake *fakeMachines) *testEnv {
	t.Helper()
	sim, err := newSimulation(testConfig(), dataDir, fake, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newSimulation: %v", err)
	}
	if err := sim.writeFiles(); err != nil {
		t.Fatalf("writeFiles: %v", err)
	}
	if err := sim.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { sim.syncMachines(sim.bg) })
	server := httptest.NewServer(sim.handler())
	t.Cleanup(func() {
		server.Close()
		sim.close()
		wg.Wait()
	})
	return &testEnv{t: t, sim: sim, machines: fake, server: server, dataDir: dataDir}
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv(t, filepath.Join(t.TempDir(), "gcp"), newFakeMachines())
}

func (e *testEnv) do(method, path, token string, body io.Reader, contentType string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.server.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (e *testEnv) call(method, path string, body, out any) int {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	status, data := e.do(method, path, testToken, reader, "application/json")
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

type googleError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
		Status string `json:"status"`
	} `json:"error"`
}

// fail sends a request that must fail and returns its error.
func (e *testEnv) fail(want int, method, path string, body any) googleError {
	e.t.Helper()
	var got googleError
	if status := e.call(method, path, body, &got); status != want {
		e.t.Fatalf("%s %s = %d, want %d: %+v", method, path, status, want, got)
	}
	return got
}

// wait waits for the operation through its wait method until it is DONE.
func (e *testEnv) wait(op operationJSON) operationJSON {
	e.t.Helper()
	path := strings.TrimPrefix(op.SelfLink, computeBase) + "/wait"
	deadline := time.Now().Add(testTimeout)
	for op.Status != "DONE" {
		if time.Now().After(deadline) {
			e.t.Fatalf("operation %s is still %s", op.Name, op.Status)
		}
		e.mustCall(http.StatusOK, http.MethodPost, "/compute/v1/"+path, nil, &op)
	}
	return op
}

// insert posts a resource and waits until its operation is DONE without an
// error.
func (e *testEnv) insert(path string, body any) operationJSON {
	e.t.Helper()
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodPost, path, body, &op)
	op = e.wait(op)
	if op.Error != nil {
		e.t.Fatalf("POST %s: operation failed: %+v", path, op.Error)
	}
	return op
}

func (e *testEnv) remove(path string) operationJSON {
	e.t.Helper()
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodDelete, path, nil, &op)
	op = e.wait(op)
	if op.Error != nil {
		e.t.Fatalf("DELETE %s: operation failed: %+v", path, op.Error)
	}
	return op
}

func instanceBody(name string, extra map[string]any) map[string]any {
	body := map[string]any{
		"name":        name,
		"machineType": "zones/" + testZone + "/machineTypes/e2-medium",
		"disks": []map[string]any{{
			"boot": true, "autoDelete": true,
			"initializeParams": map[string]any{"sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"},
		}},
		"networkInterfaces": []map[string]any{{"network": "global/networks/default", "accessConfigs": []map[string]any{{"type": "ONE_TO_ONE_NAT"}}}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (e *testEnv) createInstance(name string, extra map[string]any) instanceJSON {
	e.t.Helper()
	e.insert(computePath+"/zones/"+testZone+"/instances", instanceBody(name, extra))
	return e.instance(name)
}

func (e *testEnv) instance(name string) instanceJSON {
	e.t.Helper()
	var got instanceJSON
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/zones/"+testZone+"/instances/"+name, nil, &got)
	return got
}

func (e *testEnv) account() string {
	var account string
	e.sim.store.Read(func(w *world) { account = w.Account })
	return account
}

func (e *testEnv) diskID(name string) string {
	e.t.Helper()
	var got diskJSON
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/zones/"+testZone+"/disks/"+name, nil, &got)
	return got.ID
}

// merge returns base with the entries of changes.
func merge(base, changes map[string]any) map[string]any {
	for k, v := range changes {
		base[k] = v
	}
	return base
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
