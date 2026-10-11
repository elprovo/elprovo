package main

import (
	"testing"
)

func TestIptablesRulesAllowPeersAndDropTheRest(t *testing.T) {
	rules, err := iptablesRules(&firewallPolicy{
		Inbound: []firewallRule{
			{Protocol: "tcp", Ports: "22", Peers: []string{"10.0.0.5", "10.1.0.0/16"}},
			{Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
		},
		Outbound: []firewallRule{{Protocol: "udp", Ports: "8000-9000", Peers: []string{"192.168.1.7/24"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `*mangle
:STAGELY-FIREWALL-IN - [0:0]
:STAGELY-FIREWALL-OUT - [0:0]
-A STAGELY-FIREWALL-IN ! -i eth+ -j RETURN
-A STAGELY-FIREWALL-IN -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
-A STAGELY-FIREWALL-IN -p tcp --dport 22 -s 10.0.0.5/32 -j RETURN
-A STAGELY-FIREWALL-IN -p tcp --dport 22 -s 10.1.0.0/16 -j RETURN
-A STAGELY-FIREWALL-IN -p icmp -s 0.0.0.0/0 -j RETURN
-A STAGELY-FIREWALL-IN -j DROP
-A STAGELY-FIREWALL-OUT ! -o eth+ -j RETURN
-A STAGELY-FIREWALL-OUT -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
-A STAGELY-FIREWALL-OUT -p udp --dport 8000:9000 -d 192.168.1.0/24 -j RETURN
-A STAGELY-FIREWALL-OUT -j DROP
COMMIT
`
	if rules != want {
		t.Fatalf("rules:\n%s\nwant:\n%s", rules, want)
	}
}

func TestIptablesRulesWithoutPolicyEmptyTheChains(t *testing.T) {
	rules, err := iptablesRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rules != "*mangle\n:STAGELY-FIREWALL-IN - [0:0]\n:STAGELY-FIREWALL-OUT - [0:0]\nCOMMIT\n" {
		t.Fatalf("rules:\n%s", rules)
	}
}

func TestIptablesRulesRejectInvalidRules(t *testing.T) {
	for name, rule := range map[string]firewallRule{
		"IPv6 peer":      {Protocol: "tcp", Ports: "22", Peers: []string{"::/0"}},
		"port 0":         {Protocol: "tcp", Ports: "0", Peers: []string{"0.0.0.0/0"}},
		"reversed range": {Protocol: "tcp", Ports: "90-80", Peers: []string{"0.0.0.0/0"}},
		"icmp ports":     {Protocol: "icmp", Ports: "22", Peers: []string{"0.0.0.0/0"}},
		"unknown proto":  {Protocol: "sctp", Peers: []string{"0.0.0.0/0"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := iptablesRules(&firewallPolicy{Inbound: []firewallRule{rule}}); err == nil {
				t.Fatal("rule accepted")
			}
		})
	}
}
