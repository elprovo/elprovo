package azure

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// packerTemplate is the shape of the template that the Packer plugin's
// azure-arm builder deploys for a Linux build with a Standard public IP.
const packerTemplate = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "adminUsername": {"type": "string"}, "commandToExecute": {"type": "string"}, "dnsNameForPublicIP": {"type": "string"},
    "nicName": {"type": "string"}, "nsgName": {"type": "string"}, "osDiskName": {"type": "string"},
    "publicIPAddressName": {"type": "string"}, "subnetName": {"type": "string"}, "virtualNetworkName": {"type": "string"},
    "vmName": {"type": "string"}, "vmSize": {"type": "string"}
  },
  "resources": [
    {"apiVersion": "[variables('networkApiVersion')]", "location": "[variables('location')]", "name": "[parameters('publicIPAddressName')]",
     "properties": {"dnsSettings": {"domainNameLabel": "[parameters('dnsNameForPublicIP')]"}, "publicIPAllocationMethod": "[variables('publicIPAddressType')]"},
     "sku": {"name": "Standard", "tier": "Regional"}, "type": "Microsoft.Network/publicIPAddresses"},
    {"apiVersion": "[variables('networkApiVersion')]",
     "dependsOn": ["[concat('Microsoft.Network/publicIPAddresses/', parameters('publicIPAddressName'))]", "[concat('Microsoft.Network/virtualNetworks/', variables('virtualNetworkName'))]"],
     "location": "[variables('location')]", "name": "[parameters('nicName')]",
     "properties": {"ipConfigurations": [{"name": "ipconfig", "properties": {"privateIPAllocationMethod": "Dynamic",
       "publicIPAddress": {"id": "[resourceId('Microsoft.Network/publicIPAddresses', parameters('publicIPAddressName'))]"}, "subnet": {"id": "[variables('subnetRef')]"}}}]},
     "type": "Microsoft.Network/networkInterfaces"},
    {"apiVersion": "[variables('computeApiVersion')]", "dependsOn": ["[concat('Microsoft.Network/networkInterfaces/', parameters('nicName'))]"],
     "location": "[variables('location')]", "name": "[parameters('vmName')]",
     "properties": {"hardwareProfile": {"vmSize": "[parameters('vmSize')]"},
       "networkProfile": {"networkInterfaces": [{"id": "[resourceId('Microsoft.Network/networkInterfaces', parameters('nicName'))]"}]},
       "osProfile": {"adminUsername": "[parameters('adminUsername')]", "computerName": "[parameters('vmName')]",
         "linuxConfiguration": {"disablePasswordAuthentication": true, "ssh": {"publicKeys": [{"keyData": "KEY", "path": "[variables('sshKeyPath')]"}]}}},
       "storageProfile": {"imageReference": {"offer": "ubuntu-24_04-lts", "publisher": "Canonical", "sku": "server", "version": "latest"},
         "osDisk": {"caching": "ReadWrite", "createOption": "FromImage", "managedDisk": {"storageAccountType": "Standard_LRS"}, "name": "[parameters('osDiskName')]", "osType": "Linux"}}},
     "type": "Microsoft.Compute/virtualMachines"},
    {"apiVersion": "[variables('computeApiVersion')]", "condition": "[not(empty(parameters('commandToExecute')))]",
     "dependsOn": ["[resourceId('Microsoft.Compute/virtualMachines/', parameters('vmName'))]"], "location": "[variables('location')]",
     "name": "[concat(parameters('vmName'), '/extension-customscript')]", "properties": {}, "type": "Microsoft.Compute/virtualMachines/extensions"},
    {"apiVersion": "[variables('networkApiVersion')]", "location": "[variables('location')]", "name": "[parameters('nsgName')]",
     "properties": {"securityRules": [{"name": "AllowIPsToSshWinRMInbound", "properties": {"access": "Allow", "destinationAddressPrefix": "VirtualNetwork",
       "destinationPortRange": "22", "direction": "Inbound", "priority": 100, "protocol": "Tcp", "sourceAddressPrefix": "*", "sourcePortRange": "*"}}]},
     "type": "Microsoft.Network/networkSecurityGroups"},
    {"apiVersion": "[variables('networkApiVersion')]", "dependsOn": ["[concat('Microsoft.Network/networkSecurityGroups/', parameters('nsgName'))]"],
     "location": "[variables('location')]", "name": "[variables('virtualNetworkName')]",
     "properties": {"addressSpace": {"addressPrefixes": ["[variables('addressPrefix')]"]},
       "subnets": [{"name": "[variables('subnetName')]", "properties": {"addressPrefix": "[variables('subnetAddressPrefix')]",
         "networkSecurityGroup": {"id": "[resourceId('Microsoft.Network/networkSecurityGroups', parameters('nsgName'))]"}}}]},
     "type": "Microsoft.Network/virtualNetworks"}
  ],
  "variables": {
    "addressPrefix": "10.0.0.0/16", "computeApiVersion": "2023-03-01", "location": "[resourceGroup().location]", "networkApiVersion": "2023-04-01",
    "publicIPAddressType": "Static", "sshKeyPath": "[concat('/home/',parameters('adminUsername'),'/.ssh/authorized_keys')]",
    "subnetAddressPrefix": "10.0.0.0/24", "subnetName": "[parameters('subnetName')]",
    "subnetRef": "[concat(variables('vnetID'),'/subnets/',variables('subnetName'))]", "virtualNetworkName": "[parameters('virtualNetworkName')]",
    "virtualNetworkResourceGroup": "[resourceGroup().name]",
    "vnetID": "[resourceId(variables('virtualNetworkResourceGroup'), 'Microsoft.Network/virtualNetworks', variables('virtualNetworkName'))]"
  }
}`

func packerDeployment(t *testing.T) map[string]any {
	t.Helper()
	var template map[string]any
	if err := json.Unmarshal([]byte(strings.Replace(packerTemplate, "KEY", testKey, 1)), &template); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{}
	for k, v := range map[string]string{
		"adminUsername": "packer", "commandToExecute": "", "dnsNameForPublicIP": "pkrdns", "nicName": "pkrni", "nsgName": "pkrsg",
		"osDiskName": "pkros", "publicIPAddressName": "pkrip", "subnetName": "pkrsn", "virtualNetworkName": "pkrvn", "vmName": "pkrvm", "vmSize": "Standard_B2s",
	} {
		params[k] = map[string]string{"value": v}
	}
	return map[string]any{"properties": map[string]any{"mode": "Incremental", "template": template, "parameters": params}}
}

func TestPackerTemplateDeploysItsResourcesInDependencyOrder(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("pkr")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	path := groupPath("pkr") + "/providers/Microsoft.Resources/deployments/pkrdp"
	e.mustCall(http.StatusOK, http.MethodPost, path+"/validate"+apiVersion, packerDeployment(t), nil)

	headers := e.mustCall(http.StatusCreated, http.MethodPut, path+apiVersion, packerDeployment(t), nil)

	if got := e.wait(headers); got != statusSucceeded {
		var d map[string]any
		e.mustCall(http.StatusOK, http.MethodGet, path+apiVersion, nil, &d)
		t.Fatalf("deployment = %s: %v", got, d)
	}
	var ops struct {
		Value []struct {
			Properties struct {
				ProvisioningState string `json:"provisioningState"`
				TargetResource    struct {
					ResourceType string `json:"resourceType"`
					ResourceName string `json:"resourceName"`
				} `json:"targetResource"`
			} `json:"properties"`
		} `json:"value"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, groupPath("pkr")+"/deployments/pkrdp/operations"+apiVersion+"&$top=50", nil, &ops)
	var order []string
	for _, op := range ops.Value {
		if op.Properties.ProvisioningState != statusSucceeded {
			t.Fatalf("operation %+v did not succeed", op)
		}
		order = append(order, op.Properties.TargetResource.ResourceName)
	}
	if strings.Join(order, ",") != "pkrip,pkrsg,pkrvn,pkrni,pkrvm" {
		t.Fatalf("deployed %v, want each resource after the ones it depends on", order)
	}
	var vm vmJSON
	e.mustCall(http.StatusOK, http.MethodGet, resourcePath("pkr", typeVM, "pkrvm")+apiVersion, nil, &vm)
	if e.machines.specOf(vm.Properties.VMID).Hostname != "pkrvm" {
		t.Fatal("the template's virtual machine has no machine")
	}
	if execs := e.machines.execsOf(vm.Properties.VMID); len(execs) != 1 || execs[0].args[len(execs[0].args)-1] != "packer" {
		t.Fatalf("provisioning = %+v, want the admin user packer", execs)
	}
}

