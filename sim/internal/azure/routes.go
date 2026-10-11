package azure

import "net/http"

// armRoutes are the Resource Manager requests the simulation serves.
func (s *Simulation) armRoutes() []route {
	const (
		sub   = "subscriptions/{sub}"
		group = sub + "/resourceGroups/{group}"
		rp    = group + "/providers/"
	)
	get, put, del, post, patch := http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch
	return []route{
		pattern(get, sub, s.getSubscription),
		pattern(get, sub+"/resourceGroups", s.listGroups),
		pattern(get, group, s.getGroup),
		pattern(http.MethodHead, group, s.headGroup),
		pattern(put, group, s.putGroup),
		pattern(patch, group, s.patchGroup),
		pattern(del, group, s.deleteGroup),
		pattern(get, group+"/resources", s.listGroupResources),
		pattern(get, sub+"/providers/{namespace}/locations/{location}/operations/{id}", s.getOperationStatus),
		pattern(get, sub+"/providers/{namespace}/locations/{location}/operationResults/{id}", s.getOperationResult),

		pattern(get, sub+"/providers/Microsoft.Network/virtualNetworks", s.listVNets),
		pattern(get, rp+"Microsoft.Network/virtualNetworks", s.listVNets),
		pattern(put, rp+"Microsoft.Network/virtualNetworks/{name}", s.putVNet),
		pattern(get, rp+"Microsoft.Network/virtualNetworks/{name}", s.getVNet),
		pattern(del, rp+"Microsoft.Network/virtualNetworks/{name}", s.deleteVNet),
		pattern(get, rp+"Microsoft.Network/virtualNetworks/{name}/subnets", s.listSubnets),
		pattern(put, rp+"Microsoft.Network/virtualNetworks/{name}/subnets/{subnet}", s.putSubnet),
		pattern(get, rp+"Microsoft.Network/virtualNetworks/{name}/subnets/{subnet}", s.getSubnet),
		pattern(del, rp+"Microsoft.Network/virtualNetworks/{name}/subnets/{subnet}", s.deleteSubnet),
		pattern(get, rp+"Microsoft.Network/networkSecurityGroups", s.listNSGs),
		pattern(put, rp+"Microsoft.Network/networkSecurityGroups/{name}", s.putNSG),
		pattern(get, rp+"Microsoft.Network/networkSecurityGroups/{name}", s.getNSG),
		pattern(del, rp+"Microsoft.Network/networkSecurityGroups/{name}", s.deleteNSG),
		pattern(get, rp+"Microsoft.Network/publicIPAddresses", s.listPublicIPs),
		pattern(put, rp+"Microsoft.Network/publicIPAddresses/{name}", s.putPublicIP),
		pattern(get, rp+"Microsoft.Network/publicIPAddresses/{name}", s.getPublicIP),
		pattern(del, rp+"Microsoft.Network/publicIPAddresses/{name}", s.deletePublicIP),
		pattern(get, rp+"Microsoft.Network/networkInterfaces", s.listNICs),
		pattern(put, rp+"Microsoft.Network/networkInterfaces/{name}", s.putNIC),
		pattern(get, rp+"Microsoft.Network/networkInterfaces/{name}", s.getNIC),
		pattern(del, rp+"Microsoft.Network/networkInterfaces/{name}", s.deleteNIC),

		pattern(get, sub+"/providers/Microsoft.Compute/virtualMachines", s.listVMs),
		pattern(get, rp+"Microsoft.Compute/virtualMachines", s.listVMs),
		pattern(put, rp+"Microsoft.Compute/virtualMachines/{name}", s.putVM),
		pattern(get, rp+"Microsoft.Compute/virtualMachines/{name}", s.getVM),
		pattern(patch, rp+"Microsoft.Compute/virtualMachines/{name}", s.patchVM),
		pattern(del, rp+"Microsoft.Compute/virtualMachines/{name}", s.deleteVM),
		pattern(get, rp+"Microsoft.Compute/virtualMachines/{name}/instanceView", s.getVMInstanceView),
		pattern(post, rp+"Microsoft.Compute/virtualMachines/{name}/{action}", s.vmAction),
		pattern(get, rp+"Microsoft.Compute/disks", s.listDisks),
		pattern(get, rp+"Microsoft.Compute/disks/{name}", s.getDisk),
		pattern(del, rp+"Microsoft.Compute/disks/{name}", s.deleteDisk),
		pattern(get, rp+"Microsoft.Compute/images", s.listImages),
		pattern(put, rp+"Microsoft.Compute/images/{name}", s.putImage),
		pattern(get, rp+"Microsoft.Compute/images/{name}", s.getImage),
		pattern(del, rp+"Microsoft.Compute/images/{name}", s.deleteImage),

		pattern(post, sub+"/providers/Microsoft.ContainerRegistry/checkNameAvailability", s.checkRegistryName),
		pattern(put, rp+"Microsoft.ContainerRegistry/registries/{name}", s.putRegistry),
		pattern(get, rp+"Microsoft.ContainerRegistry/registries/{name}", s.getRegistry),
		pattern(patch, rp+"Microsoft.ContainerRegistry/registries/{name}", s.patchRegistry),
		pattern(del, rp+"Microsoft.ContainerRegistry/registries/{name}", s.deleteRegistry),
		pattern(post, rp+"Microsoft.ContainerRegistry/registries/{name}/listCredentials", s.listRegistryCredentials),
		pattern(get, rp+"Microsoft.ContainerRegistry/registries/{name}/replications", s.listRegistryReplications),
		pattern(get, rp+"Microsoft.ContainerRegistry/registries/{name}/scopeMaps/{scopeMap}", s.getScopeMap),
		pattern(get, rp+"Microsoft.ContainerRegistry/registries/{name}/tokens", s.listTokens),
		pattern(put, rp+"Microsoft.ContainerRegistry/registries/{name}/tokens/{token}", s.putToken),
		pattern(get, rp+"Microsoft.ContainerRegistry/registries/{name}/tokens/{token}", s.getToken),
		pattern(del, rp+"Microsoft.ContainerRegistry/registries/{name}/tokens/{token}", s.deleteToken),
		pattern(post, rp+"Microsoft.ContainerRegistry/registries/{name}/generateCredentials", s.generateCredentials),

		pattern(put, rp+"Microsoft.Resources/deployments/{name}", s.putDeployment),
		pattern(get, rp+"Microsoft.Resources/deployments/{name}", s.getDeployment),
		pattern(del, rp+"Microsoft.Resources/deployments/{name}", s.deleteDeployment),
		pattern(post, rp+"Microsoft.Resources/deployments/{name}/validate", s.validateDeployment),
		pattern(get, rp+"Microsoft.Resources/deployments/{name}/operations", s.listDeploymentOperations),
		pattern(get, group+"/deployments/{name}/operations", s.listDeploymentOperations),
	}
}
