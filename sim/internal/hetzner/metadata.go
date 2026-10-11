package hetzner

import (
	"encoding/json"
	"strconv"
	"strings"
)

// metadataInput is what a server's metadata documents describe.
type metadataInput struct {
	server     server
	location   locationJSON
	publicKeys []string
	userData   string
	networks   []attachedNetwork
}

type attachedNetwork struct {
	id      int64
	name    string
	ipRange string
	subnet  subnet
}

// attachedNetworks describes the networks a server is attached to, with the
// subnet in its network zone.
func attachedNetworks(p *project, srv *server) []attachedNetwork {
	loc, _ := location(srv.Location)
	var out []attachedNetwork
	for _, id := range srv.Networks {
		n := p.network(id)
		if n == nil {
			continue
		}
		a := attachedNetwork{id: n.ID, name: n.Name, ipRange: n.IPRange}
		for _, sn := range n.Subnets {
			if sn.NetworkZone == loc.NetworkZone {
				a.subnet = sn
				break
			}
		}
		out = append(out, a)
	}
	return out
}

// serverMetadata returns the documents that Hetzner's metadata service
// serves to the server under /hetzner/v1/, by path, given the addresses its
// machine got. cloud-init's Hetzner datasource reads metadata,
// metadata/private-networks and userdata.
func serverMetadata(in metadataInput, privateIPv4, publicIPv4 string) map[string]string {
	const base = "/hetzner/v1/"
	id := strconv.FormatInt(in.server.ID, 10)
	zone := availabilityZones[in.location.Name]
	var summary strings.Builder
	summary.WriteString("availability-zone: " + zone + "\n")
	summary.WriteString("hostname: " + yamlString(in.server.Name) + "\n")
	summary.WriteString("instance-id: " + id + "\n")
	summary.WriteString("public-ipv4: " + publicIPv4 + "\n")
	summary.WriteString("public-keys:")
	if len(in.publicKeys) == 0 {
		summary.WriteString(" []")
	}
	summary.WriteString("\n")
	for _, k := range in.publicKeys {
		summary.WriteString("- " + yamlString(strings.TrimSpace(k)) + "\n")
	}
	summary.WriteString("region: " + in.location.NetworkZone + "\n")
	return map[string]string{
		base + "metadata":                   summary.String(),
		base + "metadata/hostname":          in.server.Name,
		base + "metadata/instance-id":       id,
		base + "metadata/public-ipv4":       publicIPv4,
		base + "metadata/private-networks":  privateNetworksYAML(in.server.ID, in.networks, privateIPv4),
		base + "metadata/availability-zone": zone,
		base + "metadata/region":            in.location.NetworkZone,
		base + "userdata":                   in.userData,
	}
}

func privateNetworksYAML(serverID int64, networks []attachedNetwork, privateIPv4 string) string {
	if len(networks) == 0 {
		return "[]\n"
	}
	var b strings.Builder
	for i, n := range networks {
		b.WriteString("- ip: " + privateIPv4 + "\n")
		b.WriteString("  alias_ips: []\n")
		b.WriteString("  interface_num: " + strconv.Itoa(i+1) + "\n")
		b.WriteString("  mac_address: " + yamlString(macAddress(serverID, n.id)) + "\n")
		b.WriteString("  network_id: " + strconv.FormatInt(n.id, 10) + "\n")
		b.WriteString("  network_name: " + yamlString(n.name) + "\n")
		b.WriteString("  network: " + n.ipRange + "\n")
		b.WriteString("  subnet: " + n.subnet.IPRange + "\n")
		b.WriteString("  gateway: " + n.subnet.Gateway + "\n")
	}
	return b.String()
}

// yamlString quotes s for YAML. A JSON string is a valid YAML double-quoted
// scalar.
func yamlString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}