func TestTemplateWithAnUnsupportedFunctionFailsValidation(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("pkr")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	body := packerDeployment(t)
	template := body["properties"].(map[string]any)["template"].(map[string]any)
	template["variables"].(map[string]any)["location"] = "[reference('x').location]"

	got := e.mustFail(http.StatusBadRequest, "InvalidTemplate", http.MethodPost, groupPath("pkr")+"/providers/Microsoft.Resources/deployments/pkrdp/validate"+apiVersion, body)

	if !strings.Contains(got.Error.Message, "reference") {
		t.Fatalf("message = %q, want it to name the function", got.Error.Message)
	}
}

func TestTemplateExpressions(t *testing.T) {
	scope := &templateScope{
		sim: &Simulation{cfg: testConfig()}, group: &group{Name: "rg", Location: "westeurope"},
		parameters: map[string]any{"name": "vm"}, variables: map[string]any{"prefix": "[concat('pre-', parameters('name'))]"},
		evaluated: map[string]any{}, evaluating: map[string]bool{},
	}
	cases := map[string]any{
		"[variables('prefix')]":                        "pre-vm",
		"[format('{0}/{1}', 'a', parameters('name'))]": "a/vm",
		"[resourceGroup().location]":                   "westeurope",
		"[not(empty(''))]":                             false,
		"[concat('it''s ', 'quoted')]":                 "it's quoted",
		"[[not an expression]":                         "[not an expression]",
		"[resourceId('Microsoft.Compute/virtualMachines/extensions', 'vm', 'ext')]": "/subscriptions/" + defaultSubscription + "/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm/extensions/ext",
	}
	for expr, want := range cases {
		got, err := scope.eval(expr)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", expr, got, err, want)
		}
	}
}
