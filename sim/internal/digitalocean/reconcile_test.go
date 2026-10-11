package digitalocean

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func TestRestartKeepsDropletsAndMatchesThemToTheirMachines(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	kept := first.createDroplet("kept")
	stopped := first.createDroplet("stopped")
	lost := first.createDroplet("lost")
	first.sim.close()

	_ = fake.Stop(t.Context(), "c-"+itoa(stopped.ID), true)
	_ = fake.Delete(t.Context(), "c-"+itoa(lost.ID))
	var accountID string
	first.sim.store.Read(func(a *account) { accountID = a.UUID })
	orphan, _ := fake.Create(t.Context(), machines.Spec{Name: "orphan", Cloud: cloudLabel, Account: accountID, Resource: "999"})
	foreign, _ := fake.Create(t.Context(), machines.Spec{Name: "foreign", Cloud: cloudLabel, Account: "another-account", Resource: "998"})

	second := newTestEnv(t, statePath, fake)

	if got := second.droplet(kept.ID); got.Status != "active" || got.Networks.V4[0].IPAddress != kept.Networks.V4[0].IPAddress {
		t.Fatalf("kept droplet = %s %+v", got.Status, got.Networks)
	}
	if got := second.droplet(stopped.ID); got.Status != "off" {
		t.Fatalf("stopped droplet = %s, want off", got.Status)
	}
	if status := second.call(http.MethodGet, "/v2/droplets/"+itoa(lost.ID), nil, nil); status != http.StatusNotFound {
		t.Fatalf("droplet without a machine = %d, want 404", status)
	}
	if fake.hasMachineID(orphan.ID) {
		t.Fatal("a machine of this account that no droplet owns was kept")
	}
	if !fake.hasMachineID(foreign.ID) {
		t.Fatal("a machine of another account was deleted")
	}
}

func TestActionRunningAtShutdownIsErroredAfterRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	d := first.createDroplet("web-1")
	var act actionJSON
	err := first.sim.store.Update(func(a *account) error {
		act = first.sim.renderAction(first.sim.newAction(a, "reboot", a.droplet(d.ID)))
		a.droplet(d.ID).Locked = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first.sim.close()

	second := newTestEnv(t, statePath, fake)

	if got := second.waitAction(act.ID); got.Status != "errored" || got.CompletedAt == nil {
		t.Fatalf("action after restart = %+v, want errored", got)
	}
	if second.droplet(d.ID).Locked {
		t.Fatal("droplet is still locked after restart")
	}
}

func (f *fakeMachines) hasMachineID(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines[id]
	return ok
}

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
