package azure

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func TestRestartKeepsVMsWithMachinesAndDropsTheRest(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	e := newTestEnv(t, statePath, fake)
	subnet := e.network("rg")
	kept := e.createVM("rg", "kept", e.nic("rg", "kept-nic", subnet, "", "").ID)
	lost := e.createVM("rg", "lost", e.nic("rg", "lost-nic", subnet, "", "").ID)
	e.server.Close()
	e.sim.close()
	fake.mu.Lock()
	delete(fake.machines, "c-"+lost.Properties.VMID)
	var account string
	e.sim.store.Read(func(sub *subscription) { account = sub.Account })
	fake.machines["c-orphan"] = &machines.Machine{ID: "c-orphan", Cloud: cloudLabel, Account: account, Resource: "orphan"}
	fake.mu.Unlock()

	again := newTestEnv(t, statePath, fake)

	again.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "kept")+apiVersion, nil, nil)
	again.mustFail(http.StatusNotFound, "ResourceNotFound", http.MethodGet, resourcePath("rg", typeVM, "lost")+apiVersion, nil)
	if !fake.has(kept.Properties.VMID) || fake.has("orphan") {
		t.Fatal("the restart kept the wrong machines")
	}
}

func TestRestartFailsAnInterruptedCreate(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	e := newTestEnv(t, statePath, fake)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")
	fake.fail("Create", errDocker)
	e.wait(e.mustCall(http.StatusCreated, http.MethodPut, resourcePath("rg", typeVM, "web")+apiVersion, vmBody(n.ID, ""), nil))
	e.server.Close()
	e.sim.close()

	again := newTestEnv(t, statePath, fake)

	var vm vmJSON
	again.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil, &vm)
	if vm.Properties.ProvisioningState != statusFailed {
		t.Fatalf("provisioning state = %s, want Failed", vm.Properties.ProvisioningState)
	}
}
