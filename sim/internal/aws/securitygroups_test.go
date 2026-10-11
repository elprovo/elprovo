package aws

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func (e *testEnv) createGroup(name, vpc string) string {
	e.t.Helper()
	var group struct {
		ID string `xml:"groupId"`
	}
	e.mustEC2(&group, "CreateSecurityGroup", "GroupName", name, "GroupDescription", name, "VpcId", vpc)
	return group.ID
}

// TestStageletGroupAdmitsOnlyTheMembersOfTheAgentGroup is the rule the
// bootstrap module writes: stagelet machines take TCP only from instances
// in the agent's security group, at their private addresses.
func TestStageletGroupAdmitsOnlyTheMembersOfTheAgentGroup(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.10.0.0/16", "10.10.0.0/24")
	agentGroup, stageletGroup := e.createGroup("agent", vpc), e.createGroup("stagelets", vpc)
	e.mustEC2(nil, "AuthorizeSecurityGroupIngress", "GroupId", stageletGroup,
		"IpPermissions.1.IpProtocol", "tcp", "IpPermissions.1.FromPort", "0", "IpPermissions.1.ToPort", "65535",
		"IpPermissions.1.Groups.1.GroupId", agentGroup)
	stagelet := e.runInstance("SubnetId", subnet, "SecurityGroupId.1", stageletGroup)
	e.waitState(stagelet, "running")

	agent := e.runInstance("SubnetId", subnet, "SecurityGroupId.1", agentGroup)
	agentIP := e.waitState(agent, "running").PrivateIP

	want := []machines.Rule{{Protocol: "tcp", Peers: []string{agentIP}}}
	eventually(t, "the stagelet's machine admits the agent", func() bool { return reflect.DeepEqual(e.policyOf(stagelet), want) })
	e.mustEC2(nil, "TerminateInstances", "InstanceId.1", agent)
	eventually(t, "the stagelet's machine no longer admits the terminated agent", func() bool { return len(e.policyOf(stagelet)) == 0 })
}

func TestMachineFirewallFollowsRuleChanges(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.11.0.0/16", "10.11.0.0/24")
	group := e.createGroup("gateway", vpc)
	id := e.runInstance("SubnetId", subnet, "SecurityGroupId.1", group)
	e.waitState(id, "running")
	https := []string{
		"GroupId", group, "IpPermissions.1.IpProtocol", "tcp", "IpPermissions.1.FromPort", "443", "IpPermissions.1.ToPort", "443",
		"IpPermissions.1.IpRanges.1.CidrIp", "0.0.0.0/0",
	}

	e.mustEC2(nil, "AuthorizeSecurityGroupIngress", https...)
	want := []machines.Rule{{Protocol: "tcp", Ports: "443", Peers: []string{"0.0.0.0/0"}}}
	eventually(t, "the machine admits HTTPS", func() bool { return reflect.DeepEqual(e.policyOf(id), want) })

	if code := e.ec2Error("AuthorizeSecurityGroupIngress", https...); code != "InvalidPermission.Duplicate" {
		t.Fatalf("the same rule again = %s, want InvalidPermission.Duplicate", code)
	}
	e.mustEC2(nil, "RevokeSecurityGroupIngress", https...)
	eventually(t, "the machine no longer admits HTTPS", func() bool { return len(e.policyOf(id)) == 0 })
	if code := e.ec2Error("RevokeSecurityGroupIngress", https...); code != "InvalidPermission.NotFound" {
		t.Fatalf("revoke a missing rule = %s, want InvalidPermission.NotFound", code)
	}
}

func TestNewGroupAllowsAllOutboundTrafficAndMergesRulesWithTheSamePorts(t *testing.T) {
	e := newEnv(t)
	vpc, _ := e.vpcWithSubnet("10.12.0.0/16", "10.12.0.0/24")
	group := e.createGroup("ssh", vpc)
	e.mustEC2(nil, "AuthorizeSecurityGroupIngress", "GroupId", group,
		"IpPermissions.1.IpProtocol", "tcp", "IpPermissions.1.FromPort", "22", "IpPermissions.1.ToPort", "22",
		"IpPermissions.1.IpRanges.1.CidrIp", "203.0.113.7/32", "IpPermissions.1.IpRanges.2.CidrIp", "198.51.100.0/24")

	var got struct {
		Ingress []struct {
			Protocol string   `xml:"ipProtocol"`
			FromPort int      `xml:"fromPort"`
			CIDRs    []string `xml:"ipRanges>item>cidrIp"`
		} `xml:"securityGroupInfo>item>ipPermissions>item"`
		Egress []struct {
			Protocol string   `xml:"ipProtocol"`
			CIDRs    []string `xml:"ipRanges>item>cidrIp"`
		} `xml:"securityGroupInfo>item>ipPermissionsEgress>item"`
	}
	e.mustEC2(&got, "DescribeSecurityGroups", "GroupId.1", group)

	if len(got.Ingress) != 1 || got.Ingress[0].FromPort != 22 || strings.Join(got.Ingress[0].CIDRs, ",") != "203.0.113.7/32,198.51.100.0/24" {
		t.Errorf("ingress = %+v, want one SSH permission with both ranges", got.Ingress)
	}
	if len(got.Egress) != 1 || got.Egress[0].Protocol != "-1" || got.Egress[0].CIDRs[0] != "0.0.0.0/0" {
		t.Errorf("egress = %+v, want all traffic to anywhere", got.Egress)
	}
}

func TestGroupInUseCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.13.0.0/16", "10.13.0.0/24")
	used, referenced, referrer := e.createGroup("used", vpc), e.createGroup("referenced", vpc), e.createGroup("referrer", vpc)
	e.mustEC2(nil, "AuthorizeSecurityGroupIngress", "GroupId", referrer, "IpPermissions.1.IpProtocol", "-1", "IpPermissions.1.Groups.1.GroupId", referenced)
	id := e.runInstance("SubnetId", subnet, "SecurityGroupId.1", used)
	e.waitState(id, "running")

	for _, group := range []string{used, referenced} {
		if code := e.ec2Error("DeleteSecurityGroup", "GroupId", group); code != "DependencyViolation" {
			t.Errorf("delete %s = %s, want DependencyViolation", group, code)
		}
	}
	e.mustEC2(nil, "DeleteSecurityGroup", "GroupId", referrer)
	e.mustEC2(nil, "DeleteSecurityGroup", "GroupId", referenced)
}
