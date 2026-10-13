package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

var errDocker = errors.New("docker failed")

const (
	waitLimit  = 5 * time.Second
	testSecret = "unit-secret"
	testKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY operator"
	apiVersion = "?api-version=2024-03-01"
)

type execCall struct {
	machine string
	stdin   string
	args    []string
}

type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]*machines.Machine
	specs     map[string]machines.Spec
	firewalls map[string][]*machines.Firewall
	addresses map[string][]string
	images    map[string]string
	execs     []execCall
	gates     map[string]chan struct{}
	failNext  map[string]error
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{
		machines: map[string]*machines.Machine{}, specs: map[string]machines.Spec{}, firewalls: map[string][]*machines.Firewall{},
		addresses: map[string][]string{}, images: map[string]string{}, gates: map[string]chan struct{}{}, failNext: map[string]error{},
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
	m := &machines.Machine{
		ID: "c-" + spec.Resource, Name: spec.Name, Cloud: spec.Cloud, Account: spec.Account, Resource: spec.Resource, Running: true,
		PrivateIP: spec.PrivateIP, PublicIP: fmt.Sprintf("172.31.255.%d", 250-len(f.machines)),
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

func (f *fakeMachines) Exec(_ context.Context, id string, stdin io.Reader, args ...string) ([]byte, error) {
	if err := f.enter("Exec"); err != nil {
		return nil, err
	}
	input, _ := io.ReadAll(stdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, execCall{id, string(input), args})
	return nil, nil
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

func (f *fakeMachines) Commit(_ context.Context, id, ref string) (int64, error) {
	if err := f.enter("Commit"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = id
	return 2_000_000_000, nil
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

func (f *fakeMachines) specOf(vmID string) machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs["c-"+vmID]
}

func (f *fakeMachines) has(vmID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines["c-"+vmID]
	return ok
}

func (f *fakeMachines) lastFirewall(vmID string) *machines.Firewall {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.firewalls["c-"+vmID]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

func (f *fakeMachines) addressesOf(vmID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses["c-"+vmID]
}

func (f *fakeMachines) imageSource(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[ref]
}

func (f *fakeMachines) execsOf(vmID string) []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []execCall
	for _, e := range f.execs {
		if e.machine == "c-"+vmID {
			out = append(out, e)
		}
	}
	return out
}

type testEnv struct {
	t        *testing.T
	sim      *Simulation
	machines *fakeMachines
	server   *httptest.Server
	token    string
}

func testConfig() config {
	return config{
		tenant: defaultTenant, subscription: defaultSubscription, clientID: defaultClientID, clientSecret: testSecret,
		locations: []string{"westeurope", "eastus"}, network: "elprovo-sim-unit", registry: registryConfig{domain: "azurecr.io", username: "registry-user", password: "registry-secret"},
	}
}

func newTestEnv(t *testing.T, statePath string, fake *fakeMachines) *testEnv {
	t.Helper()
	sim, err := newSimulation(testConfig(), statePath, fake, slog.New(slog.DiscardHandler))
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
	e := &testEnv{t: t, sim: sim, machines: fake, server: server}
	e.token = e.issue(server.URL + "/.default")
	return e
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv(t, filepath.Join(t.TempDir(), "state.json"), newFakeMachines())
}

// issue gets an access token for scope from the token endpoint.
func (e *testEnv) issue(scope string) string {
	e.t.Helper()
	status, body := e.tokenRequest(url.Values{"grant_type": {"client_credentials"}, "client_id": {defaultClientID}, "client_secret": {testSecret}, "scope": {scope}})
	if status != http.StatusOK {
		e.t.Fatalf("token endpoint = %d: %v", status, body)
	}
	return body["access_token"].(string)
}

func (e *testEnv) tokenRequest(form url.Values) (int, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.server.URL+"/"+defaultTenant+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// call sends an authenticated request and decodes the answer into out.
func (e *testEnv) call(method, path string, body, out any) (int, http.Header) {
	e.t.Helper()
	status, headers, data := e.do(method, path, e.token, body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return status, headers
}

func (e *testEnv) mustCall(want int, method, path string, body, out any) http.Header {
	e.t.Helper()
	var raw json.RawMessage
	got, headers := e.call(method, path, body, &raw)
	if got != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return headers
}

// armErrorBody is the error a request failed with.
type armErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *testEnv) mustFail(wantStatus int, wantCode, method, path string, body any) armErrorBody {
	e.t.Helper()
	var got armErrorBody
	status, _ := e.call(method, path, body, &got)
	if status != wantStatus || got.Error.Code != wantCode {
		e.t.Fatalf("%s %s = %d %s (%s), want %d %s", method, path, status, got.Error.Code, got.Error.Message, wantStatus, wantCode)
	}
	return got
}

// wait polls the operation that a response's Azure-AsyncOperation names
// until it ends, and returns its status.
func (e *testEnv) wait(headers http.Header) string {
	e.t.Helper()
	link := headers.Get("Azure-AsyncOperation")
	if link == "" {
		e.t.Fatal("the response names no Azure-AsyncOperation")
	}
	u, err := url.Parse(link)
	if err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(waitLimit)
	for {
		var op struct {
			Status string `json:"status"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, u.RequestURI(), nil, &op)
		if op.Status != statusInProgress {
			return op.Status
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("operation %s is still in progress", link)
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

func groupPath(group string) string {
	return "/subscriptions/" + defaultSubscription + "/resourceGroups/" + group
}

func resourcePath(group, typ, name string) string {
	return groupPath(group) + "/providers/" + typ + "/" + name
}

// network makes a resource group with a virtual network and the subnet
// previews, and returns the subnet's id.
func (e *testEnv) network(group string) string {
	e.t.Helper()
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath(group)+apiVersion, map[string]any{"location": "westeurope"}, nil)
	e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typeVNet, "net")+apiVersion, map[string]any{
		"location":   "westeurope",
		"properties": map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"10.42.0.0/16"}}},
	}, nil)
	e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typeVNet, "net")+"/subnets/previews"+apiVersion, map[string]any{
		"properties": map[string]any{"addressPrefix": "10.42.1.0/24"},
	}, nil)
	return resourcePath(group, typeVNet, "net") + "/subnets/previews"
}

// nic makes a network interface in the subnet, with a public IP when
// publicIP is not empty, and returns the interface as the API renders it.
func (e *testEnv) nic(group, name, subnetID, publicIP, nsgID string) nicJSON {
	e.t.Helper()
	cfg := map[string]any{"subnet": map[string]string{"id": subnetID}, "privateIPAllocationMethod": "Dynamic"}
	if publicIP != "" {
		cfg["publicIPAddress"] = map[string]string{"id": publicIP}
	}
	props := map[string]any{"ipConfigurations": []any{map[string]any{"name": "primary", "properties": cfg}}}
	if nsgID != "" {
		props["networkSecurityGroup"] = map[string]string{"id": nsgID}
	}
	var out nicJSON
	e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typeNIC, name)+apiVersion, map[string]any{"location": "westeurope", "properties": props}, &out)
	return out
}

type nicJSON struct {
	ID         string        `json:"id"`
	Properties nicProperties `json:"properties"`
}

func (n nicJSON) privateIP() string {
	return n.Properties.IPConfigurations[0].Properties.PrivateIPAddress
}

func (e *testEnv) publicIP(group, name string) (id, address string) {
	e.t.Helper()
	var out struct {
		ID         string             `json:"id"`
		Properties publicIPProperties `json:"properties"`
	}
	e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typePublicIP, name)+apiVersion, map[string]any{
		"location": "westeurope", "sku": map[string]string{"name": "Standard"}, "properties": map[string]any{"publicIPAllocationMethod": "Static"},
	}, &out)
	return out.ID, out.Properties.IPAddress
}

func (e *testEnv) nsg(group, name string, rules ...map[string]any) string {
	e.t.Helper()
	list := []any{}
	for _, r := range rules {
		list = append(list, map[string]any{"name": r["name"], "properties": r})
	}
	var out struct {
		ID string `json:"id"`
	}
	e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typeNSG, name)+apiVersion, map[string]any{
		"location": "westeurope", "properties": map[string]any{"securityRules": list},
	}, &out)
	return out.ID
}

func rule(name string, priority int, direction, access, protocol, port string, sources ...string) map[string]any {
	return map[string]any{
		"name": name, "priority": priority, "direction": direction, "access": access, "protocol": protocol,
		"sourcePortRange": "*", "destinationPortRange": port, "sourceAddressPrefixes": sources, "destinationAddressPrefix": "*",
	}
}

// vmBody is a virtual machine create request with the NIC.
func vmBody(nicID string, customData string) map[string]any {
	return map[string]any{
		"location": "westeurope",
		"properties": map[string]any{
			"hardwareProfile": map[string]string{"vmSize": "Standard_B2s"},
			"storageProfile": map[string]any{
				"imageReference": map[string]string{"publisher": "Canonical", "offer": "ubuntu-24_04-lts", "sku": "server", "version": "latest"},
				"osDisk":         map[string]any{"createOption": "FromImage", "deleteOption": "Delete", "managedDisk": map[string]string{"storageAccountType": "StandardSSD_LRS"}},
			},
			"osProfile": map[string]any{
				"computerName": "web", "adminUsername": "elprovo", "customData": base64.StdEncoding.EncodeToString([]byte(customData)),
				"linuxConfiguration": map[string]any{
					"disablePasswordAuthentication": true,
					"ssh":                           map[string]any{"publicKeys": []any{map[string]string{"path": "/home/elprovo/.ssh/authorized_keys", "keyData": testKey}}},
				},
			},
			"networkProfile": map[string]any{"networkInterfaces": []any{map[string]any{"id": nicID, "properties": map[string]any{"deleteOption": "Delete"}}}},
		},
	}
}

type vmJSON struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Properties vmProperties `json:"properties"`
}

// createVM creates the virtual machine and waits until it is provisioned.
func (e *testEnv) createVM(group, name, nicID string) vmJSON {
	e.t.Helper()
	var out vmJSON
	headers := e.mustCall(http.StatusCreated, http.MethodPut, resourcePath(group, typeVM, name)+apiVersion, vmBody(nicID, "#cloud-config\n"), &out)
	if got := e.wait(headers); got != statusSucceeded {
		e.t.Fatalf("create %s ended %s", name, got)
	}
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath(group, typeVM, name)+apiVersion, nil, &out)
	return out
}
