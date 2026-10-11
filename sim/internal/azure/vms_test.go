package azure

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCreateVMBootsAMachineOnItsInterfaceAddress(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	pipID, pipAddr := e.publicIP("rg", "web-ip")
	n := e.nic("rg", "web-nic", subnet, pipID, "")
	release := e.machines.hold("Create")

	var created vmJSON
	headers := e.mustCall(http.StatusCreated, http.MethodPut, resourcePath("rg", typeVM, "web")+apiVersion, vmBody(n.ID, "#cloud-config\nruncmd: [true]\n"), &created)

	if created.Properties.ProvisioningState != "Creating" || headers.Get("Retry-After") == "" {
		t.Fatalf("create answered provisioning state %q and Retry-After %q", created.Properties.ProvisioningState, headers.Get("Retry-After"))
	}
	release()
	if got := e.wait(headers); got != statusSucceeded {
		t.Fatalf("create operation = %s", got)
	}
	var vm vmJSON
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil, &vm)
	spec := e.machines.specOf(vm.Properties.VMID)
	if vm.Properties.ProvisioningState != statusSucceeded || spec.PrivateIP != n.privateIP() || spec.Seed.UserData != "#cloud-config\nruncmd: [true]\n" ||
		!slices.Equal(spec.Seed.PublicKeys, []string{testKey}) || spec.Hostname != "web" || spec.Cloud != "azure" || spec.Resource != vm.Properties.VMID {
		t.Fatalf("vm = %+v, machine spec = %+v", vm.Properties, spec)
	}
	if got := e.machines.addressesOf(vm.Properties.VMID); !slices.Equal(got, []string{pipAddr}) {
		t.Fatalf("machine addresses = %v, want the public IP %s", got, pipAddr)
	}
	execs := e.machines.execsOf(vm.Properties.VMID)
	if len(execs) != 1 || execs[0].args[len(execs[0].args)-1] != "stagely" || execs[0].stdin != testKey+"\n" {
		t.Fatalf("provisioning commands = %+v, want the admin user stagely made with the key", execs)
	}
	if vm.Properties.OSProfile.CustomData != "" {
		t.Fatal("GET returned the custom data")
	}
}

