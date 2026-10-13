package gcp

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func TestInsertInstanceRunsWhenItsMachineStarts(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	userData := "#cloud-config\nruncmd: [true]\n"
	var op operationJSON

	e.mustCall(http.StatusOK, http.MethodPost, computePath+"/zones/"+testZone+"/instances", instanceBody("web-1", map[string]any{
		"metadata": map[string]any{"items": []map[string]string{{"key": "user-data", "value": userData}, {"key": "ssh-keys", "value": "root:" + agentKey}}},
		"tags":     map[string]any{"items": []string{"web"}},
		"labels":   map[string]string{"elprovo": "web"},
	}), &op)

	if op.Status != "RUNNING" || op.OperationType != "insert" || op.Zone != computeBase+"projects/"+testProject+"/zones/"+testZone {
		t.Fatalf("insert operation = %+v", op)
	}
	if got := e.instance("web-1"); got.Status != "PROVISIONING" {
		t.Fatalf("instance while its machine is created = %s, want PROVISIONING", got.Status)
	}
	release()
	if op = e.wait(op); op.Error != nil || op.Progress != 100 || op.EndTime == "" {
		t.Fatalf("insert operation after the machine started = %+v", op)
	}
	got := e.instance("web-1")
	nic := got.NetworkInterfaces[0]
	if got.Status != "RUNNING" || nic.NetworkIP != "172.30.255.254" || len(nic.AccessConfigs) != 1 || nic.AccessConfigs[0].NatIP != "172.31.255.254" {
		t.Fatalf("instance = %s %+v", got.Status, nic)
	}
	if nic.Subnetwork != computeBase+"projects/"+testProject+"/regions/us-central1/subnetworks/default" {
		t.Fatalf("subnetwork = %s, want the auto subnetwork of the default network", nic.Subnetwork)
	}
	_, spec, _ := e.machines.machineOf(got.ID)
	if spec.Hosts["metadata.google.internal"] != "169.254.169.254" {
		t.Fatalf("machine hosts = %v; want the metadata server's name", spec.Hosts)
	}
	if spec.Seed.UserData != userData || !slices.Equal(spec.Seed.PublicKeys, []string{agentKey}) || spec.Hostname != "web-1" ||
		spec.Seed.InstanceID != got.ID || spec.Cloud != "gcp" || spec.Resource != got.ID || spec.Image != "" {
		t.Fatalf("machine spec = %+v", spec)
	}
	disk := got.Disks[0]
	if !disk.Boot || !disk.AutoDelete || disk.Source != computeBase+"projects/"+testProject+"/zones/"+testZone+"/disks/web-1" || disk.DiskSizeGb != "10" {
		t.Fatalf("boot disk = %+v", disk)
	}
}

func TestInstanceMetadataServesWhatTheGuestReads(t *testing.T) {
	e := newEnv(t)
	got := e.createInstance("web-1", map[string]any{
		"metadata": map[string]any{"items": []map[string]string{{"key": "user-data", "value": "#cloud-config\n"}}},
		"tags":     map[string]any{"items": []string{"web", "preview"}},
	})
	_, spec, _ := e.machines.machineOf(got.ID)

	docs := spec.Seed.Metadata("10.0.0.5", "203.0.113.5")

	const base = "/computeMetadata/v1/"
	want := map[string]string{
		base + "instance/id":                                                got.ID,
		base + "instance/name":                                              "web-1",
		base + "instance/hostname":                                          "web-1." + testZone + ".c." + testProject + ".internal",
		base + "instance/tags":                                              `["web","preview"]`,
		base + "instance/attributes/user-data":                              "#cloud-config\n",
		base + "instance/network-interfaces/0/ip":                           "10.0.0.5",
		base + "instance/network-interfaces/0/access-configs/0/external-ip": "203.0.113.5",
		base + "project/project-id":                                         testProject,
	}
	for path, value := range want {
		if docs[path] != value {
			t.Errorf("%s = %q, want %q", path, docs[path], value)
		}
	}
}

