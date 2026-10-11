package azure

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Resource types, as Resource Manager spells them in ids and responses.
const (
	typeGroup       = "Microsoft.Resources/resourceGroups"
	typeDeployment  = "Microsoft.Resources/deployments"
	typeVNet        = "Microsoft.Network/virtualNetworks"
	typeSubnet      = "Microsoft.Network/virtualNetworks/subnets"
	typeNSG         = "Microsoft.Network/networkSecurityGroups"
	typeRule        = "Microsoft.Network/networkSecurityGroups/securityRules"
	typeDefaultRule = "Microsoft.Network/networkSecurityGroups/defaultSecurityRules"
	typePublicIP    = "Microsoft.Network/publicIPAddresses"
	typeNIC         = "Microsoft.Network/networkInterfaces"
	typeIPConfig    = "Microsoft.Network/networkInterfaces/ipConfigurations"
	typeVM          = "Microsoft.Compute/virtualMachines"
	typeDisk        = "Microsoft.Compute/disks"
	typeImage       = "Microsoft.Compute/images"
	typeRegistry    = "Microsoft.ContainerRegistry/registries"
)

// subscription is everything the simulation keeps: one subscription's
// resource groups and resources, and the long-running operations.
type subscription struct {
	// Account labels this simulation's machines, so a restart finds them
	// and never touches another simulation's.
	Account    string `json:"account"`
	SigningKey []byte `json:"signing_key"`
	NextMAC    uint32 `json:"next_mac"`

	Groups      []*group      `json:"resource_groups"`
	VNets       []*vnet       `json:"virtual_networks"`
	NSGs        []*nsg        `json:"network_security_groups"`
	PublicIPs   []*publicIP   `json:"public_ip_addresses"`
	NICs        []*nic        `json:"network_interfaces"`
	VMs         []*vm         `json:"virtual_machines"`
	Disks       []*disk       `json:"disks"`
	Images      []*image      `json:"images"`
	Registries  []*registry   `json:"registries"`
	Deployments []*deployment `json:"deployments"`
	Operations  []*operation  `json:"operations"`
}

type group struct {
	Name      string            `json:"name"`
	Location  string            `json:"location"`
	Tags      map[string]string `json:"tags,omitempty"`
	ManagedBy string            `json:"managed_by,omitempty"`
	Deleting  bool              `json:"deleting,omitempty"`
}

// meta is what every resource in a group has.
type meta struct {
	Group    string            `json:"group"`
	Name     string            `json:"name"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags,omitempty"`
	GUID     string            `json:"guid"`
	Etag     string            `json:"etag"`
}

func (m *meta) base() *meta { return m }

type resource interface{ base() *meta }

// subResource is a reference to another resource by id.
type subResource struct {
	ID string `json:"id"`
}

func refOf(id string) *subResource {
	if id == "" {
		return nil
	}
	return &subResource{ID: id}
}

func idOf(r *subResource) string {
	if r == nil {
		return ""
	}
	return r.ID
}

// resourceJSON is the envelope of every resource in a response.
type resourceJSON struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Location   string            `json:"location,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Etag       string            `json:"etag,omitempty"`
	SKU        any               `json:"sku,omitempty"`
	Zones      []string          `json:"zones,omitempty"`
	ManagedBy  string            `json:"managedBy,omitempty"`
	Properties any               `json:"properties"`
}

// resourceRequest is the body of a resource's PUT.
type resourceRequest[P any] struct {
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags"`
	SKU        *sku              `json:"sku"`
	Zones      []string          `json:"zones"`
	Identity   json.RawMessage   `json:"identity"`
	Plan       json.RawMessage   `json:"plan"`
	Properties P                 `json:"properties"`
}

type sku struct {
	Name string `json:"name"`
	Tier string `json:"tier,omitempty"`
}

func (s *Simulation) groupID(group string) string {
	return "/subscriptions/" + s.cfg.subscription + "/resourceGroups/" + group
}

// resourceID is the id of a resource of typ, such as
// Microsoft.Network/virtualNetworks, with child type and name pairs after it.
func (s *Simulation) resourceID(group, typ, name string, children ...string) string {
	id := s.groupID(group) + "/providers/" + typ + "/" + name
	for _, c := range children {
		id += "/" + c
	}
	return id
}

func (s *Simulation) idFor(typ string, m *meta) string {
	return s.resourceID(m.Group, typ, m.Name)
}

// ref is a resource id taken apart.
type ref struct {
	group string
	// typ is the canonical resource type, with child types appended, such
	// as Microsoft.Network/virtualNetworks/subnets.
	typ   string
	name  string
	child string
}

// parseRef takes apart a resource id of this subscription. It reports false
// for ids it cannot read.
func (s *Simulation) parseRef(id string) (ref, bool) {
	segs := splitPath(id)
	if len(segs) < 8 || len(segs)%2 != 0 || !strings.EqualFold(segs[0], "subscriptions") || !strings.EqualFold(segs[1], s.cfg.subscription) ||
		!strings.EqualFold(segs[2], "resourceGroups") || !strings.EqualFold(segs[4], "providers") {
		return ref{}, false
	}
	typ := segs[5] + "/" + segs[6]
	r := ref{group: segs[3], name: segs[7]}
	if len(segs) == 10 {
		typ += "/" + segs[8]
		r.child = segs[9]
	} else if len(segs) > 10 {
		return ref{}, false
	}
	for _, known := range []string{typeVNet, typeSubnet, typeNSG, typePublicIP, typeNIC, typeIPConfig, typeVM, typeDisk, typeImage, typeRegistry} {
		if strings.EqualFold(known, typ) {
			r.typ = known
			return r, true
		}
	}
	r.typ = typ
	return r, true
}

// sameID compares two resource ids as Resource Manager does, without case.
func sameID(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/"))
}

func lookup[T resource](items []T, group, name string) T {
	for _, it := range items {
		if b := it.base(); strings.EqualFold(b.Group, group) && strings.EqualFold(b.Name, name) {
			return it
		}
	}
	var zero T
	return zero
}

func inGroup[T resource](items []T, group string) []T {
	var out []T
	for _, it := range items {
		if strings.EqualFold(it.base().Group, group) {
			out = append(out, it)
		}
	}
	return out
}

func without[T resource](items []T, group, name string) []T {
	return slices.DeleteFunc(items, func(it T) bool {
		b := it.base()
		return strings.EqualFold(b.Group, group) && strings.EqualFold(b.Name, name)
	})
}

func (sub *subscription) group(name string) *group {
	for _, g := range sub.Groups {
		if strings.EqualFold(g.Name, name) {
			return g
		}
	}
	return nil
}

func etag() string { return `W/"` + newUUID() + `"` }

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.0000000Z") }

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}

func cloneTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}
