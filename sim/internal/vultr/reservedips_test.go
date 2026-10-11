package vultr

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func (e *testEnv) createReservedIP(regionID string) reservedIPJSON {
	e.t.Helper()
	var created struct {
		ReservedIP reservedIPJSON `json:"reserved_ip"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/reserved-ips", map[string]string{"region": regionID, "ip_type": "v4", "label": "front"}, &created)
	return created.ReservedIP
}

func TestReservedIPComesFromTheMiddleOfThePublicNetwork(t *testing.T) {
	e := newEnv(t)

	reserved := e.createReservedIP("ewr")

	if reserved.Subnet != "172.31.128.0" || reserved.SubnetSize != 32 || reserved.IPType != "v4" || reserved.Label != "front" || reserved.InstanceID != "" {
		t.Fatalf("reserved IP = %+v", reserved)
	}
}

func TestAttachedReservedIPReachesTheInstancesMachine(t *testing.T) {
	e := newEnv(t)
	reserved := e.createReservedIP("ewr")
	inst := e.createInstance("web-1", nil)
	path := "/v2/reserved-ips/" + reserved.ID

	e.mustCall(http.StatusNoContent, http.MethodPost, path+"/attach", map[string]string{"instance_id": inst.ID}, nil)
	e.mustCall(http.StatusNoContent, http.MethodPost, path+"/attach", map[string]string{"instance_id": inst.ID}, nil)

	if got := e.machines.addressesOf(inst.ID); !slices.Equal(got, []string{reserved.Subnet}) {
		t.Fatalf("machine addresses = %v, want the reserved IP", got)
	}
	var read struct {
		ReservedIP reservedIPJSON `json:"reserved_ip"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, path, nil, &read)
	if read.ReservedIP.InstanceID != inst.ID {
		t.Fatalf("instance_id = %q, want %q", read.ReservedIP.InstanceID, inst.ID)
	}
	other := e.createInstance("web-2", nil)
	if status := e.call(http.MethodPost, path+"/attach", map[string]string{"instance_id": other.ID}, nil); status != http.StatusBadRequest {
		t.Fatalf("attach to a second instance = %d, want 400 until it is detached", status)
	}

	e.mustCall(http.StatusNoContent, http.MethodPost, path+"/detach", nil, nil)
	if got := e.machines.addressesOf(inst.ID); len(got) != 0 {
		t.Fatalf("machine addresses after detach = %v", got)
	}
}

func TestInstanceCreatedWithAReservedIPHasItAsItsMainAddress(t *testing.T) {
	e := newEnv(t)
	reserved := e.createReservedIP("ewr")

	inst := e.createInstance("gateway", map[string]any{"reserved_ipv4": reserved.ID})

	if inst.MainIP != reserved.Subnet {
		t.Fatalf("main_ip = %s, want the reserved IP %s", inst.MainIP, reserved.Subnet)
	}
	if spec := e.machines.spec(inst.ID); spec.PublicIP != reserved.Subnet {
		t.Fatalf("machine public address = %q, want the reserved IP, so the machine sends from it", spec.PublicIP)
	}
	if got := e.machines.addressesOf(inst.ID); len(got) != 0 {
		t.Fatalf("machine extra addresses = %v, want none", got)
	}
	var body errorBody
	status := e.call(http.MethodPost, "/v2/instances", map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "os_id": 2284, "reserved_ipv4": reserved.ID}, &body)
	if status != http.StatusBadRequest || !strings.Contains(body.Error, "Floating IPv4 address is already attached to another server") {
		t.Fatalf("second instance with the address = %d %+v; the Terraform provider retries on that message", status, body)
	}
	if status := e.call(http.MethodPost, "/v2/reserved-ips/"+reserved.ID+"/detach", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("detaching a main address = %d, want 400", status)
	}
	if status := e.call(http.MethodDelete, "/v2/reserved-ips/"+reserved.ID, nil, nil); status != http.StatusBadRequest {
		t.Fatalf("deleting a main address = %d, want 400 while its instance exists", status)
	}

	release := e.machines.hold("Delete")
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+inst.ID, nil, nil)
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/reserved-ips/"+reserved.ID, nil, nil)
	release()
}

func TestDeletedInstanceReleasesItsReservedIP(t *testing.T) {
	e := newEnv(t)
	reserved := e.createReservedIP("ewr")
	inst := e.createInstance("gateway", map[string]any{"reserved_ipv4": reserved.ID})

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+inst.ID, nil, nil)

	eventually(t, "the address is free again", func() bool {
		var read struct {
			ReservedIP reservedIPJSON `json:"reserved_ip"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v2/reserved-ips/"+reserved.ID, nil, &read)
		return read.ReservedIP.InstanceID == ""
	})
}

func TestDeletingAnAttachedReservedIPTakesItOffTheMachine(t *testing.T) {
	e := newEnv(t)
	reserved := e.createReservedIP("ewr")
	inst := e.createInstance("web-1", nil)
	e.mustCall(http.StatusNoContent, http.MethodPost, "/v2/reserved-ips/"+reserved.ID+"/attach", map[string]string{"instance_id": inst.ID}, nil)

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/reserved-ips/"+reserved.ID, nil, nil)

	if got := e.machines.addressesOf(inst.ID); len(got) != 0 {
		t.Fatalf("machine addresses = %v after the address was deleted", got)
	}
	var body errorBody
	if status := e.call(http.MethodGet, "/v2/reserved-ips/"+reserved.ID, nil, &body); status != http.StatusNotFound || !strings.Contains(body.Error, "Invalid reserved-ip ID") {
		t.Fatalf("deleted address = %d %+v", status, body)
	}
}

func TestFailedAddressPushLeavesTheReservedIPDetached(t *testing.T) {
	e := newEnv(t)
	reserved := e.createReservedIP("ewr")
	inst := e.createInstance("web-1", nil)
	e.machines.fail("SetAddresses", errors.New("docker exec failed"))

	if status := e.call(http.MethodPost, "/v2/reserved-ips/"+reserved.ID+"/attach", map[string]string{"instance_id": inst.ID}, nil); status != http.StatusInternalServerError {
		t.Fatalf("attach with a failing machine = %d, want 500", status)
	}
	var read struct {
		ReservedIP reservedIPJSON `json:"reserved_ip"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/reserved-ips/"+reserved.ID, nil, &read)
	if read.ReservedIP.InstanceID != "" {
		t.Fatalf("instance_id = %q after a failed attach", read.ReservedIP.InstanceID)
	}
}

func TestReservedIPRejectsIPv6AndOtherRegions(t *testing.T) {
	e := newEnv(t)
	inst := e.createInstance("web-1", nil)
	if status := e.call(http.MethodPost, "/v2/reserved-ips", map[string]string{"region": "ewr", "ip_type": "v6"}, nil); status != http.StatusBadRequest {
		t.Fatalf("IPv6 = %d, want 400", status)
	}
	frankfurt := e.createReservedIP("fra")
	if status := e.call(http.MethodPost, "/v2/reserved-ips/"+frankfurt.ID+"/attach", map[string]string{"instance_id": inst.ID}, nil); status != http.StatusBadRequest {
		t.Fatalf("attach across regions = %d, want 400", status)
	}
}
