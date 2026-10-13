package hetzner

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
)

func TestCreatedServerHasItsPublicAddressBeforeItRuns(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	defer release()

	var created createServerResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/servers", serverRequest("web-1", map[string]string{"elprovo": "env-preview"}), &created)

	srv := created.Server
	if srv.Status != "initializing" || srv.PublicNet.IPv4 == nil || srv.PublicNet.IPv4.IP == "" || srv.PublicNet.IPv4.ID == 0 {
		t.Fatalf("server in the create answer = status %s, ipv4 %+v; want initializing with a primary IPv4", srv.Status, srv.PublicNet.IPv4)
	}
	if !netip.MustParsePrefix("172.31.0.0/16").Contains(netip.MustParseAddr(srv.PublicNet.IPv4.IP)) {
		t.Fatalf("public address %s is not on the public network", srv.PublicNet.IPv4.IP)
	}
	if created.Action.Command != "create_server" || created.Action.Status != "running" ||
		len(created.NextActions) != 1 || created.NextActions[0].Command != "start_server" {
		t.Fatalf("actions = %+v then %+v; want create_server running and start_server next", created.Action, created.NextActions)
	}
	if srv.PublicNet.IPv6 != nil || srv.Image == nil || *srv.Image.Name != "ubuntu-24.04" || srv.ServerType.Name != "cx23" || srv.Location.Name != "fsn1" {
		t.Fatalf("server = %+v", srv)
	}
}

func TestServerRunsOnAMachineWithItsPrimaryIP(t *testing.T) {
	e := newEnv(t)

	srv := e.createServer(serverRequest("web-1", nil))

	if srv.Status != "running" {
		t.Fatalf("status = %s, want running", srv.Status)
	}
	spec := e.machines.spec(srv.ID)
	if spec.PublicIP != srv.PublicNet.IPv4.IP || spec.Hostname != "web-1" || spec.Cloud != "hetzner" || spec.Image != "" {
		t.Fatalf("machine spec = %+v; want the server's primary IP %s, its name as hostname and the base image", spec, srv.PublicNet.IPv4.IP)
	}
	var ip struct {
		PrimaryIP primaryIPJSON `json:"primary_ip"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/primary_ips/%d", srv.PublicNet.IPv4.ID), nil, &ip)
	if ip.PrimaryIP.AssigneeType != "server" || ip.PrimaryIP.AssigneeID == nil || *ip.PrimaryIP.AssigneeID != srv.ID || !ip.PrimaryIP.AutoDelete {
		t.Fatalf("primary IP = %+v; want it assigned to the server and deleted with it", ip.PrimaryIP)
	}
}

func TestServersAreFoundByLabelSelectorAndName(t *testing.T) {
	e := newEnv(t)
	e.createServer(serverRequest("elprovo-a1", map[string]string{"elprovo": "prod-preview"}))
	e.createServer(serverRequest("elprovo-a2", map[string]string{"elprovo": "dev-preview"}))
	e.createServer(serverRequest("other", nil))

	var got struct {
		Servers []serverJSON `json:"servers"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/servers?label_selector=elprovo%3D%3Dprod-preview", nil, &got)
	if len(got.Servers) != 1 || got.Servers[0].Name != "elprovo-a1" {
		t.Fatalf("servers with elprovo==prod-preview = %v", got.Servers)
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/servers?name=other", nil, &got)
	if len(got.Servers) != 1 || got.Servers[0].Name != "other" {
		t.Fatalf("servers named other = %v", got.Servers)
	}
}

func TestCreateRefusesWhatTheAPIOrTheSimulationDoesNot(t *testing.T) {
	cases := []struct {
		name   string
		change func(req map[string]any)
		status int
		code   string
	}{
		{"name that is not a hostname", func(r map[string]any) { r["name"] = "web_1" }, http.StatusUnprocessableEntity, "invalid_input"},
		{"unknown server type", func(r map[string]any) { r["server_type"] = "cx99" }, http.StatusUnprocessableEntity, "invalid_input"},
		{"server type the location does not offer", func(r map[string]any) { r["location"] = "ash" }, http.StatusUnprocessableEntity, "invalid_server_type"},
		{"unknown image", func(r map[string]any) { r["image"] = "debian-13" }, http.StatusUnprocessableEntity, "invalid_input"},
		{"IPv6 by default", func(r map[string]any) { delete(r, "public_net") }, http.StatusUnprocessableEntity, "unsupported_error"},
		{"no public IPv4", func(r map[string]any) { r["public_net"] = map[string]any{"enable_ipv4": false, "enable_ipv6": false} }, http.StatusUnprocessableEntity, "unsupported_error"},
		{"datacenter", func(r map[string]any) { r["datacenter"] = "fsn1-dc14" }, http.StatusUnprocessableEntity, "invalid_input"},
		{"volumes", func(r map[string]any) { r["volumes"] = []int{1} }, http.StatusUnprocessableEntity, "unsupported_error"},
		{"unknown SSH key", func(r map[string]any) { r["ssh_keys"] = []string{"nobody"} }, http.StatusUnprocessableEntity, "invalid_input"},
		{"oversized user data", func(r map[string]any) { r["user_data"] = string(make([]byte, maxUserData+1)) }, http.StatusUnprocessableEntity, "invalid_input"},
		{"reserved label prefix", func(r map[string]any) { r["labels"] = map[string]string{"hetzner.cloud/x": "y"} }, http.StatusUnprocessableEntity, "invalid_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			req := serverRequest("web-1", nil)
			tc.change(req)

			status, code := e.errorCode(http.MethodPost, "/v1/servers", req)

			if status != tc.status || code != tc.code {
				t.Fatalf("create = %d %s, want %d %s", status, code, tc.status, tc.code)
			}
		})
	}
}

func TestServerNamesAreUnique(t *testing.T) {
	e := newEnv(t)
	e.createServer(serverRequest("web-1", nil))

	status, code := e.errorCode(http.MethodPost, "/v1/servers", serverRequest("web-1", nil))

	if status != http.StatusConflict || code != "uniqueness_error" {
		t.Fatalf("second web-1 = %d %s, want 409 uniqueness_error", status, code)
	}
}

func TestServerTakesTheSSHKeysItNames(t *testing.T) {
	e := newEnv(t)
	var key struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "agent", "public_key": agentKey}, &key)
	req := serverRequest("web-1", nil)
	req["ssh_keys"] = []any{key.SSHKey.ID}

	srv := e.createServer(req)

	if keys := e.machines.spec(srv.ID).Seed.PublicKeys; len(keys) != 1 || keys[0] != agentKey {
		t.Fatalf("machine keys = %q, want the agent's key", keys)
	}
}

