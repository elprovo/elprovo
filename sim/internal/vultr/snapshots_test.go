package vultr

import (
	"errors"
	"net/http"
	"testing"
)

func (e *testEnv) snapshot(id string) (int, snapshotJSON) {
	e.t.Helper()
	var got struct {
		Snapshot snapshotJSON `json:"snapshot"`
	}
	status := e.call(http.MethodGet, "/v2/snapshots/"+id, nil, &got)
	return status, got.Snapshot
}

func TestSnapshotCommitsTheMachineAndBootsNewInstances(t *testing.T) {
	e := newEnv(t)
	source := e.createInstance("build", nil)
	release := e.machines.hold("Commit")
	var created struct {
		Snapshot snapshotJSON `json:"snapshot"`
	}

	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/snapshots", map[string]string{"instance_id": source.ID, "description": "golden"}, &created)

	if created.Snapshot.Status != "pending" || created.Snapshot.Description != "golden" || created.Snapshot.OSID != 2284 {
		t.Fatalf("new snapshot = %+v", created.Snapshot)
	}
	if status := e.call(http.MethodPost, "/v2/instances", map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "snapshot_id": created.Snapshot.ID}, nil); status != http.StatusBadRequest {
		t.Fatalf("boot from a pending snapshot = %d, want 400", status)
	}
	release()
	eventually(t, "the snapshot is complete", func() bool {
		_, snap := e.snapshot(created.Snapshot.ID)
		return snap.Status == "complete" && snap.Size == 2_340_000_000
	})
	inst := e.createInstance("from-snapshot", map[string]any{"os_id": nil, "snapshot_id": created.Snapshot.ID})
	ref := e.machines.spec(inst.ID).Image
	if inst.SnapshotID != created.Snapshot.ID || ref == "" || !e.machines.hasImage(ref) {
		t.Fatalf("instance snapshot_id %q boots image %q", inst.SnapshotID, ref)
	}

	var listed struct {
		Snapshots []snapshotJSON `json:"snapshots"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/snapshots?description=golden", nil, &listed)
	if len(listed.Snapshots) != 1 || listed.Snapshots[0].ID != created.Snapshot.ID {
		t.Fatalf("snapshots described golden = %+v", listed.Snapshots)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/snapshots/"+created.Snapshot.ID, nil, nil)
	if e.machines.hasImage(ref) {
		t.Fatal("the snapshot's image survived its deletion")
	}
}

func TestFailedCommitRemovesTheSnapshot(t *testing.T) {
	e := newEnv(t)
	source := e.createInstance("build", nil)
	e.machines.fail("Commit", errors.New("disk full"))
	var created struct {
		Snapshot snapshotJSON `json:"snapshot"`
	}

	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/snapshots", map[string]string{"instance_id": source.ID}, &created)

	eventually(t, "the failed snapshot is gone", func() bool {
		status, _ := e.snapshot(created.Snapshot.ID)
		return status == http.StatusNotFound
	})
}
