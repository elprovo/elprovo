package digitalocean

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
)

func TestPowerOffShowsProgressThenTurnsTheDropletOff(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	release := e.machines.hold("Stop")

	status, act := e.postAction(d.ID, map[string]any{"type": "power_off"})
	if status != http.StatusCreated || act.Status != "in-progress" || act.Type != "power_off" || act.ResourceID != d.ID || act.CompletedAt != nil {
		t.Fatalf("power_off = %d %+v", status, act)
	}
	var listed struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/droplets/"+strconv.FormatInt(d.ID, 10)+"/actions/"+strconv.FormatInt(act.ID, 10), nil, &listed)
	if listed.Action.Status != "in-progress" || !e.droplet(d.ID).Locked {
		t.Fatalf("while stopping: action %s, droplet locked %v", listed.Action.Status, e.droplet(d.ID).Locked)
	}
	var busy errorBody
	if status := e.call(http.MethodPost, "/v2/droplets/"+strconv.FormatInt(d.ID, 10)+"/actions", map[string]string{"type": "reboot"}, &busy); status != http.StatusUnprocessableEntity {
		t.Fatalf("second action while one runs = %d %+v, want 422", status, busy)
	}
	release()

	if done := e.waitAction(act.ID); done.Status != "completed" || done.CompletedAt == nil {
		t.Fatalf("power_off result = %+v", done)
	}
	if got := e.droplet(d.ID); got.Status != "off" || got.Locked {
		t.Fatalf("droplet = %s locked %v, want off and unlocked", got.Status, got.Locked)
	}
	var already errorBody
	if status := e.call(http.MethodPost, "/v2/droplets/"+strconv.FormatInt(d.ID, 10)+"/actions", map[string]string{"type": "power_off"}, &already); status != http.StatusUnprocessableEntity || already.Message != "Droplet is already powered off." {
		t.Fatalf("power_off of an off droplet = %d %+v", status, already)
	}
}

