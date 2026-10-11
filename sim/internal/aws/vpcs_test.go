package aws

import (
	"net/http"
	"strings"
	"testing"
)

// vpcWithSubnet creates a VPC and a subnet in it and returns their ids.
func (e *testEnv) vpcWithSubnet(vpcCIDR, subnetCIDR string) (string, string) {
	e.t.Helper()
	var vpc struct {
		ID string `xml:"vpc>vpcId"`
	}
	e.mustEC2(&vpc, "CreateVpc", "CidrBlock", vpcCIDR)
	var subnet struct {
		ID string `xml:"subnet>subnetId"`
	}
	e.mustEC2(&subnet, "CreateSubnet", "VpcId", vpc.ID, "CidrBlock", subnetCIDR)
	return vpc.ID, subnet.ID
}

func TestEachRegionHasADefaultVPCWithPublicSubnetsAndARouteToTheInternet(t *testing.T) {
	e := newEnv(t)
	var vpcs struct {
		VPCs []struct {
			ID   string `xml:"vpcId"`
			CIDR string `xml:"cidrBlock"`
		} `xml:"vpcSet>item"`
	}
	e.mustEC2(&vpcs, "DescribeVpcs", "Filter.1.Name", "is-default", "Filter.1.Value.1", "true")
	if len(vpcs.VPCs) != 1 || vpcs.VPCs[0].CIDR != "172.31.0.0/16" {
		t.Fatalf("default VPCs = %+v", vpcs.VPCs)
	}
	id := vpcs.VPCs[0].ID

	var subnets struct {
		Subnets []struct {
			Zone        string `xml:"availabilityZone"`
			MapPublicIP bool   `xml:"mapPublicIpOnLaunch"`
		} `xml:"subnetSet>item"`
	}
	e.mustEC2(&subnets, "DescribeSubnets", "Filter.1.Name", "vpc-id", "Filter.1.Value.1", id, "Filter.2.Name", "default-for-az", "Filter.2.Value.1", "true")
	if len(subnets.Subnets) != 3 || !subnets.Subnets[0].MapPublicIP || subnets.Subnets[0].Zone != "us-east-1a" {
		t.Fatalf("default subnets = %+v", subnets.Subnets)
	}
	var tables struct {
		Routes []struct {
			Destination string `xml:"destinationCidrBlock"`
			Gateway     string `xml:"gatewayId"`
		} `xml:"routeTableSet>item>routeSet>item"`
	}
	e.mustEC2(&tables, "DescribeRouteTables", "Filter.1.Name", "vpc-id", "Filter.1.Value.1", id, "Filter.2.Name", "association.main", "Filter.2.Value.1", "true")
	if len(tables.Routes) != 2 || tables.Routes[1].Destination != "0.0.0.0/0" || tables.Routes[1].Gateway[:4] != "igw-" {
		t.Fatalf("main route table routes = %+v", tables.Routes)
	}
}

func TestNewVPCComesWithAMainRouteTableADefaultGroupAndANetworkACL(t *testing.T) {
	e := newEnv(t)
	var vpc struct {
		ID    string `xml:"vpc>vpcId"`
		State string `xml:"vpc>state"`
	}
	e.mustEC2(&vpc, "CreateVpc", "CidrBlock", "10.20.0.0/16")

	byVPC := []string{"Filter.1.Name", "vpc-id", "Filter.1.Value.1", vpc.ID}
	var tables struct {
		Routes []string `xml:"routeTableSet>item>routeSet>item>gatewayId"`
	}
	e.mustEC2(&tables, "DescribeRouteTables", byVPC...)
	var groups struct {
		Names []string `xml:"securityGroupInfo>item>groupName"`
	}
	e.mustEC2(&groups, "DescribeSecurityGroups", byVPC...)
	var acls struct {
		Default []bool `xml:"networkAclSet>item>default"`
	}
	e.mustEC2(&acls, "DescribeNetworkAcls", byVPC...)
	if vpc.State != "pending" || len(tables.Routes) != 1 || tables.Routes[0] != "local" || len(groups.Names) != 1 || groups.Names[0] != "default" || len(acls.Default) != 1 || !acls.Default[0] {
		t.Fatalf("new VPC %s: routes %v, groups %v, ACLs %v", vpc.State, tables.Routes, groups.Names, acls.Default)
	}
}

func TestVPCWithASubnetCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.3.0.0/16", "10.3.1.0/24")

	if code := e.ec2Error("DeleteVpc", "VpcId", vpc); code != "DependencyViolation" {
		t.Fatalf("DeleteVpc with a subnet = %s, want DependencyViolation", code)
	}
	e.mustEC2(nil, "DeleteSubnet", "SubnetId", subnet)
	e.mustEC2(nil, "DeleteVpc", "VpcId", vpc)
	if code := e.ec2Error("DescribeVpcs", "VpcId.1", vpc); code != "InvalidVpcID.NotFound" {
		t.Fatalf("deleted VPC = %s, want InvalidVpcID.NotFound", code)
	}
}

func TestSubnetMustFitItsVPCAndNoOtherSubnet(t *testing.T) {
	e := newEnv(t)
	vpc, _ := e.vpcWithSubnet("10.4.0.0/16", "10.4.0.0/24")

	if code := e.ec2Error("CreateSubnet", "VpcId", vpc, "CidrBlock", "10.5.0.0/24"); code != "InvalidSubnet.Range" {
		t.Errorf("subnet outside its VPC = %s", code)
	}
	if code := e.ec2Error("CreateSubnet", "VpcId", vpc, "CidrBlock", "10.4.0.128/25"); code != "InvalidSubnet.Conflict" {
		t.Errorf("overlapping subnet = %s", code)
	}
}

func TestRouteTableSendsTheInternetToAnAttachedGateway(t *testing.T) {
	e := newEnv(t)
	vpc, subnet := e.vpcWithSubnet("10.6.0.0/16", "10.6.0.0/24")
	var igw struct {
		ID string `xml:"internetGateway>internetGatewayId"`
	}
	e.mustEC2(&igw, "CreateInternetGateway")
	var table struct {
		ID string `xml:"routeTable>routeTableId"`
	}
	e.mustEC2(&table, "CreateRouteTable", "VpcId", vpc)

	if code := e.ec2Error("CreateRoute", "RouteTableId", table.ID, "DestinationCidrBlock", "0.0.0.0/0", "GatewayId", igw.ID); code != "InvalidParameterValue" {
		t.Fatalf("route to a detached gateway = %s, want InvalidParameterValue", code)
	}
	e.mustEC2(nil, "AttachInternetGateway", "InternetGatewayId", igw.ID, "VpcId", vpc)
	e.mustEC2(nil, "CreateRoute", "RouteTableId", table.ID, "DestinationCidrBlock", "0.0.0.0/0", "GatewayId", igw.ID)
	var assoc struct {
		ID string `xml:"associationId"`
	}
	e.mustEC2(&assoc, "AssociateRouteTable", "RouteTableId", table.ID, "SubnetId", subnet)

	if code := e.ec2Error("DeleteRouteTable", "RouteTableId", table.ID); code != "DependencyViolation" {
		t.Fatalf("delete an associated route table = %s, want DependencyViolation", code)
	}
	if code := e.ec2Error("DeleteInternetGateway", "InternetGatewayId", igw.ID); code != "DependencyViolation" {
		t.Fatalf("delete an attached gateway = %s, want DependencyViolation", code)
	}
	e.mustEC2(nil, "DisassociateRouteTable", "AssociationId", assoc.ID)
	e.mustEC2(nil, "DeleteRouteTable", "RouteTableId", table.ID)
	e.mustEC2(nil, "DetachInternetGateway", "InternetGatewayId", igw.ID, "VpcId", vpc)
	e.mustEC2(nil, "DeleteInternetGateway", "InternetGatewayId", igw.ID)
}

func TestCreateRouteTableWithTheSameClientTokenReturnsTheSameTable(t *testing.T) {
	e := newEnv(t)
	vpc, _ := e.vpcWithSubnet("10.7.0.0/16", "10.7.0.0/24")
	var first, second struct {
		ID string `xml:"routeTable>routeTableId"`
	}

	e.mustEC2(&first, "CreateRouteTable", "VpcId", vpc, "ClientToken", "token-1")
	e.mustEC2(&second, "CreateRouteTable", "VpcId", vpc, "ClientToken", "token-1")

	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("route tables = %s and %s, want the same one", first.ID, second.ID)
	}
}

func TestResourcesOfOneRegionAreInvisibleInAnother(t *testing.T) {
	e := newEnv(t)
	vpc, _ := e.vpcWithSubnet("10.8.0.0/16", "10.8.0.0/24")

	status, body := e.ec2In("eu-central-1", "DescribeVpcs", "VpcId.1", vpc)

	if status != http.StatusBadRequest || !strings.Contains(string(body), "InvalidVpcID.NotFound") {
		t.Fatalf("VPC of us-east-1 seen from eu-central-1 = %d %s", status, body)
	}
}
