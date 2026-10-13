package gcp

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

// TestImageFromAKeptBootDiskBootsNewInstances follows what Packer does: it
// keeps the boot disk, deletes the instance, makes an image of the disk and
// deletes the disk.
func TestImageFromAKeptBootDiskBootsNewInstances(t *testing.T) {
	e := newEnv(t)
	build := e.createInstance("build", map[string]any{"disks": []map[string]any{{
		"boot": true, "autoDelete": false,
		"initializeParams": map[string]any{"diskName": "build-disk", "diskSizeGb": "20", "sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"},
	}}})
	e.remove(computePath + "/zones/" + testZone + "/instances/build")
	diskRef := "elprovo-sim-gcp-disk:" + e.account()[:8] + "-" + e.diskID("build-disk")
	if !e.machines.hasImage(diskRef) {
		t.Fatalf("the kept disk was not saved as %s", diskRef)
	}
	e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/global/images/elprovo-1/deprecate", map[string]any{"state": "WRONG"})

	e.insert(computePath+"/global/images", map[string]any{
		"name": "elprovo-1", "family": "elprovo", "sourceType": "RAW",
		"sourceDisk": "/compute/v1/projects/" + testProject + "/zones/" + testZone + "/disks/build-disk",
	})
	e.insert(computePath+"/global/images/elprovo-1/deprecate", map[string]any{})
	e.remove(computePath + "/zones/" + testZone + "/disks/build-disk")

	var img imageJSON
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/global/images/family/elprovo", nil, &img)
	if img.Name != "elprovo-1" || img.DiskSizeGb != "20" || img.Status != "READY" {
		t.Fatalf("image = %+v", img)
	}
	if !e.machines.hasImage(diskRef) {
		t.Fatal("deleting the disk removed the image's content")
	}
	booted := e.createInstance("from-image", map[string]any{"disks": []map[string]any{{
		"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "global/images/elprovo-1"},
	}}})
	if _, spec, _ := e.machines.machineOf(booted.ID); spec.Image != diskRef {
		t.Fatalf("instance from the image boots %q, want %q", spec.Image, diskRef)
	}
	if booted.ID == build.ID {
		t.Fatal("the new instance reused the build instance's id")
	}
	e.remove(computePath + "/zones/" + testZone + "/instances/from-image")
	e.remove(computePath + "/global/images/elprovo-1")
	eventually(t, "the unused image content is removed", func() bool { return !e.machines.hasImage(diskRef) })
}

func TestImageNeedsADetachedDisk(t *testing.T) {
	e := newEnv(t)
	e.createInstance("build", nil)

	got := e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/global/images", map[string]any{
		"name": "elprovo-1", "sourceDisk": "zones/" + testZone + "/disks/build",
	})

	if got.Error.Errors[0].Reason != "resourceInUseByAnotherResource" {
		t.Fatalf("image of an attached disk = %+v", got)
	}
}

func TestPublicImagesResolveByNameAndFamily(t *testing.T) {
	e := newEnv(t)
	var byFamily, byName imageJSON

	e.mustCall(http.StatusOK, http.MethodGet, "/compute/v1/projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64", nil, &byFamily)
	e.mustCall(http.StatusOK, http.MethodGet, "/compute/v1/projects/ubuntu-os-cloud/global/images/"+byFamily.Name, nil, &byName)

	if byFamily.Name != "ubuntu-2404-noble-amd64-v20260910" || byName.SelfLink != byFamily.SelfLink || byName.Architecture != "X86_64" {
		t.Fatalf("image by family = %+v, by name = %+v", byFamily, byName)
	}
	e.fail(http.StatusNotFound, http.MethodGet, "/compute/v1/projects/ubuntu-os-cloud/global/images/ubuntu-2404-lts-amd64", nil)
}

func TestReconcileRemovesInstancesWithoutMachinesAndMachinesWithoutInstances(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gcp")
	fake := newFakeMachines()
	first := newTestEnv(t, dir, fake)
	kept := first.createInstance("kept", nil)
	lost := first.createInstance("lost", nil)
	account := first.account()
	_ = fake.Delete(t.Context(), "c-"+lost.ID)
	fake.add(machines.Machine{ID: "c-orphan", Cloud: "gcp", Account: account, Resource: "123", Running: true})

	second := newTestEnv(t, dir, fake)

	if got := second.instance("kept"); got.Status != "RUNNING" || got.ID != kept.ID {
		t.Fatalf("kept instance = %+v", got)
	}
	second.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/instances/lost", nil)
	second.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/disks/lost", nil)
	if _, _, ok := fake.machineOf("orphan"); ok {
		t.Fatal("the machine that no instance owns is still there")
	}
}

func TestKeptDiskIsNotSavedWhenItsMachineStays(t *testing.T) {
	e := newEnv(t)
	e.createInstance("build", map[string]any{"disks": []map[string]any{{
		"boot": true, "autoDelete": false, "initializeParams": map[string]any{"sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"},
	}}})
	diskRef := "elprovo-sim-gcp-disk:" + e.account()[:8] + "-" + e.diskID("build")
	e.machines.fail("Delete", errors.New("docker rm: device busy"))
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodDelete, computePath+"/zones/"+testZone+"/instances/build", nil, &op)

	if op = e.wait(op); op.Error == nil {
		t.Fatalf("delete operation = %+v, want an error", op)
	}

	if e.machines.hasImage(diskRef) {
		t.Fatal("the disk's content was saved although its machine still runs")
	}
}

func TestDeletedDefaultNetworkStaysDeletedAcrossARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gcp")
	first := newTestEnv(t, dir, newFakeMachines())
	for _, rule := range []string{"default-allow-internal", "default-allow-ssh", "default-allow-rdp", "default-allow-icmp"} {
		first.remove(computePath + "/global/firewalls/" + rule)
	}
	first.remove(computePath + "/global/networks/default")

	second := newTestEnv(t, dir, newFakeMachines())

	second.fail(http.StatusNotFound, http.MethodGet, computePath+"/global/networks/default", nil)
}
