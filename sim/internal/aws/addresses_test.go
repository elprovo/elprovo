package aws

import (
	"slices"
	"testing"
)

func TestElasticIPReachesTheInstanceItIsAssociatedWith(t *testing.T) {
	e := newEnv(t)
	id := e.runInstance()
	launched := e.waitState(id, "running")
	var allocated struct {
		IP string `xml:"publicIp"`
		ID string `xml:"allocationId"`
	}
	e.mustEC2(&allocated, "AllocateAddress", "Domain", "vpc")
	if allocated.IP != "172.31.128.0" {
		t.Fatalf("Elastic IP = %s, want the middle of the public network", allocated.IP)
	}

	var association struct {
		ID string `xml:"associationId"`
	}
	e.mustEC2(&association, "AssociateAddress", "AllocationId", allocated.ID, "InstanceId", id)

	if got := e.machines.addressesOf(id); !slices.Equal(got, []string{allocated.IP}) {
		t.Fatalf("machine's extra addresses = %v, want the Elastic IP", got)
	}
	if got := e.instance(id); got.PublicIP != allocated.IP || launched.PublicIP == allocated.IP {
		t.Fatalf("instance public address = %s, want the Elastic IP %s instead of %s", got.PublicIP, allocated.IP, launched.PublicIP)
	}
	if code := e.ec2Error("ReleaseAddress", "AllocationId", allocated.ID); code != "InvalidIPAddress.InUse" {
		t.Fatalf("release an associated address = %s, want InvalidIPAddress.InUse", code)
	}
	e.mustEC2(nil, "DisassociateAddress", "AssociationId", association.ID)
	if got := e.machines.addressesOf(id); len(got) != 0 {
		t.Fatalf("machine's extra addresses after disassociation = %v", got)
	}
	if got := e.instance(id); got.PublicIP != launched.PublicIP {
		t.Fatalf("instance public address after disassociation = %s, want %s", got.PublicIP, launched.PublicIP)
	}
	e.mustEC2(nil, "ReleaseAddress", "AllocationId", allocated.ID)
}

func TestElasticIPMovesOnlyWhenReassociationIsAllowed(t *testing.T) {
	e := newEnv(t)
	first, second := e.runInstance(), e.runInstance()
	e.waitState(first, "running")
	e.waitState(second, "running")
	var allocated struct {
		IP string `xml:"publicIp"`
		ID string `xml:"allocationId"`
	}
	e.mustEC2(&allocated, "AllocateAddress", "Domain", "vpc")
	e.mustEC2(nil, "AssociateAddress", "AllocationId", allocated.ID, "InstanceId", first)

	if code := e.ec2Error("AssociateAddress", "AllocationId", allocated.ID, "InstanceId", second); code != "Resource.AlreadyAssociated" {
		t.Fatalf("move without AllowReassociation = %s, want Resource.AlreadyAssociated", code)
	}
	e.mustEC2(nil, "AssociateAddress", "AllocationId", allocated.ID, "InstanceId", second, "AllowReassociation", "true")

	if len(e.machines.addressesOf(first)) != 0 || !slices.Equal(e.machines.addressesOf(second), []string{allocated.IP}) {
		t.Fatalf("extra addresses = %v and %v, want the address on the second machine only", e.machines.addressesOf(first), e.machines.addressesOf(second))
	}
}

func TestTerminatedInstanceReleasesItsElasticIPAssociation(t *testing.T) {
	e := newEnv(t)
	id := e.runInstance()
	e.waitState(id, "running")
	var allocated struct {
		ID string `xml:"allocationId"`
	}
	e.mustEC2(&allocated, "AllocateAddress", "Domain", "vpc")
	e.mustEC2(nil, "AssociateAddress", "AllocationId", allocated.ID, "InstanceId", id)

	e.mustEC2(nil, "TerminateInstances", "InstanceId.1", id)
	e.waitState(id, "terminated")

	var got struct {
		InstanceID string `xml:"addressesSet>item>instanceId"`
	}
	e.mustEC2(&got, "DescribeAddresses", "AllocationId.1", allocated.ID)
	if got.InstanceID != "" {
		t.Fatalf("address still names instance %s", got.InstanceID)
	}
	e.mustEC2(nil, "ReleaseAddress", "AllocationId", allocated.ID)
}
