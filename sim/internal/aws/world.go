package aws

import (
	"slices"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/registrytoken"
)

// world is the account's state: every resource of every region.
type world struct {
	AccountID         string              `json:"account_id"`
	Instances         []*instance         `json:"instances"`
	KeyPairs          []*keyPair          `json:"key_pairs"`
	VPCs              []*vpc              `json:"vpcs"`
	Subnets           []*subnet           `json:"subnets"`
	InternetGateways  []*internetGateway  `json:"internet_gateways"`
	RouteTables       []*routeTable       `json:"route_tables"`
	NetworkACLs       []*networkACL       `json:"network_acls"`
	SecurityGroups    []*securityGroup    `json:"security_groups"`
	Addresses         []*address          `json:"addresses"`
	Images            []*image            `json:"images"`
	Snapshots         []*snapshot         `json:"snapshots"`
	Volumes           []*volume           `json:"volumes"`
	Repositories      []*repository       `json:"repositories"`
	CreationTemplates []*creationTemplate `json:"creation_templates"`
	Tags              map[string][]tag    `json:"tags"`
	// Sessions are the federated sessions, by access key id.
	Sessions map[string]*session `json:"sessions"`
	// RegistryTokens are the passwords of ECR authorization tokens.
	RegistryTokens map[string]*registryToken `json:"registry_tokens"`
	RegistrySigner *registrytoken.Signer     `json:"registry_signer,omitempty"`
}

type tag struct {
	Key   string `json:"key" xml:"key"`
	Value string `json:"value" xml:"value"`
}

// tagsOf returns the tags of the resource with the id.
func (w *world) tagsOf(id string) []tag {
	return slices.Clone(w.Tags[id])
}

// setTag adds a tag to the resource, or replaces the value of its tag with
// the same key.
func (w *world) setTag(id string, t tag) {
	tags := w.Tags[id]
	if i := slices.IndexFunc(tags, func(x tag) bool { return x.Key == t.Key }); i >= 0 {
		tags[i] = t
		return
	}
	w.Tags[id] = append(tags, t)
}

func (w *world) forget(id string) {
	delete(w.Tags, id)
}

func find[T any](items []*T, match func(*T) bool) *T {
	if i := slices.IndexFunc(items, match); i >= 0 {
		return items[i]
	}
	return nil
}

func (w *world) instance(id string) *instance {
	return find(w.Instances, func(i *instance) bool { return i.ID == id })
}

func (w *world) vpc(region, id string) *vpc {
	return find(w.VPCs, func(v *vpc) bool { return v.ID == id && v.Region == region })
}

func (w *world) defaultVPC(region string) *vpc {
	return find(w.VPCs, func(v *vpc) bool { return v.Region == region && v.Default })
}

func (w *world) subnet(region, id string) *subnet {
	return find(w.Subnets, func(s *subnet) bool { return s.ID == id && s.Region == region })
}

func (w *world) securityGroup(region, id string) *securityGroup {
	return find(w.SecurityGroups, func(g *securityGroup) bool { return g.ID == id && g.Region == region })
}

func (w *world) image(region, id string) *image {
	return find(w.Images, func(i *image) bool { return i.ID == id && i.Region == region })
}

func (w *world) snapshot(region, id string) *snapshot {
	return find(w.Snapshots, func(s *snapshot) bool { return s.ID == id && s.Region == region })
}

func (w *world) volume(id string) *volume {
	return find(w.Volumes, func(v *volume) bool { return v.ID == id })
}

func (w *world) keyPair(region, name string) *keyPair {
	return find(w.KeyPairs, func(k *keyPair) bool { return k.Name == name && k.Region == region })
}

// timestamp formats t as the EC2 API does.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
