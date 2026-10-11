package hetzner

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

type createFirewallResponse struct {
	Firewall firewallJSON `json:"firewall"`
	Actions  []actionJSON `json:"actions"`
}

// stageletFirewall admits SSH and HTTP from one agent address to the
// servers with the stagelet label.
func stageletFirewall(agent string) map[string]any {
	return map[string]any{
		"name": "stagelets",
		"rules": []map[string]any{
			{"direction": "in", "protocol": "tcp", "port": "1-65535", "source_ips": []string{agent + "/32"}},
		},
		"apply_to": []map[string]any{{"type": "label_selector", "label_selector": map[string]string{"selector": "stagely=env-stagelet"}}},
	}
}

func TestNewServerBootsWithTheFirewallItsLabelsMatch(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), nil)

	srv := e.createServer(serverRequest("stagely-a1", map[string]string{"stagely": "env-stagelet"}))

	want := &machines.Firewall{
		Inbound: []machines.Rule{{Protocol: "tcp", Ports: "1-65535", Peers: []string{"172.31.0.9/32"}}},
		Outbound: []machines.Rule{
			{Protocol: "tcp", Peers: []string{"0.0.0.0/0"}}, {Protocol: "udp", Peers: []string{"0.0.0.0/0"}}, {Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
		},
	}
	if got := e.machines.spec(srv.ID).Firewall; !reflect.DeepEqual(got, want) {
		t.Fatalf("machine boots with firewall %+v, want inbound only from the agent and all outbound traffic, %+v", got, want)
	}
	eventually(t, "the server reports its firewall as applied", func() bool {
		fws := e.serverByID(srv.ID).PublicNet.Firewalls
		return len(fws) == 1 && fws[0].Status == "applied"
	})
}

func TestServerWithoutAFirewallAcceptsEverything(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), nil)

	srv := e.createServer(serverRequest("other", nil))

	if fw := e.machines.spec(srv.ID).Firewall; fw != nil {
		t.Fatalf("a server no firewall applies to boots with %+v, want no firewall", fw)
	}
}

func TestFirewallOnTheLabelSelectorListsTheServersItCovers(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), &created)
	srv := e.createServer(serverRequest("stagely-a1", map[string]string{"stagely": "env-stagelet"}))

	var got struct {
		Firewall firewallJSON `json:"firewall"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/firewalls/%d", created.Firewall.ID), nil, &got)

	applied := got.Firewall.AppliedTo
	if len(applied) != 1 || applied[0].Type != "label_selector" || len(applied[0].AppliedToResources) != 1 || applied[0].AppliedToResources[0].Server.ID != srv.ID {
		t.Fatalf("applied_to = %+v, want the selector with the stagelet server", applied)
	}
}

func TestNewRulesReachRunningServersBeforeTheActionSucceeds(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), &created)
	srv := e.createServer(serverRequest("stagely-a1", map[string]string{"stagely": "env-stagelet"}))
	release := e.machines.hold("SetFirewall")
	defer release()

	var set struct {
		Actions []actionJSON `json:"actions"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, fmt.Sprintf("/v1/firewalls/%d/actions/set_rules", created.Firewall.ID), map[string]any{
		"rules": []map[string]any{{"direction": "in", "protocol": "tcp", "port": "22", "source_ips": []string{"172.31.0.10/32"}}},
	}, &set)

	if len(set.Actions) != 1 || set.Actions[0].Command != "set_firewall_rules" || set.Actions[0].Status != "running" {
		t.Fatalf("set_rules actions = %+v, want one running set_firewall_rules", set.Actions)
	}
	if fws := e.serverByID(srv.ID).PublicNet.Firewalls; len(fws) != 1 || fws[0].Status != "pending" {
		t.Fatalf("server firewalls while the push waits = %+v, want pending", fws)
	}
	release()
	if got := e.waitAction(set.Actions[0].ID); got.Status != "success" {
		t.Fatalf("set_rules action = %+v", got)
	}
	if fw := e.machines.lastFirewall(srv.ID); fw == nil || fw.Inbound[0].Ports != "22" || fw.Inbound[0].Peers[0] != "172.31.0.10/32" {
		t.Fatalf("machine firewall = %+v, want the new rule", fw)
	}
}

func TestOutboundRulesCloseOtherOutboundTraffic(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", map[string]any{
		"name": "egress",
		"rules": []map[string]any{
			{"direction": "out", "protocol": "tcp", "port": "443", "destination_ips": []string{"0.0.0.0/0", "::/0"}},
		},
	}, &created)
	req := serverRequest("web-1", nil)
	req["firewalls"] = []map[string]any{{"firewall": created.Firewall.ID}}

	srv := e.createServer(req)

	fw := e.machines.spec(srv.ID).Firewall
	if fw == nil || len(fw.Inbound) != 0 || len(fw.Outbound) != 1 || fw.Outbound[0].Ports != "443" || !reflect.DeepEqual(fw.Outbound[0].Peers, []string{"0.0.0.0/0"}) {
		t.Fatalf("machine firewall = %+v; want no inbound traffic and outbound only to 443 over IPv4", fw)
	}
}

