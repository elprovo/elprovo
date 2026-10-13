package digitalocean

import (
	"net/http"
	"slices"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func TestFirewallOnATagIsEnforcedOnTaggedDroplets(t *testing.T) {
	e := newEnv(t)
	web := e.createDroplet("web-1", "web")
	agent := e.createDroplet("agent-1", "agent")
	var created struct {
		Firewall firewallJSON `json:"firewall"`
	}

	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/firewalls", map[string]any{
		"name": "web-in",
		"tags": []string{"web"},
		"inbound_rules": []map[string]any{
			{"protocol": "tcp", "ports": "22", "sources": map[string]any{"tags": []string{"agent"}}},
			{"protocol": "icmp", "sources": map[string]any{"addresses": []string{"0.0.0.0/0", "::/0"}}},
		},
		"outbound_rules": []map[string]any{{"protocol": "tcp", "ports": "all", "destinations": map[string]any{"addresses": []string{"0.0.0.0/0"}}}},
	}, &created)

	want := &machines.Firewall{
		Inbound: []machines.Rule{
			{Protocol: "tcp", Ports: "22", Peers: []string{agent.Networks.V4[0].IPAddress, agent.Networks.V4[1].IPAddress}},
			{Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
		},
		Outbound: []machines.Rule{{Protocol: "tcp", Peers: []string{"0.0.0.0/0"}}},
	}
	eventually(t, "the web droplet's machine enforces the firewall", func() bool {
		got, _ := e.machines.lastFirewall(web.ID)
		return policyKey(got) == policyKey(want)
	})
	if got, _ := e.machines.lastFirewall(agent.ID); got != nil {
		t.Fatalf("the agent droplet got a firewall: %+v", got)
	}
	var read struct {
		Firewall firewallJSON `json:"firewall"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/firewalls/"+created.Firewall.ID, nil, &read)
	if read.Firewall.Status != "succeeded" || len(read.Firewall.PendingChanges) != 0 {
		t.Fatalf("firewall after sync = %s %+v", read.Firewall.Status, read.Firewall.PendingChanges)
	}
	if r := read.Firewall.InboundRules[0]; r.Action != "allow" || r.Ports != "22" || !slices.Equal(r.Sources.Tags, []string{"agent"}) {
		t.Fatalf("inbound rule = %+v; want the request echoed with action allow", r)
	}
	if r := read.Firewall.OutboundRules[0]; r.Ports != "all" {
		t.Fatalf("outbound ports = %q, want them echoed as sent", r.Ports)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/firewalls/"+created.Firewall.ID, nil, nil)
	eventually(t, "the web droplet's firewall is removed", func() bool {
		got, ok := e.machines.lastFirewall(web.ID)
		return ok && got == nil
	})
}

func TestFirewallFollowsDropletsAddedAndRemoved(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	var created struct {
		Firewall firewallJSON `json:"firewall"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/firewalls", map[string]any{
		"name":          "ssh",
		"inbound_rules": []map[string]any{{"protocol": "tcp", "ports": "22", "sources": map[string]any{"addresses": []string{"10.0.0.0/8"}}}},
	}, &created)
	path := "/v2/firewalls/" + created.Firewall.ID + "/droplets"

	e.mustCall(http.StatusNoContent, http.MethodPost, path, map[string]any{"droplet_ids": []int64{d.ID}}, nil)
	eventually(t, "the droplet gets the firewall", func() bool {
		got, _ := e.machines.lastFirewall(d.ID)
		return got != nil && len(got.Inbound) == 1
	})
	e.mustCall(http.StatusNoContent, http.MethodDelete, path, map[string]any{"droplet_ids": []int64{d.ID}}, nil)
	eventually(t, "the droplet loses the firewall", func() bool {
		got, ok := e.machines.lastFirewall(d.ID)
		return ok && got == nil
	})
}

func TestNewDropletBootsWithTheFirewallOfItsTag(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/tags", map[string]string{"name": "web"}, nil)
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/firewalls", map[string]any{
		"name": "web", "tags": []string{"web"},
		"inbound_rules": []map[string]any{{"protocol": "tcp", "ports": "443", "sources": map[string]any{"addresses": []string{"0.0.0.0/0"}}}},
	}, nil)

	d := e.createDroplet("web-1", "web")

	if fw := e.machines.spec(d.ID).Firewall; fw == nil || fw.Inbound[0].Ports != "443" {
		t.Fatalf("machine booted with firewall %+v, want the tag's firewall", fw)
	}
}

func TestFirewallRejectsWhatTheSimulationCannotEnforce(t *testing.T) {
	e := newEnv(t)
	rule := func(r map[string]any) map[string]any {
		return map[string]any{"name": "fw", "inbound_rules": []map[string]any{r}}
	}
	for name, req := range map[string]map[string]any{
		"no rules":      {"name": "fw"},
		"bad name":      rule(map[string]any{"protocol": "tcp", "ports": "22", "sources": map[string]any{}}),
		"deny rule":     rule(map[string]any{"protocol": "tcp", "ports": "22", "action": "deny", "sources": map[string]any{}}),
		"bad ports":     rule(map[string]any{"protocol": "tcp", "ports": "70000", "sources": map[string]any{}}),
		"unknown tag":   rule(map[string]any{"protocol": "tcp", "ports": "22", "sources": map[string]any{"tags": []string{"nope"}}}),
		"load balancer": rule(map[string]any{"protocol": "tcp", "ports": "80", "sources": map[string]any{"load_balancer_uids": []string{"lb"}}}),
		"bad address":   rule(map[string]any{"protocol": "tcp", "ports": "22", "sources": map[string]any{"addresses": []string{"10.0.0.0/33"}}}),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "bad name" {
				req["name"] = "-fw"
			}
			if status := e.call(http.MethodPost, "/v2/firewalls", req, nil); status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", status)
			}
		})
	}
}

func TestFirewallUpdateReplacesEverySetting(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	var created struct {
		Firewall firewallJSON `json:"firewall"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/firewalls", map[string]any{
		"name": "fw", "droplet_ids": []int64{d.ID},
		"inbound_rules": []map[string]any{{"protocol": "tcp", "ports": "22", "sources": map[string]any{"addresses": []string{"10.0.0.1"}}}},
	}, &created)
	var updated struct {
		Firewall firewallJSON `json:"firewall"`
	}
	e.mustCall(http.StatusOK, http.MethodPut, "/v2/firewalls/"+created.Firewall.ID, map[string]any{
		"name":           "fw-2",
		"outbound_rules": []map[string]any{{"protocol": "udp", "ports": "53", "destinations": map[string]any{"addresses": []string{"10.0.0.2"}}}},
	}, &updated)
	if updated.Firewall.Name != "fw-2" || len(updated.Firewall.InboundRules) != 0 || len(updated.Firewall.DropletIDs) != 0 {
		t.Fatalf("updated = %+v; want the settings the request left out reset", updated.Firewall)
	}
	if status := e.call(http.MethodGet, "/v2/firewalls/"+created.Firewall.ID+"x", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown firewall = %d", status)
	}
}
