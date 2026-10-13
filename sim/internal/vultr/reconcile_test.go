package vultr

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func TestRestartKeepsInstancesAndMatchesThemToTheirMachines(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	kept := first.createInstance("kept", nil)
	lost := first.createInstance("lost", nil)
	first.sim.close()

	_ = fake.Delete(t.Context(), "c-"+lost.ID)
	var accountID string
	first.sim.store.Read(func(a *account) { accountID = a.ID })
	orphan, _ := fake.Create(t.Context(), machines.Spec{Name: "orphan", Cloud: cloudLabel, Account: accountID, Resource: "orphan"})
	foreign, _ := fake.Create(t.Context(), machines.Spec{Name: "foreign", Cloud: cloudLabel, Account: "another-account", Resource: "foreign"})

	second := newTestEnv(t, statePath, fake)

	if got := second.instance(kept.ID); got.Status != "active" || got.PowerStatus != "running" || got.MainIP != kept.MainIP {
		t.Fatalf("kept instance = %+v", got)
	}
	if status := second.call(http.MethodGet, "/v2/instances/"+lost.ID, nil, nil); status != http.StatusNotFound {
		t.Fatalf("instance without a machine = %d, want 404", status)
	}
	if fake.hasMachine(orphan.ID) {
		t.Fatal("a machine of this account that no instance owns was kept")
	}
	if !fake.hasMachine(foreign.ID) {
		t.Fatal("a machine of another account was deleted")
	}
}

func TestRestartFinishesAnInterruptedDeletionAndDropsAPendingSnapshot(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	inst := first.createInstance("web-1", nil)
	err := first.sim.store.Update(func(a *account) error {
		a.instance(inst.ID).Destroying = true
		a.Snapshots = append(a.Snapshots, &snapshot{ID: "snap-1", Status: "pending", ImageRef: "elprovo-sim-vultr-snapshot:test"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first.sim.close()

	second := newTestEnv(t, statePath, fake)

	if status := second.call(http.MethodGet, "/v2/instances/"+inst.ID, nil, nil); status != http.StatusNotFound {
		t.Fatalf("instance whose deletion was interrupted = %d, want 404", status)
	}
	if fake.hasMachine("c-" + inst.ID) {
		t.Fatal("the machine of an interrupted deletion was kept")
	}
	if status, _ := second.snapshot("snap-1"); status != http.StatusNotFound {
		t.Fatalf("pending snapshot after restart = %d, want 404", status)
	}
}
