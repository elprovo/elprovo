package aws

import (
	"encoding/json"
	"strings"
)

// instanceDocument is what an instance's metadata service knows about it.
type instanceDocument struct {
	accountID  string
	instance   instance
	image      string
	publicKey  string
	groupNames []string
	userData   string
}

// metadata returns the documents that EC2's instance metadata service
// serves under /latest, by path, with the addresses the machine got.
func (d instanceDocument) metadata(privateIPv4, publicIPv4 string) map[string]string {
	const base = "/latest/meta-data/"
	i := d.instance
	privateDNS, publicDNS := instanceDNS(i.Region, privateIPv4, publicIPv4)
	identity, _ := json.Marshal(map[string]any{
		"accountId": d.accountID, "architecture": "x86_64", "availabilityZone": i.AZ, "imageId": d.image,
		"instanceId": i.ID, "instanceType": i.InstanceType, "pendingTime": timestamp(i.LaunchTime), "privateIp": privateIPv4,
		"region": i.Region, "version": "2017-09-30",
	})
	mac := base + "network/interfaces/macs/" + i.MAC + "/"
	docs := map[string]string{
		base + "ami-id":                              d.image,
		base + "instance-id":                         i.ID,
		base + "instance-type":                       i.InstanceType,
		base + "hostname":                            privateDNS,
		base + "local-hostname":                      privateDNS,
		base + "local-ipv4":                          privateIPv4,
		base + "mac":                                 i.MAC,
		base + "reservation-id":                      i.ReservationID,
		base + "security-groups":                     strings.Join(d.groupNames, "\n"),
		base + "placement/availability-zone":         i.AZ,
		base + "placement/availability-zone-id":      zoneID(i.AZ),
		base + "placement/region":                    i.Region,
		mac + "device-number":                        "0",
		mac + "interface-id":                         i.ENIID,
		mac + "local-ipv4s":                          privateIPv4,
		mac + "mac":                                  i.MAC,
		mac + "security-group-ids":                   strings.Join(i.SecurityGroupIDs, "\n"),
		mac + "subnet-id":                            i.SubnetID,
		mac + "vpc-id":                               i.VPCID,
		"/latest/dynamic/instance-identity/document": string(identity),
	}
	if publicIPv4 != "" {
		docs[base+"public-ipv4"] = publicIPv4
		docs[base+"public-hostname"] = publicDNS
		docs[mac+"public-ipv4s"] = publicIPv4
	}
	if d.publicKey != "" {
		docs[base+"public-keys/0/openssh-key"] = d.publicKey
	}
	if d.userData != "" {
		docs["/latest/user-data"] = d.userData
	}
	return docs
}