func TestPowerOnRebootAndPowerCycleLeaveTheDropletActive(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	for _, typ := range []string{"power_off", "power_on", "reboot", "power_cycle"} {
		_, act := e.postAction(d.ID, map[string]any{"type": typ})
		if done := e.waitAction(act.ID); done.Status != "completed" {
			t.Fatalf("%s = %s", typ, done.Status)
		}
	}
	if got := e.droplet(d.ID).Status; got != "active" {
		t.Fatalf("status = %s, want active", got)
	}
	var history struct {
		Actions []actionJSON `json:"actions"`
		Meta    meta         `json:"meta"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/droplets/"+strconv.FormatInt(d.ID, 10)+"/actions", nil, &history)
	if history.Meta.Total != 5 || history.Actions[0].Type != "power_cycle" || history.Actions[4].Type != "create" {
		t.Fatalf("droplet actions = %+v, want five, newest first", history.Actions)
	}
}

func TestRenameCompletesAtOnceAndResizeNeedsTheDropletOff(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")

	status, act := e.postAction(d.ID, map[string]any{"type": "rename", "name": "web-2"})
	if status != http.StatusCreated || act.Status != "completed" || e.droplet(d.ID).Name != "web-2" {
		t.Fatalf("rename = %d %+v, name %s", status, act, e.droplet(d.ID).Name)
	}
	if status, _ := e.postAction(d.ID, map[string]any{"type": "resize", "size": "s-2vcpu-4gb"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("resize of a running droplet = %d, want 422", status)
	}
	_, off := e.postAction(d.ID, map[string]any{"type": "power_off"})
	e.waitAction(off.ID)
	if status, act := e.postAction(d.ID, map[string]any{"type": "resize", "size": "s-2vcpu-4gb"}); status != http.StatusCreated || act.Status != "completed" {
		t.Fatalf("resize = %d %+v", status, act)
	}
	if got := e.droplet(d.ID); got.Size.Slug != "s-2vcpu-4gb" || got.Memory != 4096 {
		t.Fatalf("size after resize = %s %d", got.Size.Slug, got.Memory)
	}
}

func TestUnsupportedActionTypeIsRejected(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	var body errorBody
	if status := e.call(http.MethodPost, "/v2/droplets/"+strconv.FormatInt(d.ID, 10)+"/actions", map[string]string{"type": "rebuild"}, &body); status != http.StatusUnprocessableEntity {
		t.Fatalf("rebuild = %d %+v, want 422", status, body)
	}
}

func TestSnapshotBecomesAnImageThatNewDropletsBootFrom(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	_, act := e.postAction(d.ID, map[string]any{"type": "snapshot", "name": "golden"})
	e.waitAction(act.ID)

	var images struct {
		Images []imageJSON `json:"images"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/images?private=true", nil, &images)
	if len(images.Images) != 1 || images.Images[0].Name != "golden" || images.Images[0].Type != "snapshot" || images.Images[0].Public {
		t.Fatalf("private images = %+v", images.Images)
	}
	snap := images.Images[0]
	if snap.SizeGigabytes != 2.34 || snap.MinDiskSize != 25 || !slices.Equal(snap.Regions, []string{"nyc3"}) {
		t.Fatalf("snapshot image = %+v", snap)
	}
	if got := e.droplet(d.ID).SnapshotIDs; !slices.Equal(got, []int64{snap.ID}) {
		t.Fatalf("droplet snapshot_ids = %v", got)
	}
	var snapshots struct {
		Snapshots []snapshotJSON `json:"snapshots"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/snapshots?resource_type=droplet", nil, &snapshots)
	if len(snapshots.Snapshots) != 1 || snapshots.Snapshots[0].ID != strconv.FormatInt(snap.ID, 10) || snapshots.Snapshots[0].ResourceID != strconv.FormatInt(d.ID, 10) {
		t.Fatalf("snapshots = %+v", snapshots.Snapshots)
	}

	var clone struct {
		Droplet dropletJSON `json:"droplet"`
		Links   links       `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{"name": "clone", "region": "nyc3", "size": "s-1vcpu-1gb", "image": snap.ID}, &clone)
	e.waitAction(clone.Links.Actions[0].ID)
	ref := e.machines.spec(clone.Droplet.ID).Image
	if ref == "" || !e.machines.hasImage(ref) {
		t.Fatalf("clone booted image %q, want the committed snapshot", ref)
	}
	if status := e.call(http.MethodPost, "/v2/droplets", map[string]any{"name": "far", "region": "fra1", "size": "s-1vcpu-1gb", "image": snap.ID}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("create from a snapshot in another region = %d, want 422", status)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/images/"+strconv.FormatInt(snap.ID, 10), nil, nil)
	if e.machines.hasImage(ref) {
		t.Fatal("deleting the snapshot left its image")
	}
}

func TestTagActionRunsOnEveryTaggedDroplet(t *testing.T) {
	e := newEnv(t)
	a := e.createDroplet("web-1", "web")
	b := e.createDroplet("web-2", "web")
	var got struct {
		Actions []actionJSON `json:"actions"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/droplets/actions?tag_name=web", map[string]string{"type": "power_off"}, &got)
	if len(got.Actions) != 2 {
		t.Fatalf("actions = %+v", got.Actions)
	}
	for _, act := range got.Actions {
		e.waitAction(act.ID)
	}
	if e.droplet(a.ID).Status != "off" || e.droplet(b.ID).Status != "off" {
		t.Fatal("tagged droplets are not off")
	}
	if status := e.call(http.MethodPost, "/v2/droplets/actions?tag_name=web", map[string]string{"type": "rename"}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("rename by tag = %d, want 422", status)
	}
}

func TestFailedPowerOnLeavesTheStatusTheMachineHas(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	_, off := e.postAction(d.ID, map[string]any{"type": "power_off"})
	e.waitAction(off.ID)
	e.machines.fail("Start", errors.New("docker start failed"))

	_, on := e.postAction(d.ID, map[string]any{"type": "power_on"})

	if got := e.waitAction(on.ID); got.Status != "errored" {
		t.Fatalf("power_on = %s, want errored", got.Status)
	}
	if got := e.droplet(d.ID); got.Status != "off" || got.Locked {
		t.Fatalf("droplet = %s locked %v, want off and unlocked", got.Status, got.Locked)
	}
}