func TestDeletedServerAnswersUntilItsMachineIsGone(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", nil))
	release := e.machines.hold("Delete")
	defer release()

	var deleted struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusOK, http.MethodDelete, fmt.Sprintf("/v1/servers/%d", srv.ID), nil, &deleted)

	if deleted.Action.Command != "delete_server" || e.serverByID(srv.ID).Status != "deleting" {
		t.Fatalf("delete action = %+v; want delete_server while the server is deleting", deleted.Action)
	}
	release()
	if got := e.waitAction(deleted.Action.ID); got.Status != "success" {
		t.Fatalf("delete action = %+v, want success", got)
	}
	if status, code := e.errorCode(http.MethodGet, fmt.Sprintf("/v1/servers/%d", srv.ID), nil); status != http.StatusNotFound || code != "not_found" {
		t.Fatalf("GET deleted server = %d %s, want 404 not_found", status, code)
	}
	if e.machines.hasMachine(srv.ID) {
		t.Fatal("the deleted server's machine still exists")
	}
	if status := e.call(http.MethodGet, fmt.Sprintf("/v1/primary_ips/%d", srv.PublicNet.IPv4.ID), nil, nil); status != http.StatusNotFound {
		t.Fatalf("GET the server's auto-delete primary IP = %d, want 404", status)
	}
}

func TestFailedMachineDeletionKeepsTheServer(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", nil))
	e.machines.fail("Delete", errors.New("docker is gone"))

	var deleted struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusOK, http.MethodDelete, fmt.Sprintf("/v1/servers/%d", srv.ID), nil, &deleted)

	if got := e.waitAction(deleted.Action.ID); got.Status != "error" || got.Error == nil {
		t.Fatalf("delete action = %+v, want error", got)
	}
	if got := e.serverByID(srv.ID); got.Status != "running" {
		t.Fatalf("server after a failed delete = %s, want running again", got.Status)
	}
}

