package digitalocean

import (
	"encoding/json"
	"strconv"
	"strings"
)

// dropletMetadata returns the documents that DigitalOcean's metadata service
// serves to the droplet under /metadata/v1, by path, with the addresses its
// machine got.
func dropletMetadata(d *droplet, plan dropletPlan, userData, privateIPv4, publicIPv4 string) map[string]string {
	const base = "/metadata/v1"
	id := strconv.FormatInt(d.ID, 10)
	type ipv4 struct {
		IPAddress string `json:"ip_address"`
	}
	type networkInterface struct {
		IPv4 ipv4   `json:"ipv4"`
		Type string `json:"type"`
	}
	full, _ := json.Marshal(struct {
		DropletID  int64                         `json:"droplet_id"`
		Hostname   string                        `json:"hostname"`
		UserData   string                        `json:"user_data,omitempty"`
		PublicKeys []string                      `json:"public_keys"`
		Region     string                        `json:"region"`
		Interfaces map[string][]networkInterface `json:"interfaces"`
		Tags       []string                      `json:"tags"`
		Features   map[string]bool               `json:"features"`
	}{
		DropletID: d.ID, Hostname: d.Name, UserData: userData, PublicKeys: append([]string{}, plan.publicKeys...), Region: d.Region,
		Interfaces: map[string][]networkInterface{
			"private": {{IPv4: ipv4{privateIPv4}, Type: "private"}},
			"public":  {{IPv4: ipv4{publicIPv4}, Type: "public"}},
		},
		Tags: append([]string{}, d.Tags...), Features: map[string]bool{"dhcp_enabled": false},
	})
	docs := map[string]string{
		base + ".json":        string(full),
		base + "/id":          id,
		base + "/hostname":    d.Name,
		base + "/user-data":   userData,
		base + "/public-keys": strings.Join(plan.publicKeys, "\n"),
		base + "/region":      d.Region,
		base + "/interfaces/private/0/ipv4/address": privateIPv4,
		base + "/interfaces/private/0/type":         "private",
		base + "/interfaces/public/0/ipv4/address":  publicIPv4,
		base + "/interfaces/public/0/type":          "public",
	}
	for _, tag := range d.Tags {
		docs[base+"/tags/"+tag] = tag
	}
	return docs
}
