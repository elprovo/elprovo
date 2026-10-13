package hetzner

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func TestRestartKeepsServersWhoseMachinesExist(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	kept := first.createServer(serverRequest("kept", nil))
	lost := first.createServer(serverRequest("lost", nil))
	if err := fake.Delete(context.Background(), "c-"+strconv.FormatInt(lost.ID, 10)); err != nil {
		t.Fatal(err)
	}
	var projectID string
	first.sim.store.Read(func(p *project) { projectID = p.UUID })
	if _, err := fake.Create(context.Background(), machines.Spec{Resource: "999", Cloud: cloudLabel, Account: projectID}); err != nil {
		t.Fatal(err)
	}

	second := newTestEnv(t, statePath, fake)

	if got := second.serverByID(kept.ID); got.Status != "running" || got.PublicNet.IPv4.IP != kept.PublicNet.IPv4.IP {
		t.Fatalf("kept server after restart = %+v", got)
	}
	if status := second.call(http.MethodGet, fmt.Sprintf("/v1/servers/%d", lost.ID), nil, nil); status != http.StatusNotFound {
		t.Fatalf("server whose machine is gone = %d, want 404", status)
	}
	if _, err := fake.Inspect(context.Background(), "c-999"); err == nil {
		t.Fatal("a machine that no server owns survived the restart")
	}
}

func TestRestartFailsActionsThatWereRunning(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fake := newFakeMachines()
	first := newTestEnv(t, statePath, fake)
	srv := first.createServer(serverRequest("web-1", nil))
	err := first.sim.store.Update(func(p *project) error {
		first.sim.startAction(p, "reboot_server", resourceRef{ID: srv.ID, Type: "server"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	second := newTestEnv(t, statePath, fake)

	var got struct {
		Actions []actionJSON `json:"actions"`
	}
	second.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/servers/%d/actions?status=running", srv.ID), nil, &got)
	if len(got.Actions) != 0 {
		t.Fatalf("running actions after restart = %+v, want none", got.Actions)
	}
	if status, _ := second.postServerAction(srv.ID, "reboot", nil); status != http.StatusCreated {
		t.Fatalf("reboot after restart = %d, want the server unlocked", status)
	}
}
