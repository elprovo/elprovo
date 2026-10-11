package digitalocean

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
)

type reservedIPResponse struct {
	ReservedIP reservedIPJSON `json:"reserved_ip"`
	Links      links          `json:"links"`
}

func (e *testEnv) reservedIP(ip string) reservedIPJSON {
	e.t.Helper()
	var got reservedIPResponse
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/reserved_ips/"+ip, nil, &got)
	return got.ReservedIP
}

func (e *testEnv) reservedIPAction(ip, typ string, dropletID int64) actionJSON {
	e.t.Helper()
	var started struct {
		Action actionJSON `json:"action"`
	}
	body := map[string]any{"type": typ}
	if dropletID != 0 {
		body["droplet_id"] = dropletID
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/reserved_ips/"+ip+"/actions", body, &started)
	var done struct {
		Action actionJSON `json:"action"`
	}
	path := "/v2/reserved_ips/" + ip + "/actions/" + strconv.FormatInt(started.Action.ID, 10)
	eventually(e.t, typ+" to finish", func() bool {
		e.mustCall(http.StatusOK, http.MethodGet, path, nil, &done)
		return done.Action.Status != "in-progress"
	})
	return done.Action
}

func TestReservedIPMovesBetweenDropletsAndBack(t *testing.T) {
	e := newEnv(t)
	web1 := e.createDroplet("web-1")
	web2 := e.createDroplet("web-2")
	var created reservedIPResponse

	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/reserved_ips", map[string]any{"region": "nyc3"}, &created)

	ip := created.ReservedIP.IP
	if ip != "172.31.128.0" || created.ReservedIP.Droplet != nil || created.ReservedIP.Region.Slug != "nyc3" {
		t.Fatalf("created = %+v; want the first address in the middle of the public network, in nyc3, unassigned", created.ReservedIP)
	}

	assign := e.reservedIPAction(ip, "assign", web1.ID)
	if assign.Status != "completed" || assign.Type != "assign_ip" || assign.ResourceType != "reserved_ip" || assign.ResourceID != 2887745536 {
		t.Fatalf("assign = %+v; want a completed assign_ip action on the address as a number", assign)
	}
	if got := e.reservedIP(ip); got.Droplet == nil || got.Droplet.ID != web1.ID {
		t.Fatalf("after assign, droplet = %+v, want web-1", got.Droplet)
	}
	if got := e.machines.addressesOf(web1.ID); !slices.Equal(got, []string{ip}) {
		t.Fatalf("web-1's machine has %v, want the reserved IP", got)
	}

	e.reservedIPAction(ip, "assign", web2.ID)
	if a, b := e.machines.addressesOf(web1.ID), e.machines.addressesOf(web2.ID); len(a) != 0 || !slices.Equal(b, []string{ip}) {
		t.Fatalf("after moving, web-1 has %v and web-2 has %v; want the address only on web-2", a, b)
	}

	e.reservedIPAction(ip, "unassign", 0)
	if got := e.reservedIP(ip); got.Droplet != nil {
		t.Fatalf("after unassign, droplet = %+v, want none", got.Droplet)
	}
	if got := e.machines.addressesOf(web2.ID); len(got) != 0 {
		t.Fatalf("web-2's machine still has %v after unassign", got)
	}
}

func TestReservedIPCreatedForADropletIsAssignedToIt(t *testing.T) {
	e := newEnv(t)
	web := e.createDroplet("web-1")
	var created reservedIPResponse

	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/reserved_ips", map[string]any{"droplet_id": web.ID}, &created)

	if len(created.Links.Actions) != 1 {
		t.Fatalf("links = %+v, want the assign action", created.Links)
	}
	e.waitAction(created.Links.Actions[0].ID)
	if got := e.reservedIP(created.ReservedIP.IP); got.Droplet == nil || got.Droplet.ID != web.ID || got.Region.Slug != "nyc3" {
		t.Fatalf("reserved IP = %+v, want it on web-1 in its region", got)
	}
}

func TestDeletingAReservedIPOrItsDropletReleasesTheAddress(t *testing.T) {
	e := newEnv(t)
	web := e.createDroplet("web-1")
	var first, second reservedIPResponse
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/reserved_ips", map[string]any{"region": "nyc3"}, &first)
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/reserved_ips", map[string]any{"region": "nyc3"}, &second)
	e.reservedIPAction(first.ReservedIP.IP, "assign", web.ID)
	e.reservedIPAction(second.ReservedIP.IP, "assign", web.ID)

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/reserved_ips/"+first.ReservedIP.IP, nil, nil)

	if got := e.machines.addressesOf(web.ID); !slices.Equal(got, []string{second.ReservedIP.IP}) {
		t.Fatalf("after deleting the first address, the machine has %v; want only the second", got)
	}
	e.mustCall(http.StatusNotFound, http.MethodGet, "/v2/reserved_ips/"+first.ReservedIP.IP, nil, nil)

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/droplets/"+strconv.FormatInt(web.ID, 10), nil, nil)
	eventually(t, "the droplet's deletion to release the second address", func() bool {
		return e.reservedIP(second.ReservedIP.IP).Droplet == nil
	})
}

func TestReservedIPRequestsDigitalOceanRefuses(t *testing.T) {
	e := newEnv(t)
	web := e.createDroplet("web-1")
	var other struct {
		Droplet dropletJSON `json:"droplet"`
		Links   links       `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{"name": "far", "region": "fra1", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64"}, &other)
	e.waitAction(other.Links.Actions[0].ID)
	var created reservedIPResponse
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/reserved_ips", map[string]any{"region": "nyc3"}, &created)
	ip := created.ReservedIP.IP

	tests := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"neither region nor droplet", http.MethodPost, "/v2/reserved_ips", map[string]any{}, http.StatusUnprocessableEntity},
		{"unknown region", http.MethodPost, "/v2/reserved_ips", map[string]any{"region": "mars1"}, http.StatusUnprocessableEntity},
		{"droplet in another region", http.MethodPost, "/v2/reserved_ips/" + ip + "/actions", map[string]any{"type": "assign", "droplet_id": other.Droplet.ID}, http.StatusUnprocessableEntity},
		{"missing droplet", http.MethodPost, "/v2/reserved_ips/" + ip + "/actions", map[string]any{"type": "assign", "droplet_id": 999999}, http.StatusUnprocessableEntity},
		{"unassign an unassigned address", http.MethodPost, "/v2/reserved_ips/" + ip + "/actions", map[string]any{"type": "unassign"}, http.StatusUnprocessableEntity},
		{"unknown action", http.MethodPost, "/v2/reserved_ips/" + ip + "/actions", map[string]any{"type": "rename"}, http.StatusUnprocessableEntity},
		{"unknown address", http.MethodGet, "/v2/reserved_ips/203.0.113.9", nil, http.StatusNotFound},
		{"not an address", http.MethodGet, "/v2/reserved_ips/web/actions/1", nil, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.call(tt.method, tt.path, tt.body, nil); got != tt.want {
				t.Fatalf("%s %s = %d, want %d", tt.method, tt.path, got, tt.want)
			}
		})
	}

	release := e.machines.hold("SetAddresses")
	var started struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/reserved_ips/"+ip+"/actions", map[string]any{"type": "assign", "droplet_id": web.ID}, &started)
	if got := e.call(http.MethodPost, "/v2/reserved_ips/"+ip+"/actions", map[string]any{"type": "unassign"}, nil); got != http.StatusUnprocessableEntity {
		t.Fatalf("an action while another runs = %d, want 422", got)
	}
	if got := e.call(http.MethodDelete, "/v2/reserved_ips/"+ip, nil, nil); got != http.StatusUnprocessableEntity {
		t.Fatalf("a delete while an action runs = %d, want 422", got)
	}
	release()
	e.waitAction(started.Action.ID)
}
