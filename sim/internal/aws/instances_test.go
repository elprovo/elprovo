package aws

import (
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func (e *testEnv) importKey(name string) {
	e.t.Helper()
	e.mustEC2(nil, "ImportKeyPair", "KeyName", name, "PublicKeyMaterial", base64.StdEncoding.EncodeToString([]byte(agentKey)))
}

func TestRunInstancesGoesFromPendingToRunningOnItsMachine(t *testing.T) {
	e := newEnv(t)
	e.importKey("agent")
	release := e.machines.hold("Create")
	userData := "#cloud-config\nwrite_files: []\n"

	id := e.runInstance("KeyName", "agent", "UserData", base64.StdEncoding.EncodeToString([]byte(userData)),
		"TagSpecification.1.ResourceType", "instance", "TagSpecification.1.Tag.1.Key", "Name", "TagSpecification.1.Tag.1.Value", "web-1")

	if got := e.instance(id); got.State != "pending" || got.PrivateIP != "" {
		t.Fatalf("instance while its machine is created = %+v, want pending without addresses", got)
	}
	release()
	got := e.waitState(id, "running")
	if got.PrivateIP != "172.30.255.254" || got.PublicIP != "172.31.255.254" || got.SubnetID == "" || got.VPCID == "" {
		t.Fatalf("running instance = %+v, want the machine's addresses in a default subnet", got)
	}
	if !slices.Contains(got.Tags, tag{"Name", "web-1"}) {
		t.Fatalf("tags = %+v, want Name=web-1", got.Tags)
	}
	spec := e.machines.spec(id)
	if spec.Hostname != id || spec.Seed.InstanceID != id || spec.Seed.UserData != userData || !slices.Equal(spec.Seed.PublicKeys, []string{strings.TrimSuffix(agentKey, " agent")}) {
		t.Fatalf("machine seed = %+v", spec.Seed)
	}
	if spec.Cloud != "aws" || spec.Resource != id || spec.Account == "" || spec.Image != "" {
		t.Fatalf("machine labels and image = %q %q %q %q, want aws labels and the base image", spec.Cloud, spec.Account, spec.Resource, spec.Image)
	}
}

func TestInstanceInSubnetWithoutPublicAddressesHasOnlyAPrivateAddress(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.1.0.0/16", "10.1.0.0/24")
	_ = vpc

	id := e.runInstance("SubnetId", subnet)

	got := e.waitState(id, "running")
	if got.PrivateIP == "" || got.PublicIP != "" || got.SubnetID != subnet {
		t.Fatalf("instance = %+v, want a private address only, in %s", got, subnet)
	}
}

func TestRunInstancesRefusesWhatItCannotLaunch(t *testing.T) {
	e := newEnv(t)
	_, otherSubnet := e.vpcWithSubnet("10.2.0.0/16", "10.2.0.0/24")
	var group struct {
		ID string `xml:"groupId"`
	}
	e.mustEC2(&group, "CreateSecurityGroup", "GroupName", "default-vpc-group", "GroupDescription", "d")
	for name, tc := range map[string]struct {
		params []string
		code   string
	}{
		"unknown image":        {[]string{"ImageId", "ami-00000000000000000", "InstanceType", "t3.micro"}, "InvalidAMIID.NotFound"},
		"unknown type":         {[]string{"ImageId", e.stockImage(), "InstanceType", "x9.huge"}, "InvalidParameterValue"},
		"unknown key pair":     {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "KeyName", "nobody"}, "InvalidKeyPair.NotFound"},
		"user data not base64": {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "UserData", "#cloud-config"}, "InvalidParameterValue"},
		"group of other VPC":   {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "SubnetId", otherSubnet, "SecurityGroupId.1", group.ID}, "InvalidParameterValue"},
		"IPv6 addresses":       {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "Ipv6AddressCount", "1"}, "UnknownParameter"},
		"instance profile":     {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "IamInstanceProfile.Name", "admin"}, "UnsupportedOperation"},
		"dedicated tenancy":    {[]string{"ImageId", e.stockImage(), "InstanceType", "t3.micro", "Placement.Tenancy", "dedicated"}, "UnsupportedOperation"},
	} {
		t.Run(name, func(t *testing.T) {
			if code := e.ec2Error("RunInstances", append(tc.params, "MinCount", "1", "MaxCount", "1")...); code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}
}

func TestTerminatedInstanceIsListedUntilItsMachineIsGoneAndForAnHourAfter(t *testing.T) {
	e := newEnv(t)
	id := e.runInstance()
	e.waitState(id, "running")
	release := e.machines.hold("Delete")

	var change struct {
		Current  string `xml:"instancesSet>item>currentState>name"`
		Previous string `xml:"instancesSet>item>previousState>name"`
	}
	e.mustEC2(&change, "TerminateInstances", "InstanceId.1", id)

	if change.Current != "shutting-down" || change.Previous != "running" {
		t.Fatalf("state change = %+v", change)
	}
	if got := e.instance(id); got.State != "shutting-down" || !e.machines.hasMachine(id) {
		t.Fatalf("instance while its machine is deleted = %s, machine exists %v", got.State, e.machines.hasMachine(id))
	}
	release()
	got := e.waitState(id, "terminated")
	if e.machines.hasMachine(id) || got.PrivateIP != "" || got.StateReason != "Client.UserInitiatedShutdown" {
		t.Fatalf("terminated instance = %+v, machine exists %v", got, e.machines.hasMachine(id))
	}
	e.clock.advance(61 * time.Minute)
	if code := e.ec2Error("DescribeInstances", "InstanceId.1", id); code != "InvalidInstanceID.NotFound" {
		t.Fatalf("an hour later DescribeInstances = %s, want InvalidInstanceID.NotFound", code)
	}
}

func TestInstanceTerminatedWhileItsMachineIsCreatedIsDestroyedOnceCreated(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	id := e.runInstance()

	e.mustEC2(nil, "TerminateInstances", "InstanceId.1", id)
	release()

	e.waitState(id, "terminated")
	if e.machines.hasMachine(id) {
		t.Fatal("the machine created after the termination still exists")
	}
}

func TestInstanceWhoseMachineFailsIsTerminatedWithAnInternalError(t *testing.T) {
	e := newEnv(t)
	e.machines.fail("Create", errors.New("docker is down"))

	id := e.runInstance()

	if got := e.waitState(id, "terminated"); got.StateReason != "Server.InternalError" {
		t.Fatalf("state reason = %q, want Server.InternalError", got.StateReason)
	}
}

func TestDescribeInstancesFiltersByTagAndState(t *testing.T) {
	e := newEnv(t)
	tagged := func(name, group string) string {
		return e.runInstance("TagSpecification.1.ResourceType", "instance",
			"TagSpecification.1.Tag.1.Key", "Name", "TagSpecification.1.Tag.1.Value", name,
			"TagSpecification.1.Tag.2.Key", "elprovo-group", "TagSpecification.1.Tag.2.Value", group)
	}
	mine, other, gone := tagged("elprovo-a", "env-a"), tagged("elprovo-b", "env-b"), tagged("elprovo-c", "env-a")
	for _, id := range []string{mine, other, gone} {
		e.waitState(id, "running")
	}
	e.mustEC2(nil, "TerminateInstances", "InstanceId.1", gone)
	e.waitState(gone, "terminated")

	var got describeInstances
	e.mustEC2(&got, "DescribeInstances", "Filter.1.Name", "tag:elprovo-group", "Filter.1.Value.1", "env-a",
		"Filter.2.Name", "instance-state-name", "Filter.2.Value.1", "pending", "Filter.2.Value.2", "running")

	if all := got.all(); len(all) != 1 || all[0].ID != mine {
		t.Fatalf("filtered instances = %+v, want only %s", all, mine)
	}
	if code := e.ec2Error("DescribeInstances", "Filter.1.Name", "no-such-filter", "Filter.1.Value.1", "x"); code != "InvalidParameterValue" {
		t.Fatalf("unknown filter = %s, want InvalidParameterValue", code)
	}
}

func TestStoppedInstanceKeepsItsPrivateAddressAndStartsAgain(t *testing.T) {
	e := newEnv(t)
	id := e.runInstance()
	running := e.waitState(id, "running")

	e.mustEC2(nil, "StopInstances", "InstanceId.1", id)
	stopped := e.waitState(id, "stopped")
	if stopped.PrivateIP != running.PrivateIP || stopped.PublicIP != "" {
		t.Fatalf("stopped instance = %+v, want its private address and no public one", stopped)
	}
	e.mustEC2(nil, "StartInstances", "InstanceId.1", id)
	if got := e.waitState(id, "running"); got.PublicIP == "" {
		t.Fatalf("started instance = %+v, want a public address again", got)
	}
}

func TestMetadataServiceServesTheInstanceItsUserDataAndAddresses(t *testing.T) {
	e := newEnv(t)
	e.importKey("agent")
	id := e.runInstance("KeyName", "agent", "UserData", base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi\n")))
	e.waitState(id, "running")

	docs := e.machines.spec(id).Seed.Metadata("10.0.0.5", "203.0.113.9")

	want := map[string]string{
		"/latest/user-data":                           "#!/bin/sh\necho hi\n",
		"/latest/meta-data/instance-id":               id,
		"/latest/meta-data/local-ipv4":                "10.0.0.5",
		"/latest/meta-data/public-ipv4":               "203.0.113.9",
		"/latest/meta-data/local-hostname":            "ip-10-0-0-5.ec2.internal",
		"/latest/meta-data/placement/region":          testRegion,
		"/latest/meta-data/public-keys/0/openssh-key": strings.TrimSuffix(agentKey, " agent"),
	}
	for path, value := range want {
		if docs[path] != value {
			t.Errorf("%s = %q, want %q", path, docs[path], value)
		}
	}
}

func TestDescribeInstanceAttributeAnswersWhatTerraformReads(t *testing.T) {
	e := newEnv(t)
	userData := base64.StdEncoding.EncodeToString([]byte("#cloud-config\n"))
	id := e.runInstance("UserData", userData, "DisableApiTermination", "true")
	for attribute, want := range map[string]string{
		"userData":                          "<userData><value>" + userData + "</value></userData>",
		"disableApiTermination":             "<disableApiTermination><value>true</value></disableApiTermination>",
		"disableApiStop":                    "<disableApiStop><value>false</value></disableApiStop>",
		"instanceInitiatedShutdownBehavior": "<instanceInitiatedShutdownBehavior><value>stop</value></instanceInitiatedShutdownBehavior>",
	} {
		status, body := e.ec2("DescribeInstanceAttribute", "InstanceId", id, "Attribute", attribute)
		if status != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("%s = %d %s, want %s", attribute, status, body, want)
		}
	}
	if code := e.ec2Error("TerminateInstances", "InstanceId.1", id); code != "OperationNotPermitted" {
		t.Fatalf("terminate with termination protection = %s, want OperationNotPermitted", code)
	}
}

func TestUnknownActionIsRefused(t *testing.T) {
	e := newEnv(t)

	if code := e.ec2Error("RequestSpotInstances"); code != "InvalidAction" {
		t.Fatalf("code = %s, want InvalidAction", code)
	}
}

// policyOf returns the inbound rules of the instance's machine.
func (e *testEnv) policyOf(id string) []machines.Rule {
	e.t.Helper()
	fw := e.machines.lastFirewall(id)
	if fw == nil {
		return nil
	}
	return fw.Inbound
}
