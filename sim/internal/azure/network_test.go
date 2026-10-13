package azure

import (
	"net/http"
	"testing"
)

func TestInterfaceGetsAPrivateAddressWhenItIsCreated(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")

	first := e.nic("rg", "a", subnet, "", "")
	second := e.nic("rg", "b", subnet, "", "")

	if first.privateIP() != "172.30.255.254" || second.privateIP() != "172.30.255.253" {
		t.Fatalf("private addresses = %s, %s; want the top of the private network", first.privateIP(), second.privateIP())
	}
	again := e.nic2("rg", "a", subnet)
	if again.privateIP() != first.privateIP() {
		t.Fatalf("an update moved the address from %s to %s", first.privateIP(), again.privateIP())
	}
}

// nic2 updates an existing interface with the same configuration.
func (e *testEnv) nic2(group, name, subnetID string) nicJSON {
	e.t.Helper()
	var out nicJSON
	e.mustCall(http.StatusOK, http.MethodPut, resourcePath(group, typeNIC, name)+apiVersion, map[string]any{
		"location": "westeurope",
		"properties": map[string]any{"ipConfigurations": []any{map[string]any{
			"name": "primary", "properties": map[string]any{"subnet": map[string]string{"id": subnetID}},
		}}},
	}, &out)
	return out
}

func TestPublicIPsComeFromTheMiddleOfThePublicNetwork(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)

	_, first := e.publicIP("rg", "a")
	_, second := e.publicIP("rg", "b")

	if first != "172.31.128.0" || second != "172.31.128.1" {
		t.Fatalf("addresses = %s, %s", first, second)
	}
}

func TestResourcesInUseCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pip, _ := e.publicIP("rg", "ip")
	nsgID := e.nsg("rg", "fw", rule("ssh", 100, "Inbound", "Allow", "Tcp", "22", "*"))
	e.nic("rg", "nic", subnet, pip, nsgID)

	e.mustFail(http.StatusBadRequest, "PublicIPAddressCannotBeDeleted", http.MethodDelete, resourcePath("rg", typePublicIP, "ip")+apiVersion, nil)
	e.mustFail(http.StatusBadRequest, "InUseNetworkSecurityGroupCannotBeDeleted", http.MethodDelete, resourcePath("rg", typeNSG, "fw")+apiVersion, nil)
	e.mustFail(http.StatusBadRequest, "InUseSubnetCannotBeDeleted", http.MethodDelete, resourcePath("rg", typeVNet, "net")+"/subnets/previews"+apiVersion, nil)
	e.mustFail(http.StatusBadRequest, "InUseSubnetCannotBeDeleted", http.MethodDelete, resourcePath("rg", typeVNet, "net")+apiVersion, nil)

	e.wait(e.mustCall(http.StatusAccepted, http.MethodDelete, resourcePath("rg", typeNIC, "nic")+apiVersion, nil, nil))
	e.wait(e.mustCall(http.StatusAccepted, http.MethodDelete, resourcePath("rg", typePublicIP, "ip")+apiVersion, nil, nil))
	e.mustCall(http.StatusNoContent, http.MethodDelete, resourcePath("rg", typePublicIP, "ip")+apiVersion, nil, nil)
}

func TestPublicIPCannotServeTwoInterfaces(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pip, _ := e.publicIP("rg", "ip")
	e.nic("rg", "a", subnet, pip, "")

	e.mustFail(http.StatusBadRequest, "PublicIPAddressInUse", http.MethodPut, resourcePath("rg", typeNIC, "b")+apiVersion, map[string]any{
		"location": "westeurope",
		"properties": map[string]any{"ipConfigurations": []any{map[string]any{"name": "primary", "properties": map[string]any{
			"subnet": map[string]string{"id": subnet}, "publicIPAddress": map[string]string{"id": pip},
		}}}},
	})
}

func TestSubnetsMustFitTheNetworkWithoutOverlap(t *testing.T) {
	e := newEnv(t)
	e.network("rg")
	path := resourcePath("rg", typeVNet, "net") + "/subnets/"

	e.mustFail(http.StatusBadRequest, "NetcfgSubnetRangeOutsideVnet", http.MethodPut, path+"outside"+apiVersion, map[string]any{"properties": map[string]any{"addressPrefix": "10.43.0.0/24"}})
	e.mustFail(http.StatusBadRequest, "NetcfgSubnetRangesOverlap", http.MethodPut, path+"overlap"+apiVersion, map[string]any{"properties": map[string]any{"addressPrefixes": []string{"10.42.1.128/25"}}})
	e.mustCall(http.StatusCreated, http.MethodPut, path+"hosts"+apiVersion, map[string]any{"properties": map[string]any{"addressPrefixes": []string{"10.42.0.0/24"}}}, nil)
}

func TestSecurityRulesAreValidated(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	cases := []struct {
		name  string
		rules []map[string]any
		code  string
	}{
		{"a priority below 100", []map[string]any{rule("low", 99, "Inbound", "Allow", "Tcp", "22", "*")}, "SecurityRuleInvalidPriority"},
		{"two rules with one priority", []map[string]any{rule("a", 200, "Inbound", "Allow", "Tcp", "22", "*"), rule("b", 200, "Inbound", "Deny", "*", "*", "*")}, "SecurityRuleConflict"},
		{"a bad port range", []map[string]any{rule("ports", 200, "Inbound", "Allow", "Tcp", "90-80", "*")}, "SecurityRuleInvalidPortRange"},
		{"an unknown service tag", []map[string]any{rule("tag", 200, "Inbound", "Allow", "Tcp", "22", "Storage")}, "UnsupportedBySimulation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list := []any{}
			for _, r := range c.rules {
				list = append(list, map[string]any{"name": r["name"], "properties": r})
			}
			e.mustFail(http.StatusBadRequest, c.code, http.MethodPut, resourcePath("rg", typeNSG, "fw")+apiVersion, map[string]any{
				"location": "westeurope", "properties": map[string]any{"securityRules": list},
			})
		})
	}
}

func TestSecurityGroupListsItsDefaultRulesAndUsers(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	nsgID := e.nsg("rg", "fw", rule("ssh", 100, "Inbound", "Allow", "tcp", "22", "*"))
	e.nic("rg", "nic", subnet, "", nsgID)

	var got struct {
		Properties nsgProperties `json:"properties"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeNSG, "fw")+apiVersion, nil, &got)

	if len(got.Properties.DefaultSecurityRules) != 6 || len(got.Properties.NetworkInterfaces) != 1 || got.Properties.SecurityRules[0].Properties.Protocol != "Tcp" {
		t.Fatalf("security group = %+v", got.Properties)
	}
}