func TestDeleteInstanceRemovesItsMachineBeforeGetAnswers404(t *testing.T) {
	e := newEnv(t)
	got := e.createInstance("web-1", nil)
	release := e.machines.hold("Delete")
	var op operationJSON

	e.mustCall(http.StatusOK, http.MethodDelete, computePath+"/zones/"+testZone+"/instances/web-1", nil, &op)

	if state := e.instance("web-1"); state.Status != "STOPPING" {
		t.Fatalf("instance while its machine is deleted = %s, want STOPPING", state.Status)
	}
	release()
	if op = e.wait(op); op.Error != nil {
		t.Fatalf("delete operation = %+v", op)
	}
	e.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/instances/web-1", nil)
	e.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/disks/web-1", nil)
	if _, _, exists := e.machines.machineOf(got.ID); exists {
		t.Fatal("the machine is still there after the delete operation finished")
	}
}

func TestInsertThatCannotStartAMachineLeavesNothing(t *testing.T) {
	e := newEnv(t)
	e.machines.fail("Create", errors.New("docker create: no space left on device"))
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodPost, computePath+"/zones/"+testZone+"/instances", instanceBody("web-1", nil), &op)

	op = e.wait(op)

	if op.Error == nil || op.Error.Errors[0].Code != "INTERNAL_ERROR" || op.HTTPErrorStatusCode != http.StatusInternalServerError {
		t.Fatalf("insert operation = %+v, want an error", op)
	}
	e.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/instances/web-1", nil)
	e.fail(http.StatusNotFound, http.MethodGet, computePath+"/zones/"+testZone+"/disks/web-1", nil)
}

func TestDeleteThatCannotRemoveTheMachineKeepsTheInstance(t *testing.T) {
	e := newEnv(t)
	e.createInstance("web-1", nil)
	e.machines.fail("Delete", errors.New("docker rm: device busy"))
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodDelete, computePath+"/zones/"+testZone+"/instances/web-1", nil, &op)

	op = e.wait(op)

	if op.Error == nil {
		t.Fatalf("delete operation = %+v, want an error", op)
	}
	if got := e.instance("web-1"); got.Status != "RUNNING" {
		t.Fatalf("instance after a failed delete = %s, want RUNNING", got.Status)
	}
	e.remove(computePath + "/zones/" + testZone + "/instances/web-1")
}

func TestDeleteWhileTheInstanceIsCreatedIsNotReady(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	defer release()
	var op operationJSON
	e.mustCall(http.StatusOK, http.MethodPost, computePath+"/zones/"+testZone+"/instances", instanceBody("web-1", nil), &op)

	got := e.fail(http.StatusBadRequest, http.MethodDelete, computePath+"/zones/"+testZone+"/instances/web-1", nil)

	if got.Error.Errors[0].Reason != "resourceNotReady" {
		t.Fatalf("delete during insert = %+v", got)
	}
}

func TestListInstancesFiltersByLabelAndPages(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"a-1", "a-2", "a-3"} {
		e.createInstance(name, map[string]any{"labels": map[string]string{"elprovo": "env-a"}})
	}
	e.createInstance("b-1", map[string]any{"labels": map[string]string{"elprovo": "env-b"}})
	var names []string
	token := ""

	for {
		var got list[instanceJSON]
		path := computePath + "/zones/" + testZone + "/instances?maxResults=2&filter=" + strings.ReplaceAll(`labels.elprovo+%3D+"env-a"`, " ", "+")
		if token != "" {
			path += "&pageToken=" + token
		}
		e.mustCall(http.StatusOK, http.MethodGet, path, nil, &got)
		for _, inst := range got.Items {
			names = append(names, inst.Name)
		}
		if token = got.NextPageToken; token == "" {
			break
		}
	}

	if !slices.Equal(names, []string{"a-1", "a-2", "a-3"}) {
		t.Fatalf("instances labeled env-a = %v", names)
	}
}

func TestListInstancesRejectsFiltersItDoesNotSupport(t *testing.T) {
	e := newEnv(t)

	got := e.fail(http.StatusBadRequest, http.MethodGet, computePath+"/zones/"+testZone+"/instances?filter=cpuPlatform+%3D+x", nil)

	if !strings.Contains(got.Error.Message, "not supported by the simulation") {
		t.Fatalf("unsupported filter = %+v", got)
	}
}

