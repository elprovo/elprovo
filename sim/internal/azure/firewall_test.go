package azure

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func TestStageletSubnetAdmitsOnlyTheAgentAndTheGateway(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	nsgID := e.nsg("rg", "stagelets",
		rule("from-agent-and-gateway", 100, "Inbound", "Allow", "Tcp", "*", "172.30.0.10", "172.30.0.20"),
		rule("deny-other-inbound", 4096, "Inbound", "Deny", "*", "*", "*"))
	e.associate("rg", "stagelets", nsgID)
	n := e.nic("rg", "web-nic", subnet, "", "")

	vm := e.createVM("rg", "web", n.ID)

	want := &machines.Firewall{
		Inbound: []machines.Rule{{Protocol: "tcp", Peers: []string{"172.30.0.10/32", "172.30.0.20/32"}}},
		Outbound: []machines.Rule{
			{Protocol: "tcp", Peers: []string{"0.0.0.0/0"}}, {Protocol: "udp", Peers: []string{"0.0.0.0/0"}}, {Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
		},
	}
	if got := e.machines.specOf(vm.Properties.VMID).Firewall; !reflect.DeepEqual(got, want) {
		t.Fatalf("firewall = %+v, want %+v", got, want)
	}
}

// associate puts the security group on the subnet of the test network.
func (e *testEnv) associate(group, subnet, nsgID string) {
	e.t.Helper()
	e.mustCall(200, "PUT", resourcePath(group, typeVNet, "net")+"/subnets/"+subnet+apiVersion, map[string]any{
		"properties": map[string]any{"addressPrefix": "10.42.1.0/24", "networkSecurityGroup": map[string]string{"id": nsgID}},
	}, nil)
}

func TestVMWithoutPublicIPIsReachedOnlyFromTheNetwork(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")

	vm := e.createVM("rg", "web", n.ID)

	got := e.machines.specOf(vm.Properties.VMID).Firewall
	for _, r := range got.Inbound {
		if !reflect.DeepEqual(r.Peers, []string{"172.30.0.0/16"}) || r.Ports != "" {
			t.Fatalf("inbound rule %+v, want every port from the private network only", r)
		}
	}
	if len(got.Inbound) != 3 {
		t.Fatalf("inbound = %+v, want tcp, udp and icmp from the private network", got.Inbound)
	}
}

func TestStandardPublicIPNeedsASecurityGroupToAdmitTheInternet(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pip, _ := e.publicIP("rg", "ip")
	bare := e.nic("rg", "bare-nic", subnet, pip, "")
	bareVM := e.createVM("rg", "bare", bare.ID)
	pip2, _ := e.publicIP("rg", "ip2")
	web := e.nsg("rg", "web", rule("https", 100, "Inbound", "Allow", "Tcp", "443", "Internet"))
	open := e.nic("rg", "web-nic", subnet, pip2, web)

	webVM := e.createVM("rg", "web", open.ID)

	if in := e.machines.specOf(bareVM.Properties.VMID).Firewall.Inbound; len(in) != 3 || in[0].Peers[0] != "172.30.0.0/16" {
		t.Fatalf("inbound without a security group = %+v, want the private network only", in)
	}
	in := e.machines.specOf(webVM.Properties.VMID).Firewall.Inbound
	https := machines.Rule{Protocol: "tcp", Ports: "443", Peers: []string{"0.0.0.0/1", "128.0.0.0/3", "160.0.0.0/5", "168.0.0.0/6", "172.0.0.0/12", "172.16.0.0/13", "172.24.0.0/14", "172.28.0.0/15", "172.31.0.0/16", "172.32.0.0/11", "172.64.0.0/10", "172.128.0.0/9", "173.0.0.0/8", "174.0.0.0/7", "176.0.0.0/4", "192.0.0.0/2"}}
	found := false
	for _, r := range in {
		found = found || reflect.DeepEqual(r, https)
	}
	if !found {
		t.Fatalf("inbound = %+v, want 443 from every address outside the private network", in)
	}
}

func TestHigherPriorityDenyWinsOverALaterAllow(t *testing.T) {
	scope := netScope{vnet: []span{spanOf(netip.MustParsePrefix("172.30.0.0/16"))}}
	rules := []ruleJSON{
		{Name: "deny-one", Properties: ruleProperties{Protocol: "Tcp", Access: "Deny", Direction: "Inbound", Priority: 100, SourceAddressPrefix: "10.0.0.5", DestinationPortRange: "22", DestinationAddressPrefix: "*"}},
		{Name: "allow-block", Properties: ruleProperties{Protocol: "Tcp", Access: "Allow", Direction: "Inbound", Priority: 200, SourceAddressPrefix: "10.0.0.0/29", DestinationPortRange: "22", DestinationAddressPrefix: "*"}},
	}

	got := evaluate(rules, "Inbound", netip.MustParseAddr("172.30.0.9"), scope)

	tcp := machineRules(region{"tcp": got["tcp"]})
	want := []string{"10.0.0.0/30", "10.0.0.4/32", "10.0.0.6/31"}
	var ssh []string
	for _, r := range tcp {
		if r.Ports == "22" {
			ssh = append(ssh, r.Peers...)
		}
	}
	if !reflect.DeepEqual(ssh, want) {
		t.Fatalf("port 22 peers = %v, want %v (all of 10.0.0.0/29 but 10.0.0.5)", ssh, want)
	}
}

func TestRuleForAnotherDestinationDoesNotApply(t *testing.T) {
	scope := netScope{vnet: []span{spanOf(netip.MustParsePrefix("172.30.0.0/16"))}}
	rules := []ruleJSON{{Name: "elsewhere", Properties: ruleProperties{
		Protocol: "Tcp", Access: "Allow", Direction: "Inbound", Priority: 100, SourceAddressPrefix: "*", DestinationPortRange: "80", DestinationAddressPrefix: "172.30.9.9",
	}}}

	got := machineRules(evaluate(rules, "Inbound", netip.MustParseAddr("172.30.0.9"), scope))

	for _, r := range got {
		if r.Ports == "80" {
			t.Fatalf("a rule for 172.30.9.9 admitted traffic to 172.30.0.9: %+v", r)
		}
	}
}

func TestCIDRsCoverASpanWithTheFewestBlocks(t *testing.T) {
	got := cidrs([]span{{lo: ip("10.0.0.1"), hi: ip("10.0.0.6")}})

	want := []string{"10.0.0.1/32", "10.0.0.2/31", "10.0.0.4/31", "10.0.0.6/32"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cidrs = %v, want %v", got, want)
	}
}

func TestCIDRsJoinAdjacentSpans(t *testing.T) {
	got := cidrs([]span{spanOf(netip.MustParsePrefix("128.0.0.0/1")), spanOf(netip.MustParsePrefix("0.0.0.0/1"))})

	want := []string{"0.0.0.0/0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cidrs = %v, want %v", got, want)
	}
}