func TestFirewallRulesAreValidated(t *testing.T) {
	cases := []struct {
		name   string
		rule   map[string]any
		status int
		code   string
	}{
		{"tcp without a port", map[string]any{"direction": "in", "protocol": "tcp", "source_ips": []string{"0.0.0.0/0"}}, http.StatusUnprocessableEntity, "invalid_input"},
		{"icmp with a port", map[string]any{"direction": "in", "protocol": "icmp", "port": "80", "source_ips": []string{"0.0.0.0/0"}}, http.StatusUnprocessableEntity, "invalid_input"},
		{"block with host bits", map[string]any{"direction": "in", "protocol": "tcp", "port": "80", "source_ips": []string{"10.0.0.1/8"}}, http.StatusUnprocessableEntity, "invalid_input"},
		{"destination on an inbound rule", map[string]any{"direction": "in", "protocol": "tcp", "port": "80", "source_ips": []string{"0.0.0.0/0"}, "destination_ips": []string{"0.0.0.0/0"}}, http.StatusUnprocessableEntity, "invalid_input"},
		{"esp, which machines can't filter", map[string]any{"direction": "in", "protocol": "esp", "source_ips": []string{"0.0.0.0/0"}}, http.StatusUnprocessableEntity, "unsupported_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)

			status, code := e.errorCode(http.MethodPost, "/v1/firewalls", map[string]any{"name": "fw", "rules": []map[string]any{tc.rule}})

			if status != tc.status || code != tc.code {
				t.Fatalf("create = %d %s, want %d %s", status, code, tc.status, tc.code)
			}
		})
	}
}

func TestFirewallEchoesRulesWithCanonicalIPv6AndNullPort(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse

	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", map[string]any{
		"name": "fw",
		"rules": []map[string]any{
			{"direction": "in", "protocol": "icmp", "source_ips": []string{"0.0.0.0/0", "2001:0db8:0000::/48"}},
		},
	}, &created)

	rule := created.Firewall.Rules[0]
	if rule.Port != nil || rule.Description != nil || !reflect.DeepEqual(rule.SourceIPs, []string{"0.0.0.0/0", "2001:db8::/48"}) || len(rule.DestinationIPs) != 0 {
		t.Fatalf("echoed rule = %+v", rule)
	}
}

func TestFirewallInUseCanNotBeDeleted(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), &created)
	path := fmt.Sprintf("/v1/firewalls/%d", created.Firewall.ID)

	if status, code := e.errorCode(http.MethodDelete, path, nil); status != http.StatusUnprocessableEntity || code != "resource_in_use" {
		t.Fatalf("delete an applied firewall = %d %s, want 422 resource_in_use", status, code)
	}
	var removed struct {
		Actions []actionJSON `json:"actions"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, path+"/actions/remove_from_resources", map[string]any{
		"remove_from": []map[string]any{{"type": "label_selector", "label_selector": map[string]string{"selector": "stagely=env-stagelet"}}},
	}, &removed)
	e.waitAction(removed.Actions[0].ID)
	e.mustCall(http.StatusNoContent, http.MethodDelete, path, nil, nil)
}

func TestServerCoveredByASelectorCanNotBeRemovedOnItsOwn(t *testing.T) {
	e := newEnv(t)
	var created createFirewallResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), &created)
	srv := e.createServer(serverRequest("stagely-a1", map[string]string{"stagely": "env-stagelet"}))

	status, code := e.errorCode(http.MethodPost, fmt.Sprintf("/v1/firewalls/%d/actions/remove_from_resources", created.Firewall.ID), map[string]any{
		"remove_from": []map[string]any{{"type": "server", "server": map[string]int64{"id": srv.ID}}},
	})

	if status != http.StatusUnprocessableEntity || code != "firewall_managed_by_label_selector" {
		t.Fatalf("remove = %d %s, want 422 firewall_managed_by_label_selector", status, code)
	}
}

func TestRelabelingAServerMovesItOutOfTheFirewall(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/firewalls", stageletFirewall("172.31.0.9"), nil)
	srv := e.createServer(serverRequest("stagely-a1", map[string]string{"stagely": "env-stagelet"}))

	e.mustCall(http.StatusOK, http.MethodPut, fmt.Sprintf("/v1/servers/%d", srv.ID), map[string]any{"labels": map[string]string{}}, nil)

	eventually(t, "the machine drops its firewall", func() bool {
		fw := e.machines.lastFirewall(srv.ID)
		return fw == nil && len(e.serverByID(srv.ID).PublicNet.Firewalls) == 0
	})
}
