package hetzner

import (
	"fmt"
	"net/http"
	"testing"
)

func TestPowerActionsMoveTheServerBetweenOffAndRunning(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", nil))

	status, off := e.postServerAction(srv.ID, "shutdown", nil)
	if status != http.StatusCreated || off.Command != "shutdown_server" {
		t.Fatalf("shutdown = %d %+v", status, off)
	}
	e.waitAction(off.ID)
	if got := e.serverByID(srv.ID).Status; got != "off" {
		t.Fatalf("status after shutdown = %s, want off", got)
	}

	_, on := e.postServerAction(srv.ID, "poweron", map[string]any{})
	if on.Command != "start_server" {
		t.Fatalf("poweron command = %s, want start_server", on.Command)
	}
	e.waitAction(on.ID)
	if got := e.serverByID(srv.ID).Status; got != "running" {
		t.Fatalf("status after poweron = %s, want running", got)
	}

	_, reboot := e.postServerAction(srv.ID, "reboot", nil)
	if got := e.waitAction(reboot.ID); got.Status != "success" || got.Command != "reboot_server" {
		t.Fatalf("reboot = %+v", got)
	}
}

func TestUnsupportedServerActionFailsLoudly(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", nil))

	status, code := e.errorCode(http.MethodPost, fmt.Sprintf("/v1/servers/%d/actions/rebuild", srv.ID), map[string]any{"image": "ubuntu-24.04"})

	if status != http.StatusUnprocessableEntity || code != "unsupported_error" {
		t.Fatalf("rebuild = %d %s, want 422 unsupported_error", status, code)
	}
}

func TestSnapshotBootsNewServers(t *testing.T) {
	e := newEnv(t)
	source := e.createServer(serverRequest("build", nil))
	var created struct {
		Image  imageJSON  `json:"image"`
		Action actionJSON `json:"action"`
	}

	e.mustCall(http.StatusCreated, http.MethodPost, fmt.Sprintf("/v1/servers/%d/actions/create_image", source.ID),
		map[string]any{"type": "snapshot", "description": "stagely-golden", "labels": map[string]string{"stagely": "image"}}, &created)

	if created.Image.Status != "creating" || created.Image.Type != "snapshot" || created.Action.Command != "create_image" {
		t.Fatalf("create_image answer = %+v %+v", created.Image, created.Action)
	}
	if got := e.waitAction(created.Action.ID); got.Status != "success" {
		t.Fatalf("create_image action = %+v", got)
	}
	var listed struct {
		Images []imageJSON `json:"images"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/images?type=snapshot&architecture=x86", nil, &listed)
	if len(listed.Images) != 1 || listed.Images[0].Description != "stagely-golden" || listed.Images[0].Status != "available" ||
		listed.Images[0].CreatedFrom == nil || listed.Images[0].CreatedFrom.ID != source.ID {
		t.Fatalf("snapshots = %+v", listed.Images)
	}

	req := serverRequest("from-snapshot", nil)
	req["image"] = created.Image.ID
	clone := e.createServer(req)

	ref := e.machines.spec(clone.ID).Image
	if ref == "" || !e.machines.hasImage(ref) || clone.Image == nil || clone.Image.ID != created.Image.ID {
		t.Fatalf("clone boots image %q (committed: %v), reports %+v; want the snapshot", ref, e.machines.hasImage(ref), clone.Image)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/v1/images/%d", created.Image.ID), nil, nil)
	if e.machines.hasImage(ref) {
		t.Fatal("the snapshot's machine image is still there after the snapshot was deleted")
	}
}

func TestSnapshotBeingTakenCanNotBeDeleted(t *testing.T) {
	e := newEnv(t)
	source := e.createServer(serverRequest("build", nil))
	release := e.machines.hold("Commit")
	defer release()
	var created struct {
		Image imageJSON `json:"image"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, fmt.Sprintf("/v1/servers/%d/actions/create_image", source.ID), map[string]any{"type": "snapshot"}, &created)

	status, code := e.errorCode(http.MethodDelete, fmt.Sprintf("/v1/images/%d", created.Image.ID), nil)

	if status != http.StatusLocked || code != "locked" {
		t.Fatalf("delete a snapshot while it is taken = %d %s, want 423 locked, so its machine image is not left behind", status, code)
	}
}

func TestSystemImagesCanNotBeDeleted(t *testing.T) {
	e := newEnv(t)

	status, code := e.errorCode(http.MethodDelete, "/v1/images/100000001", nil)

	if status != http.StatusForbidden || code != "forbidden" {
		t.Fatalf("delete ubuntu-24.04 = %d %s, want 403 forbidden", status, code)
	}
}

func TestSystemImageIsFoundByNameForEachArchitecture(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Images []imageJSON `json:"images"`
	}

	e.mustCall(http.StatusOK, http.MethodGet, "/v1/images?name=ubuntu-24.04&architecture=arm", nil, &got)

	if len(got.Images) != 1 || got.Images[0].Architecture != "arm" || got.Images[0].Type != "system" {
		t.Fatalf("ubuntu-24.04 for arm = %+v", got.Images)
	}
}