func ip(s string) uint32 {
	b := netip.MustParseAddr(s).As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func TestFirewallFollowsASecurityGroupChange(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	nsgID := e.nsg("rg", "fw", rule("ssh", 100, "Inbound", "Allow", "Tcp", "22", "172.30.0.10"), rule("deny", 4096, "Inbound", "Deny", "*", "*", "*"))
	n := e.nic("rg", "web-nic", subnet, "", nsgID)
	vm := e.createVM("rg", "web", n.ID)

	e.nsg2("rg", "fw", rule("ssh", 100, "Inbound", "Allow", "Tcp", "22", "172.30.0.20"), rule("deny", 4096, "Inbound", "Deny", "*", "*", "*"))

	eventually(t, "the machine has the new firewall", func() bool {
		fw := e.machines.lastFirewall(vm.Properties.VMID)
		return fw != nil && len(fw.Inbound) == 1 && reflect.DeepEqual(fw.Inbound[0].Peers, []string{"172.30.0.20/32"})
	})
}

// nsg2 replaces the rules of an existing security group.
func (e *testEnv) nsg2(group, name string, rules ...map[string]any) {
	e.t.Helper()
	list := []any{}
	for _, r := range rules {
		list = append(list, map[string]any{"name": r["name"], "properties": r})
	}
	e.mustCall(200, "PUT", resourcePath(group, typeNSG, name)+apiVersion, map[string]any{"location": "westeurope", "properties": map[string]any{"securityRules": list}}, nil)
}

func TestAgentHostAdmitsOnlyOperatorSSH(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pip, _ := e.publicIP("rg", "agent-ip")
	nsgID := e.nsg("rg", "agent",
		rule("ssh-from-operators", 100, "Inbound", "Allow", "Tcp", "22", "203.0.113.7/32"),
		rule("deny-other-inbound", 4096, "Inbound", "Deny", "*", "*", "*"))
	n := e.nic("rg", "agent-nic", subnet, pip, nsgID)

	vm := e.createVM("rg", "agent", n.ID)

	want := []machines.Rule{{Protocol: "tcp", Ports: "22", Peers: []string{"203.0.113.7/32"}}}
	if got := e.machines.specOf(vm.Properties.VMID).Firewall.Inbound; !reflect.DeepEqual(got, want) {
		t.Fatalf("inbound = %+v, want SSH from the operator only and nothing from the virtual network", got)
	}
}

func TestPrivateSubnetKeepsAVMWithoutPublicIPInsideTheNetwork(t *testing.T) {
	e := newEnv(t)
	e.network("rg")
	e.mustCall(201, "PUT", resourcePath("rg", typeVNet, "net")+"/subnets/private"+apiVersion, map[string]any{
		"properties": map[string]any{"addressPrefix": "10.42.2.0/24", "defaultOutboundAccess": false},
	}, nil)
	subnet := resourcePath("rg", typeVNet, "net") + "/subnets/private"
	pip, _ := e.publicIP("rg", "ip")
	web := e.nsg("rg", "web", rule("ssh", 100, "Inbound", "Allow", "Tcp", "22", "*"))

	inside := e.createVM("rg", "inside", e.nic("rg", "inside-nic", subnet, "", "").ID)
	public := e.createVM("rg", "public", e.nic("rg", "public-nic", subnet, pip, web).ID)

	for _, r := range e.machines.specOf(inside.Properties.VMID).Firewall.Outbound {
		if !reflect.DeepEqual(r.Peers, []string{"172.30.0.0/16"}) {
			t.Fatalf("outbound rule %+v of a machine without public IP in a private subnet, want the private network only", r)
		}
	}
	if out := e.machines.specOf(public.Properties.VMID).Firewall.Outbound; len(out) != 3 || out[0].Peers[0] != "0.0.0.0/0" {
		t.Fatalf("outbound with a public IP = %+v, want everywhere", out)
	}
}