func TestCreateVMAddsItsOSDiskAndAttachesItsInterface(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")

	vm := e.createVM("rg", "web", n.ID)

	disk := vm.Properties.StorageProfile.OSDisk
	if !strings.HasPrefix(disk.Name, "web_OsDisk_1_") || disk.ManagedDisk == nil || disk.ManagedDisk.StorageAccountType != "StandardSSD_LRS" {
		t.Fatalf("os disk = %+v", disk)
	}
	var d struct {
		ManagedBy string `json:"managedBy"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeDisk, disk.Name)+apiVersion, nil, &d)
	if !strings.EqualFold(d.ManagedBy, vm.ID) {
		t.Fatalf("disk managed by %q, want %q", d.ManagedBy, vm.ID)
	}
	var got nicJSON
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeNIC, "web-nic")+apiVersion, nil, &got)
	if got.Properties.VirtualMachine == nil || !strings.EqualFold(got.Properties.VirtualMachine.ID, vm.ID) {
		t.Fatalf("interface virtualMachine = %+v", got.Properties.VirtualMachine)
	}
	e.mustFail(http.StatusBadRequest, "NicInUse", http.MethodDelete, resourcePath("rg", typeNIC, "web-nic")+apiVersion, nil)
}

func TestCreateVMRefusesWhatAzureRefuses(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")
	cases := []struct {
		name   string
		change func(props map[string]any)
		code   string
	}{
		{"root as the admin user", func(p map[string]any) { p["osProfile"].(map[string]any)["adminUsername"] = "root" }, "InvalidParameter"},
		{"keys outside the admin's home", func(p map[string]any) {
			p["osProfile"].(map[string]any)["linuxConfiguration"].(map[string]any)["ssh"] = map[string]any{"publicKeys": []any{map[string]string{"path": "/root/.ssh/authorized_keys", "keyData": testKey}}}
		}, "InvalidParameter"},
		{"an unknown size", func(p map[string]any) { p["hardwareProfile"] = map[string]string{"vmSize": "Standard_Huge"} }, "InvalidParameter"},
		{"an unknown image", func(p map[string]any) {
			p["storageProfile"].(map[string]any)["imageReference"] = map[string]string{"publisher": "Canonical", "offer": "nope", "sku": "server", "version": "latest"}
		}, "PlatformImageNotFound"},
		{"a data disk", func(p map[string]any) {
			p["storageProfile"].(map[string]any)["dataDisks"] = []any{map[string]any{"lun": 0}}
		}, "UnsupportedBySimulation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := vmBody(n.ID, "")
			c.change(body["properties"].(map[string]any))
			e.mustFail(http.StatusBadRequest, c.code, http.MethodPut, resourcePath("rg", typeVM, "web")+apiVersion, body)
		})
	}
}

func TestDeleteVMRemovesTheMachineBeforeTheVMIsGone(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")
	vm := e.createVM("rg", "web", n.ID)
	release := e.machines.hold("Delete")

	headers := e.mustCall(http.StatusAccepted, http.MethodDelete, resourcePath("rg", typeVM, "web")+apiVersion, nil, nil)

	var deleting vmJSON
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil, &deleting)
	if deleting.Properties.ProvisioningState != "Deleting" {
		t.Fatalf("provisioning state while the machine is deleted = %s", deleting.Properties.ProvisioningState)
	}
	release()
	if got := e.wait(headers); got != statusSucceeded {
		t.Fatalf("delete operation = %s", got)
	}
	e.mustFail(http.StatusNotFound, "ResourceNotFound", http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil)
	if e.machines.has(vm.Properties.VMID) {
		t.Fatal("the machine still exists")
	}
	e.mustFail(http.StatusNotFound, "ResourceNotFound", http.MethodGet, resourcePath("rg", typeNIC, "web-nic")+apiVersion, nil)
	e.mustFail(http.StatusNotFound, "ResourceNotFound", http.MethodGet, resourcePath("rg", typeDisk, vm.Properties.StorageProfile.OSDisk.Name)+apiVersion, nil)
}

func TestDeleteVMStaysWhenItsMachineCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")
	e.createVM("rg", "web", n.ID)
	e.machines.fail("Delete", errDocker)

	headers := e.mustCall(http.StatusAccepted, http.MethodDelete, resourcePath("rg", typeVM, "web")+apiVersion, nil, nil)

	if got := e.wait(headers); got != statusFailed {
		t.Fatalf("delete operation = %s, want Failed", got)
	}
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil, nil)
}

func TestCaptureImageFromAGeneralizedVMAndBootFromIt(t *testing.T) {
	e := newEnv(t)
	subnet := e.network("rg")
	n := e.nic("rg", "build-nic", subnet, "", "")
	vm := e.createVM("rg", "build", n.ID)
	imageBody := map[string]any{"location": "westeurope", "properties": map[string]any{"sourceVirtualMachine": map[string]string{"id": vm.ID}, "storageProfile": map[string]any{"zoneResilient": false}}}
	e.mustFail(http.StatusConflict, "OperationNotAllowed", http.MethodPost, resourcePath("rg", typeVM, "build")+"/generalize"+apiVersion, nil)

	e.wait(e.mustCall(http.StatusAccepted, http.MethodPost, resourcePath("rg", typeVM, "build")+"/deallocate"+apiVersion, nil, nil))
	e.mustFail(http.StatusConflict, "OperationNotAllowed", http.MethodPut, resourcePath("rg", typeImage, "golden")+apiVersion, imageBody)
	e.mustCall(http.StatusOK, http.MethodPost, resourcePath("rg", typeVM, "build")+"/generalize"+apiVersion, nil, nil)
	if got := e.wait(e.mustCall(http.StatusCreated, http.MethodPut, resourcePath("rg", typeImage, "golden")+apiVersion, imageBody, nil)); got != statusSucceeded {
		t.Fatalf("capture = %s", got)
	}

	e.mustFail(http.StatusConflict, "OperationNotAllowed", http.MethodPost, resourcePath("rg", typeVM, "build")+"/start"+apiVersion, nil)
	body := vmBody(e.nic("rg", "web-nic", subnet, "", "").ID, "")
	body["properties"].(map[string]any)["storageProfile"].(map[string]any)["imageReference"] = map[string]string{"id": resourcePath("rg", typeImage, "golden")}
	headers := e.mustCall(http.StatusCreated, http.MethodPut, resourcePath("rg", typeVM, "web")+apiVersion, body, nil)
	e.wait(headers)
	var web vmJSON
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("rg", typeVM, "web")+apiVersion, nil, &web)
	image := e.machines.specOf(web.Properties.VMID).Image
	if !strings.HasPrefix(image, "stagely-sim-az-image:") || e.machines.imageSource(image) != "c-"+vm.Properties.VMID {
		t.Fatalf("the VM booted %q, want the image committed from the build machine", image)
	}
}

func TestDeleteGroupDeletesItsMachinesAndResources(t *testing.T) {
	e := newEnv(t)
	n := e.nic("rg", "web-nic", e.network("rg"), "", "")
	vm := e.createVM("rg", "web", n.ID)

	headers := e.mustCall(http.StatusAccepted, http.MethodDelete, groupPath("rg")+apiVersion, nil, nil)

	if got := e.wait(headers); got != statusSucceeded {
		t.Fatalf("delete group = %s", got)
	}
	if e.machines.has(vm.Properties.VMID) {
		t.Fatal("the group's machine still exists")
	}
	e.mustFail(http.StatusNotFound, "ResourceGroupNotFound", http.MethodGet, groupPath("rg")+apiVersion, nil)
	var all struct {
		Value []any `json:"value"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/subscriptions/"+defaultSubscription+"/providers/Microsoft.Network/virtualNetworks"+apiVersion, nil, &all)
	if len(all.Value) != 0 {
		t.Fatalf("networks left = %v", all.Value)
	}
}
