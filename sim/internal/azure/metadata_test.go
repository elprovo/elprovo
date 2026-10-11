package azure

import (
	"encoding/json"
	"testing"
)

func TestInstanceMetadataDescribesTheVM(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pip, addr := e.publicIP("rg", "ip")
	n := e.nic("rg", "web-nic", subnet, pip, "")

	vm := e.createVM("rg", "web", n.ID)

	docs := e.machines.specOf(vm.Properties.VMID).Seed.Metadata("", "")
	var instance struct {
		Compute struct {
			Name, VMID, ResourceGroupName, SubscriptionID, Location, VMSize string
			OSProfile                                                       struct{ AdminUsername string }
		}
		Network struct {
			Interface []struct {
				IPv4 struct {
					IPAddress []struct{ PrivateIPAddress, PublicIPAddress string }
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(docs["/metadata/instance"]), &instance); err != nil {
		t.Fatal(err)
	}
	c := instance.Compute
	if c.Name != "web" || c.VMID != vm.Properties.VMID || c.ResourceGroupName != "rg" || c.SubscriptionID != defaultSubscription ||
		c.Location != "westeurope" || c.VMSize != "Standard_B2s" || c.OSProfile.AdminUsername != "stagely" {
		t.Fatalf("compute = %+v", c)
	}
	ips := instance.Network.Interface[0].IPv4.IPAddress[0]
	if ips.PrivateIPAddress != n.privateIP() || ips.PublicIPAddress != addr {
		t.Fatalf("addresses = %+v", ips)
	}
	if docs["/metadata/instance/compute/name"] != "web" || docs["/metadata/instance/network/interface/0/ipv4/ipAddress/0/publicIpAddress"] != addr {
		t.Fatal("the leaves are missing")
	}
}