func TestServerIsLockedWhileItsActionRuns(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	defer release()
	var created createServerResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/servers", serverRequest("web-1", nil), &created)

	status, code := e.errorCode(http.MethodDelete, fmt.Sprintf("/v1/servers/%d", created.Server.ID), nil)

	if status != http.StatusLocked || code != "locked" {
		t.Fatalf("delete while creating = %d %s, want 423 locked", status, code)
	}
}

func TestFailedMachineCreationFailsTheCreateAction(t *testing.T) {
	e := newEnv(t)
	e.machines.fail("Create", errors.New("no space left"))
	var created createServerResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/servers", serverRequest("web-1", nil), &created)

	got := e.waitAction(created.Action.ID)

	if got.Status != "error" || got.Error == nil || e.serverByID(created.Server.ID).Status != "off" {
		t.Fatalf("create action = %+v, server status %s; want an error and the server off", got, e.serverByID(created.Server.ID).Status)
	}
}

func TestStandalonePrimaryIPOutlivesItsServer(t *testing.T) {
	e := newEnv(t)
	var ip struct {
		PrimaryIP primaryIPJSON `json:"primary_ip"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/primary_ips", map[string]any{"name": "gateway", "type": "ipv4", "location": "fsn1"}, &ip)
	req := serverRequest("gateway", nil)
	req["public_net"] = map[string]any{"enable_ipv4": true, "enable_ipv6": false, "ipv4": ip.PrimaryIP.ID}

	srv := e.createServer(req)

	if srv.PublicNet.IPv4.IP != ip.PrimaryIP.IP || e.machines.spec(srv.ID).PublicIP != ip.PrimaryIP.IP {
		t.Fatalf("server address %s, machine address %s; want the primary IP %s", srv.PublicNet.IPv4.IP, e.machines.spec(srv.ID).PublicIP, ip.PrimaryIP.IP)
	}
	if status, code := e.errorCode(http.MethodDelete, fmt.Sprintf("/v1/primary_ips/%d", ip.PrimaryIP.ID), nil); status != http.StatusUnprocessableEntity || code != "must_be_unassigned" {
		t.Fatalf("delete an assigned primary IP = %d %s, want 422 must_be_unassigned", status, code)
	}
	var deleted struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusOK, http.MethodDelete, fmt.Sprintf("/v1/servers/%d", srv.ID), nil, &deleted)
	e.waitAction(deleted.Action.ID)
	e.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/primary_ips/%d", ip.PrimaryIP.ID), nil, &ip)
	if ip.PrimaryIP.AssigneeID != nil || ip.PrimaryIP.AssigneeType != "unassigned" {
		t.Fatalf("primary IP after its server's deletion = %+v, want it kept and unassigned", ip.PrimaryIP)
	}
	again := e.createServer(req)
	if again.PublicNet.IPv4.IP != ip.PrimaryIP.IP {
		t.Fatalf("a new server took %s, want the kept primary IP %s", again.PublicNet.IPv4.IP, ip.PrimaryIP.IP)
	}
}

func TestPrimaryIPInAnotherLocationIsRefused(t *testing.T) {
	e := newEnv(t)
	var ip struct {
		PrimaryIP primaryIPJSON `json:"primary_ip"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/primary_ips", map[string]any{"name": "gateway", "type": "ipv4", "location": "nbg1"}, &ip)
	req := serverRequest("gateway", nil)
	req["public_net"] = map[string]any{"enable_ipv4": true, "enable_ipv6": false, "ipv4": ip.PrimaryIP.ID}

	status, code := e.errorCode(http.MethodPost, "/v1/servers", req)

	if status != http.StatusUnprocessableEntity || code != "primary_ip_datacenter_mismatch" {
		t.Fatalf("create = %d %s, want 422 primary_ip_datacenter_mismatch", status, code)
	}
}

func TestRelabeledServerKeepsItsName(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", map[string]string{"a": "b"}))
	var got struct {
		Server serverJSON `json:"server"`
	}

	e.mustCall(http.StatusOK, http.MethodPut, fmt.Sprintf("/v1/servers/%d", srv.ID), map[string]any{"labels": map[string]string{"c": "d"}}, &got)

	if got.Server.Name != "web-1" || len(got.Server.Labels) != 1 || got.Server.Labels["c"] != "d" {
		t.Fatalf("server after relabel = %+v, want name web-1 and only the new labels", got.Server)
	}
}
