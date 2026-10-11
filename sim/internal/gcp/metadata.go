package gcp

import (
	"encoding/json"
	"strconv"
)

// instanceMetadata returns the documents that the Compute Engine metadata
// server serves to the instance under /computeMetadata/v1, by path, with the
// addresses its machine got.
func instanceMetadata(inst *instance, project string, projectNumber int64, privateIPv4, publicIPv4 string) map[string]string {
	const base = "/computeMetadata/v1/"
	number := strconv.FormatInt(projectNumber, 10)
	tags, _ := json.Marshal(append([]string{}, inst.Tags...))
	docs := map[string]string{
		base + "instance/id":                           idString(inst.ID),
		base + "instance/name":                         inst.Name,
		base + "instance/hostname":                     inst.Name + "." + inst.Zone + ".c." + project + ".internal",
		base + "instance/zone":                         "projects/" + number + "/zones/" + inst.Zone,
		base + "instance/machine-type":                 "projects/" + number + "/machineTypes/" + inst.MachineType,
		base + "instance/tags":                         string(tags),
		base + "instance/network-interfaces/0/ip":      privateIPv4,
		base + "instance/network-interfaces/0/network": "projects/" + number + "/networks/" + inst.Network,
		base + "project/project-id":                    project,
		base + "project/numeric-project-id":            number,
	}
	if inst.AccessConfig != nil {
		external := publicIPv4
		if inst.StaticIP != "" {
			external = inst.StaticIP
		}
		docs[base+"instance/network-interfaces/0/access-configs/0/external-ip"] = external
		docs[base+"instance/network-interfaces/0/access-configs/0/type"] = "ONE_TO_ONE_NAT"
	}
	for _, item := range inst.Metadata {
		docs[base+"instance/attributes/"+item.Key] = item.Value
	}
	return docs
}
