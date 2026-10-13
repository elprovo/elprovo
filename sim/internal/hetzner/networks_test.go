package hetzner

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

type networkResponse struct {
	Network networkJSON `json:"network"`
}

func (e *testEnv) createNetwork(name string) networkJSON {
	e.t.Helper()
	var got networkResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/networks", map[string]any{
		"name": name, "ip_range": "10.0.0.0/16",
		"subnets": []map[string]any{{"type": "cloud", "ip_range": "10.0.1.0/24", "network_zone": "eu-central"}},
	}, &got)
	return got.Network
}

func TestNetworkSubnetGetsTheNetworkGateway(t *testing.T) {
	e := newEnv(t)

	n := e.createNetwork("elprovo")

	if len(n.Subnets) != 1 || n.Subnets[0].Gateway != "10.0.0.1" || n.Subnets[0].NetworkZone != "eu-central" || n.Subnets[0].VSwitchID != nil {
		t.Fatalf("subnets = %+v; want one cloud subnet with the gateway 10.0.0.1", n.Subnets)
	}
}

func TestNetworkRangeMustBePrivate(t *testing.T) {
	e := newEnv(t)

	status, code := e.errorCode(http.MethodPost, "/v1/networks", map[string]any{"name": "public", "ip_range": "8.8.0.0/16"})

	if status != http.StatusUnprocessableEntity || code != "invalid_input" {
		t.Fatalf("create with a public range = %d %s, want 422 invalid_input", status, code)
	}
}

func TestSubnetWithoutRangeTakesTheFirstFreeBlock(t *testing.T) {
	e := newEnv(t)
	n := e.createNetwork("elprovo")

	var added struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, fmt.Sprintf("/v1/networks/%d/actions/add_subnet", n.ID),
		map[string]any{"type": "cloud", "network_zone": "eu-central"}, &added)

	var got networkResponse
	e.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/networks/%d", n.ID), nil, &got)
	if added.Action.Command != "add_subnet" || len(got.Network.Subnets) != 2 || got.Network.Subnets[1].IPRange != "10.0.0.0/24" {
		t.Fatalf("subnets after add_subnet = %+v", got.Network.Subnets)
	}
}

func TestAttachedServerReportsItsPrivateAddress(t *testing.T) {
	e := newEnv(t)
	n := e.createNetwork("elprovo")
	srv := e.createServer(serverRequest("web-1", nil))

	status, attach := e.postServerAction(srv.ID, "attach_to_network", map[string]any{"network": n.ID})
	if status != http.StatusCreated || attach.Status != "success" {
		t.Fatalf("attach = %d %+v", status, attach)
	}

	got := e.serverByID(srv.ID)
	if len(got.PrivateNet) != 1 || got.PrivateNet[0].Network != n.ID || got.PrivateNet[0].IP == "" {
		t.Fatalf("private_net = %+v, want the network with the machine's private address", got.PrivateNet)
	}
	var listed networkResponse
	e.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/networks/%d", n.ID), nil, &listed)
	if !slices.Equal(listed.Network.Servers, []int64{srv.ID}) {
		t.Fatalf("network servers = %v, want [%d]", listed.Network.Servers, srv.ID)
	}
	if status, code := e.errorCode(http.MethodPost, fmt.Sprintf("/v1/servers/%d/actions/attach_to_network", srv.ID), map[string]any{"network": n.ID}); status != http.StatusUnprocessableEntity || code != "server_already_attached" {
		t.Fatalf("second attach = %d %s, want 422 server_already_attached", status, code)
	}
}

func TestFirewallsLetAllTrafficOnTheNetworkThrough(t *testing.T) {
	e := newEnv(t)
	n := e.createNetwork("elprovo")
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", previewFirewall("172.31.0.9"), nil)
	attached := serverRequest("elprovo-a1", map[string]string{"elprovo": "env-preview"})
	attached["networks"] = []int64{n.ID}

	srv := e.createServer(attached)
	off := e.createServer(serverRequest("elprovo-a2", map[string]string{"elprovo": "env-preview"}))

	fromNetwork := func(r machines.Rule) bool { return slices.Contains(r.Peers, "172.30.0.0/16") }
	if fw := e.machines.spec(srv.ID).Firewall; fw == nil || !slices.ContainsFunc(fw.Inbound, fromNetwork) {
		t.Fatalf("machine firewall = %+v; want the whole private network admitted, as Hetzner's firewalls do not filter networks", fw)
	}
	if fw := e.machines.spec(off.ID).Firewall; fw == nil || slices.ContainsFunc(fw.Inbound, fromNetwork) {
		t.Fatalf("firewall of a server off the network = %+v; want only its rules", fw)
	}
}

func TestDeletedNetworkDetachesItsServers(t *testing.T) {
	e := newEnv(t)
	n := e.createNetwork("elprovo")
	req := serverRequest("web-1", nil)
	req["networks"] = []int64{n.ID}
	srv := e.createServer(req)

	e.mustCall(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/v1/networks/%d", n.ID), nil, nil)

	if got := e.serverByID(srv.ID).PrivateNet; len(got) != 0 {
		t.Fatalf("private_net after the network's deletion = %+v, want none", got)
	}
}
