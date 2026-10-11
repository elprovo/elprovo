package aws

import (
	"path/filepath"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func TestRestartedSimulationTakesInstanceStatesFromTheirMachines(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	kept, stopped, lost := first.runInstance(), first.runInstance(), first.runInstance()
	for _, id := range []string{kept, stopped, lost} {
		first.waitState(id, "running")
	}
	first.server.Close()
	first.sim.close()

	if _, err := fake.setRunning("c-"+stopped, false); err != nil {
		t.Fatal(err)
	}
	if err := fake.Delete(t.Context(), "c-"+lost); err != nil {
		t.Fatal(err)
	}
	orphan, err := fake.Create(t.Context(), machines.Spec{Name: "orphan", Cloud: "aws", Account: fake.spec(kept).Account, Resource: "i-0000000000000000f"})
	if err != nil {
		t.Fatal(err)
	}
	second := newTestEnv(t, statePath, fake)

	if got := second.instance(kept).State; got != "running" {
		t.Errorf("kept instance = %s, want running", got)
	}
	if got := second.instance(stopped).State; got != "stopped" {
		t.Errorf("instance whose machine stopped = %s, want stopped", got)
	}
	if got := second.instance(lost); got.State != "terminated" || got.StateReason != "Server.InternalError" {
		t.Errorf("instance whose machine is gone = %+v, want terminated", got)
	}
	if _, err := fake.Inspect(t.Context(), orphan.ID); err == nil {
		t.Error("a machine that no instance owns was not deleted")
	}
}