func TestInsertInstanceRejectsWhatTheSimulationCannotHonor(t *testing.T) {
	e := newEnv(t)
	tests := map[string]map[string]any{
		"a GPU":            {"guestAccelerators": []map[string]any{{"acceleratorType": "nvidia-l4", "acceleratorCount": 1}}},
		"an unknown field": {"sourceMachineImage": "global/machineImages/x"},
		"a startup script": {"metadata": map[string]any{"items": []map[string]string{{"key": "startup-script", "value": "echo hi"}}}},
		"two disks": {"disks": []map[string]any{
			{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"}},
			{"initializeParams": map[string]any{"diskSizeGb": "20"}},
		}},
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			got := e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/zones/"+testZone+"/instances", instanceBody("web-1", extra))
			if !strings.Contains(got.Error.Message, "not supported by the simulation") {
				t.Fatalf("error = %+v", got)
			}
		})
	}
	var all list[instanceJSON]
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/zones/"+testZone+"/instances", nil, &all)
	if len(all.Items) != 0 {
		t.Fatalf("rejected inserts left instances: %+v", all.Items)
	}
}

func TestInsertInstanceAcceptsFieldsThatHoldNothing(t *testing.T) {
	e := newEnv(t)

	got := e.createInstance("web-1", map[string]any{"params": map[string]any{}, "guestAccelerators": []any{}, "minCpuPlatform": ""})

	if got.Status != "RUNNING" {
		t.Fatalf("instance = %s", got.Status)
	}
}

func TestCustomModeNetworkNeedsASubnetwork(t *testing.T) {
	e := newEnv(t)
	e.insert(computePath+"/global/networks", map[string]any{"name": "env", "autoCreateSubnetworks": false})
	body := instanceBody("web-1", map[string]any{"networkInterfaces": []map[string]any{{"network": "global/networks/env"}}})

	got := e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/zones/"+testZone+"/instances", body)

	if !strings.Contains(got.Error.Message, "Subnetwork should be specified") {
		t.Fatalf("error = %+v", got)
	}
}

func TestInstanceInSubnetworkJoinsItsNetwork(t *testing.T) {
	e := newEnv(t)
	e.insert(computePath+"/global/networks", map[string]any{"name": "env", "autoCreateSubnetworks": false})
	e.insert(computePath+"/regions/us-central1/subnetworks", map[string]any{"name": "env", "network": "projects/" + testProject + "/global/networks/env", "ipCidrRange": "10.42.0.0/24"})

	got := e.createInstance("web-1", map[string]any{"networkInterfaces": []map[string]any{{"subnetwork": "projects/" + testProject + "/regions/us-central1/subnetworks/env"}}})

	nic := got.NetworkInterfaces[0]
	if nic.Network != computeBase+"projects/"+testProject+"/global/networks/env" || len(nic.AccessConfigs) != 0 {
		t.Fatalf("network interface = %+v", nic)
	}
	_, spec, _ := e.machines.machineOf(got.ID)
	anywhere := []string{"0.0.0.0/0"}
	allEgress := []machines.Rule{{Protocol: "tcp", Peers: anywhere}, {Protocol: "udp", Peers: anywhere}, {Protocol: "icmp", Peers: anywhere}}
	if len(spec.Firewall.Inbound) != 0 || !slices.EqualFunc(spec.Firewall.Outbound, allEgress, ruleEqual) {
		t.Fatalf("firewall of an instance in a network without rules = %+v, want no inbound and all outbound", spec.Firewall)
	}
}

