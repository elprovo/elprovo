package gcp

import (
	"slices"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/registrytoken"
)

// world is everything the simulated project holds. It is saved as JSON after
// every change.
type world struct {
	// Account labels the project's machines, so two simulations that serve
	// the same project ID on one Docker host keep apart.
	Account       string                 `json:"account"`
	ProjectNumber int64                  `json:"project_number"`
	NextID        uint64                 `json:"next_id"`
	Keys          []*serviceAccountKey   `json:"service_account_keys"`
	Tokens        map[string]issuedToken `json:"access_tokens"`
	Accounts      []*iamAccount          `json:"service_accounts"`
	Registry      *registrytoken.Signer  `json:"registry_signer,omitempty"`
	Networks      []*network             `json:"networks"`
	Subnetworks   []*subnetwork          `json:"subnetworks"`
	Firewalls     []*firewall            `json:"firewalls"`
	Addresses     []*address             `json:"addresses"`
	Instances     []*instance            `json:"instances"`
	Disks         []*disk                `json:"disks"`
	Images        []*image               `json:"images"`
	Operations    []*operation           `json:"operations"`
	Repositories  []*repository          `json:"repositories"`
	LROs          []*longRunningOp       `json:"long_running_operations"`
}

// issuedToken is an access token the simulation issued: whose it is and
// when it expires.
type issuedToken struct {
	Email  string    `json:"email"`
	Expiry time.Time `json:"expiry"`
}

func (w *world) nextID() uint64 {
	w.NextID++
	return w.NextID
}

func find[T any](items []*T, match func(*T) bool) *T {
	if i := slices.IndexFunc(items, match); i >= 0 {
		return items[i]
	}
	return nil
}

func (w *world) network(name string) *network {
	return find(w.Networks, func(n *network) bool { return n.Name == name })
}

func (w *world) subnetwork(region, name string) *subnetwork {
	return find(w.Subnetworks, func(s *subnetwork) bool { return s.Region == region && s.Name == name })
}

func (w *world) firewall(name string) *firewall {
	return find(w.Firewalls, func(f *firewall) bool { return f.Name == name })
}

func (w *world) address(region, name string) *address {
	return find(w.Addresses, func(a *address) bool { return a.Region == region && a.Name == name })
}

func (w *world) instance(zone, name string) *instance {
	return find(w.Instances, func(i *instance) bool { return i.Zone == zone && i.Name == name })
}

func (w *world) instanceByID(id uint64) *instance {
	return find(w.Instances, func(i *instance) bool { return i.ID == id })
}

func (w *world) disk(zone, name string) *disk {
	return find(w.Disks, func(d *disk) bool { return d.Zone == zone && d.Name == name })
}

func (w *world) image(name string) *image {
	return find(w.Images, func(i *image) bool { return i.Name == name })
}

func (w *world) operation(scope, location, name string) *operation {
	return find(w.Operations, func(o *operation) bool {
		return o.Scope == scope && o.Location == location && o.Name == name
	})
}

func (w *world) account(email string) *iamAccount {
	return find(w.Accounts, func(a *iamAccount) bool { return a.Email == email || a.UniqueID == email })
}

func (w *world) repository(name string) *repository {
	return find(w.Repositories, func(r *repository) bool { return r.Name == name })
}

// imageRefInUse reports whether a disk or an image still boots from the
// Docker image ref.
func (w *world) imageRefInUse(ref string) bool {
	return slices.ContainsFunc(w.Disks, func(d *disk) bool { return d.ImageRef == ref }) ||
		slices.ContainsFunc(w.Images, func(i *image) bool { return i.ImageRef == ref })
}
