package vultr

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

var everyProtocolOut = []machines.Rule{
	{Protocol: "tcp", Peers: []string{"0.0.0.0/0"}},
	{Protocol: "udp", Peers: []string{"0.0.0.0/0"}},
	{Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
}

func TestFirewallGroupAllowsOnlyItsRulesInAndEverythingOut(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup(
		map[string]any{"ip_type": "v4", "protocol": "tcp", "port": "22", "subnet": "203.0.113.0", "subnet_size": 24},
		map[string]any{"ip_type": "v4", "protocol": "tcp", "port": "8000:9000", "subnet": "198.51.100.7", "subnet_size": 32},
		map[string]any{"ip_type": "v6", "protocol": "tcp", "port": "443", "subnet": "::", "subnet_size": 0},
	)

	inst := e.createInstance("web-1", map[string]any{"firewall_group_id": group})

	want := &machines.Firewall{
		Inbound: []machines.Rule{
			{Protocol: "tcp", Ports: "22", Peers: []string{"203.0.113.0/24"}},
			{Protocol: "tcp", Ports: "8000-9000", Peers: []string{"198.51.100.7/32"}},
		},
		Outbound: everyProtocolOut,
	}
	if got := e.machines.spec(inst.ID).Firewall; policyKey(got) != policyKey(want) {
		t.Fatalf("machine booted with %s, want %s", policyKey(got), policyKey(want))
	}
}

func TestFirewallGroupDoesNotFilterTheVPC(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup()
	network := e.createVPC("ewr")

	inst := e.createInstance("web-1", map[string]any{"firewall_group_id": group, "attach_vpc": []string{network.ID}})

	want := &machines.Firewall{
		Inbound: []machines.Rule{
			{Protocol: "tcp", Peers: []string{"172.30.0.0/16"}}, {Protocol: "udp", Peers: []string{"172.30.0.0/16"}}, {Protocol: "icmp", Peers: []string{"172.30.0.0/16"}},
		},
		Outbound: everyProtocolOut,
	}
	if got := e.machines.spec(inst.ID).Firewall; policyKey(got) != policyKey(want) {
		t.Fatalf("machine in a VPC booted with %s, want all traffic from the private network in, %s", policyKey(got), policyKey(want))
	}
}

func TestFirewallRuleChangesReachTheGroupsMachines(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup()
	inst := e.createInstance("web-1", map[string]any{"firewall_group_id": group})
	var rule struct {
		FirewallRule firewallRuleJSON `json:"firewall_rule"`
	}

	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/firewalls/"+group+"/rules", map[string]any{
		"ip_type": "v4", "protocol": "tcp", "port": "1:65535", "subnet": "10.1.2.3", "subnet_size": 32, "notes": "agent",
	}, &rule)

	want := &machines.Firewall{Inbound: []machines.Rule{{Protocol: "tcp", Peers: []string{"10.1.2.3/32"}}}, Outbound: everyProtocolOut}
	eventually(t, "the machine gets the new rule", func() bool {
		got, _ := e.machines.lastFirewall(inst.ID)
		return policyKey(got) == policyKey(want)
	})
	r := rule.FirewallRule
	if r.ID != 1 || r.Action != "accept" || r.Port != "1:65535" || r.Notes != "agent" || r.IPType != "v4" || r.Type != "v4" || r.Source != "" {
		t.Fatalf("rule = %+v", r)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/firewalls/"+group+"/rules/"+strconv.Itoa(r.ID), nil, nil)
	var body errorBody
	if status := e.call(http.MethodGet, "/v2/firewalls/"+group+"/rules/1", nil, &body); status != http.StatusNotFound || body.Error != firewallRuleNotFound {
		t.Fatalf("deleted rule = %d %+v", status, body)
	}
}

func TestDeletingAFirewallGroupOpensItsInstances(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup()
	inst := e.createInstance("web-1", map[string]any{"firewall_group_id": group})
	var read struct {
		FirewallGroup firewallGroupJSON `json:"firewall_group"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/firewalls/"+group, nil, &read)
	if read.FirewallGroup.InstanceCount != 1 || read.FirewallGroup.MaxRuleCount != maxFirewallRules {
		t.Fatalf("group = %+v", read.FirewallGroup)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/firewalls/"+group, nil, nil)

	eventually(t, "the machine's firewall is removed", func() bool {
		got, ok := e.machines.lastFirewall(inst.ID)
		return ok && got == nil
	})
	if got := e.instance(inst.ID); got.FirewallGroupID != "" {
		t.Fatalf("firewall_group_id = %q after the group's deletion", got.FirewallGroupID)
	}
	var body errorBody
	if status := e.call(http.MethodGet, "/v2/firewalls/"+group, nil, &body); status != http.StatusNotFound || body.Status != http.StatusNotFound {
		t.Fatalf("deleted group = %d %+v", status, body)
	}
}

func TestMovingAnInstanceToAnotherGroupChangesItsFirewall(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup(map[string]any{"ip_type": "v4", "protocol": "icmp", "subnet": "0.0.0.0", "subnet_size": 0})
	inst := e.createInstance("web-1", nil)

	e.mustCall(http.StatusAccepted, http.MethodPatch, "/v2/instances/"+inst.ID, map[string]any{"firewall_group_id": group}, nil)

	want := &machines.Firewall{Inbound: []machines.Rule{{Protocol: "icmp", Peers: []string{"0.0.0.0/0"}}}, Outbound: everyProtocolOut}
	eventually(t, "the machine gets the group's firewall", func() bool {
		got, _ := e.machines.lastFirewall(inst.ID)
		return policyKey(got) == policyKey(want)
	})
}

func TestFirewallRuleRejectsWhatTheSimulationCannotEnforce(t *testing.T) {
	e := newEnv(t)
	group := e.createFirewallGroup()
	valid := func() map[string]any {
		return map[string]any{"ip_type": "v4", "protocol": "tcp", "port": "22", "subnet": "10.0.0.0", "subnet_size": 8}
	}
	for name, change := range map[string]func(map[string]any){
		"unknown ip type":      func(r map[string]any) { r["ip_type"] = "v5" },
		"GRE":                  func(r map[string]any) { r["protocol"] = "gre"; delete(r, "port") },
		"unknown protocol":     func(r map[string]any) { r["protocol"] = "sctp" },
		"Cloudflare source":    func(r map[string]any) { r["source"] = "cloudflare" },
		"no subnet size":       func(r map[string]any) { delete(r, "subnet_size") },
		"subnet of other type": func(r map[string]any) { r["subnet"] = "::1" },
		"subnet size too big":  func(r map[string]any) { r["subnet_size"] = 33 },
		"dash range":           func(r map[string]any) { r["port"] = "8000-9000" },
		"no port":              func(r map[string]any) { delete(r, "port") },
		"port on icmp":         func(r map[string]any) { r["protocol"] = "icmp" },
	} {
		t.Run(name, func(t *testing.T) {
			req := valid()
			change(req)
			if status := e.call(http.MethodPost, "/v2/firewalls/"+group+"/rules", req, nil); status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
		})
	}
}