func TestFirewallRulesFollowTargetAndSourceTags(t *testing.T) {
	e := newEnv(t)
	e.insert(computePath+"/global/networks", map[string]any{"name": "env", "autoCreateSubnetworks": false})
	e.insert(computePath+"/regions/us-central1/subnetworks", map[string]any{"name": "env", "network": "global/networks/env", "ipCidrRange": "10.42.0.0/24"})
	nic := map[string]any{"networkInterfaces": []map[string]any{{"subnetwork": "regions/us-central1/subnetworks/env"}}}
	agent := e.createInstance("agent", merge(map[string]any{"tags": map[string]any{"items": []string{"agent"}}}, nic))
	preview := e.createInstance("preview", merge(map[string]any{"tags": map[string]any{"items": []string{"preview"}}}, nic))

	e.insert(computePath+"/global/firewalls", map[string]any{
		"name": "previews", "network": "global/networks/env", "direction": "INGRESS",
		"allowed": []map[string]any{{"IPProtocol": "tcp", "ports": []string{"1-65535"}}}, "sourceTags": []string{"agent"}, "targetTags": []string{"preview"},
	})

	want := []machines.Rule{{Protocol: "tcp", Peers: []string{agent.NetworkInterfaces[0].NetworkIP}}}
	eventually(t, "the preview's machine gets the rule", func() bool {
		fw := e.machines.lastFirewall(preview.ID)
		return fw != nil && slices.EqualFunc(fw.Inbound, want, ruleEqual)
	})
	if fw := e.machines.lastFirewall(agent.ID); len(fw.Inbound) != 0 {
		t.Fatalf("agent firewall = %+v, want no inbound rule", fw.Inbound)
	}
	e.remove(computePath + "/global/firewalls/previews")
	eventually(t, "the preview's machine loses the rule", func() bool { return len(e.machines.lastFirewall(preview.ID).Inbound) == 0 })
}

func ruleEqual(a, b machines.Rule) bool {
	return a.Protocol == b.Protocol && a.Ports == b.Ports && slices.Equal(a.Peers, b.Peers)
}

func TestFirewallRejectsDenyRules(t *testing.T) {
	e := newEnv(t)

	got := e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/global/firewalls", map[string]any{
		"name": "deny", "network": "global/networks/default", "denied": []map[string]any{{"IPProtocol": "tcp"}},
	})

	if !strings.Contains(got.Error.Message, "not supported by the simulation") {
		t.Fatalf("error = %+v", got)
	}
}

func TestStaticAddressReachesItsInstanceAndIsReleasedWithIt(t *testing.T) {
	e := newEnv(t)
	e.insert(computePath+"/regions/us-central1/addresses", map[string]any{"name": "gateway"})
	var addr addressJSON
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/regions/us-central1/addresses/gateway", nil, &addr)
	if addr.Address != "172.31.128.0" || addr.Status != "RESERVED" || addr.AddressType != "EXTERNAL" {
		t.Fatalf("new address = %+v", addr)
	}

	got := e.createInstance("gateway", map[string]any{"networkInterfaces": []map[string]any{{
		"network": "global/networks/default", "accessConfigs": []map[string]any{{"natIP": addr.Address}},
	}}})

	if nat := got.NetworkInterfaces[0].AccessConfigs[0].NatIP; nat != addr.Address {
		t.Fatalf("natIP = %s, want the static address %s", nat, addr.Address)
	}
	if extra := e.machines.addressesOf(got.ID); !slices.Equal(extra, []string{addr.Address}) {
		t.Fatalf("machine's extra addresses = %v, want the static address", extra)
	}
	e.mustCall(http.StatusOK, http.MethodGet, computePath+"/regions/us-central1/addresses/gateway", nil, &addr)
	if addr.Status != "IN_USE" || len(addr.Users) != 1 {
		t.Fatalf("address in use = %+v", addr)
	}
	e.fail(http.StatusBadRequest, http.MethodDelete, computePath+"/regions/us-central1/addresses/gateway", nil)
	e.remove(computePath + "/zones/" + testZone + "/instances/gateway")
	e.remove(computePath + "/regions/us-central1/addresses/gateway")
}

func TestInstanceRejectsAnAddressThatIsNotReserved(t *testing.T) {
	e := newEnv(t)

	got := e.fail(http.StatusBadRequest, http.MethodPost, computePath+"/zones/"+testZone+"/instances", instanceBody("web-1", map[string]any{
		"networkInterfaces": []map[string]any{{"network": "global/networks/default", "accessConfigs": []map[string]any{{"natIP": "203.0.113.9"}}}},
	}))

	if !strings.Contains(got.Error.Message, "not reserved") {
		t.Fatalf("error = %+v", got)
	}
}
