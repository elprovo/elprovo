package aws

import (
	"testing"
)

type testImage struct {
	ID         string `xml:"imageId"`
	Name       string `xml:"name"`
	State      string `xml:"imageState"`
	Owner      string `xml:"imageOwnerId"`
	SnapshotID string `xml:"blockDeviceMapping>item>ebs>snapshotId"`
}

func (e *testEnv) image(id string) testImage {
	e.t.Helper()
	var got struct {
		Images []testImage `xml:"imagesSet>item"`
	}
	e.mustEC2(&got, "DescribeImages", "ImageId.1", id)
	if len(got.Images) != 1 {
		e.t.Fatalf("DescribeImages %s found %d images", id, len(got.Images))
	}
	return got.Images[0]
}

func TestStockUbuntuImageIsFoundAsTerraformAndPackerLookForIt(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Images []testImage `xml:"imagesSet>item"`
	}

	e.mustEC2(&got, "DescribeImages", "Owner.1", "099720109477",
		"Filter.1.Name", "name", "Filter.1.Value.1", "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",
		"Filter.2.Name", "architecture", "Filter.2.Value.1", "x86_64",
		"Filter.3.Name", "root-device-type", "Filter.3.Value.1", "ebs")

	if len(got.Images) != 1 || got.Images[0].State != "available" || got.Images[0].Owner != canonicalOwner {
		t.Fatalf("images = %+v, want the stock Ubuntu image", got.Images)
	}
	var mine struct {
		Images []testImage `xml:"imagesSet>item"`
	}
	e.mustEC2(&mine, "DescribeImages", "Owner.1", "self")
	if len(mine.Images) != 0 {
		t.Fatalf("own images of a new account = %+v", mine.Images)
	}
}

func TestCreateImageCommitsTheMachineAndNewInstancesBootFromIt(t *testing.T) {
	e := newEnv(t)
	source := e.runInstance()
	e.waitState(source, "running")
	e.mustEC2(nil, "StopInstances", "InstanceId.1", source)
	e.waitState(source, "stopped")
	release := e.machines.hold("Commit")

	var created struct {
		ID string `xml:"imageId"`
	}
	e.mustEC2(&created, "CreateImage", "InstanceId", source, "Name", "stagely-golden",
		"TagSpecification.1.ResourceType", "image", "TagSpecification.1.Tag.1.Key", "Name", "TagSpecification.1.Tag.1.Value", "golden")

	if got := e.image(created.ID); got.State != "pending" {
		t.Fatalf("image while the machine is committed = %s, want pending", got.State)
	}
	if code := e.ec2Error("CreateImage", "InstanceId", source, "Name", "stagely-golden-2"); code != "IncorrectInstanceState" {
		t.Fatalf("second image while the first is taken = %s, want IncorrectInstanceState", code)
	}
	release()
	var img testImage
	eventually(t, "the image is available", func() bool { img = e.image(created.ID); return img.State == "available" })
	ref := "stagely-sim-aws-image:" + e.machines.spec(source).Account + "-" + img.SnapshotID
	if !e.machines.hasImage(ref) {
		t.Fatalf("no Docker image %s behind the AMI", ref)
	}

	booted := e.runInstanceOf(created.ID)
	e.waitState(booted, "running")
	if got := e.machines.spec(booted).Image; got != ref {
		t.Fatalf("instance booted from %q, want %q", got, ref)
	}
	if code := e.ec2Error("CreateImage", "InstanceId", booted, "Name", "stagely-golden"); code != "InvalidAMIName.Duplicate" {
		t.Fatalf("image with a taken name = %s, want InvalidAMIName.Duplicate", code)
	}
}

func (e *testEnv) runInstanceOf(image string) string {
	e.t.Helper()
	var run struct {
		ID string `xml:"instancesSet>item>instanceId"`
	}
	e.mustEC2(&run, "RunInstances", "ImageId", image, "InstanceType", "t3.medium", "MinCount", "1", "MaxCount", "1")
	return run.ID
}

func TestSnapshotOfARegisteredImageIsInUseUntilTheImageIsDeregistered(t *testing.T) {
	e := newEnv(t)
	source := e.runInstance()
	e.waitState(source, "running")
	var created struct {
		ID string `xml:"imageId"`
	}
	e.mustEC2(&created, "CreateImage", "InstanceId", source, "Name", "to-delete")
	var img testImage
	eventually(t, "the image is available", func() bool { img = e.image(created.ID); return img.State == "available" })
	ref := "stagely-sim-aws-image:" + e.machines.spec(source).Account + "-" + img.SnapshotID

	if code := e.ec2Error("DeleteSnapshot", "SnapshotId", img.SnapshotID); code != "InvalidSnapshot.InUse" {
		t.Fatalf("delete the snapshot of a registered image = %s, want InvalidSnapshot.InUse", code)
	}
	e.mustEC2(nil, "DeregisterImage", "ImageId", created.ID)
	e.mustEC2(nil, "DeleteSnapshot", "SnapshotId", img.SnapshotID)

	if e.machines.hasImage(ref) {
		t.Fatalf("Docker image %s is left after its snapshot was deleted", ref)
	}
	if code := e.ec2Error("DeregisterImage", "ImageId", e.stockImage()); code != "AuthFailure" {
		t.Fatalf("deregister the stock image = %s, want AuthFailure", code)
	}
}
