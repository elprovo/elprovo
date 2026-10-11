package azure

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

var groupNamePattern = regexp.MustCompile(`^[-\w.()]{0,89}[-\w()]$`)

type groupJSON struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags,omitempty"`
	ManagedBy  string            `json:"managedBy,omitempty"`
	Properties map[string]string `json:"properties"`
}

func (s *Simulation) renderGroup(g *group) groupJSON {
	state := statusSucceeded
	if g.Deleting {
		state = "Deleting"
	}
	return groupJSON{
		ID: s.groupID(g.Name), Name: g.Name, Type: typeGroup, Location: g.Location, Tags: cloneTags(g.Tags), ManagedBy: g.ManagedBy,
		Properties: map[string]string{"provisioningState": state},
	}
}

func (s *Simulation) getSubscription(*http.Request, params) (reply, error) {
	return okReply(map[string]any{
		"id": "/subscriptions/" + s.cfg.subscription, "subscriptionId": s.cfg.subscription, "tenantId": s.cfg.tenant,
		"displayName": "Stagely simulation", "state": "Enabled", "authorizationSource": "RoleBased",
		"subscriptionPolicies": map[string]string{"locationPlacementId": "Public_2014-09-01", "quotaId": "PayAsYouGo_2014-09-01", "spendingLimit": "Off"},
	}), nil
}

func (s *Simulation) listGroups(r *http.Request, _ params) (reply, error) {
	var out []groupJSON
	s.store.Read(func(sub *subscription) {
		for _, g := range sub.Groups {
			out = append(out, s.renderGroup(g))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) getGroup(_ *http.Request, p params) (reply, error) {
	var out *groupJSON
	s.store.Read(func(sub *subscription) {
		if g := sub.group(p["group"]); g != nil {
			j := s.renderGroup(g)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errGroupNotFound(p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) headGroup(r *http.Request, p params) (reply, error) {
	if _, err := s.getGroup(r, p); err != nil {
		return reply{status: http.StatusNotFound}, nil
	}
	return reply{status: http.StatusNoContent}, nil
}

type groupRequest struct {
	Location  string            `json:"location"`
	Tags      map[string]string `json:"tags"`
	ManagedBy string            `json:"managedBy"`
}

func (s *Simulation) putGroup(r *http.Request, p params) (reply, error) {
	var req groupRequest
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	name := p["group"]
	if !groupNamePattern.MatchString(name) {
		return reply{}, errBadRequest("InvalidResourceGroupName", "Resource group name '%s' is invalid. It can include alphanumeric, underscore, parentheses, hyphen, period (except at end), and Unicode characters that match the allowed characters.", name)
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	status := http.StatusOK
	var out groupJSON
	err = s.store.Update(func(sub *subscription) error {
		g := sub.group(name)
		switch {
		case g == nil:
			g = &group{Name: name, Location: location}
			sub.Groups = append(sub.Groups, g)
			status = http.StatusCreated
		case g.Deleting:
			return errConflict("ResourceGroupBeingDeleted", "The resource group '%s' is in deprovisioning state and cannot perform this operation.", g.Name)
		case g.Location != location:
			return errConflict("InvalidResourceGroupLocation", "Invalid resource group location '%s'. The Resource group already exists in location '%s'.", location, g.Location)
		}
		g.Tags, g.ManagedBy = cloneTags(req.Tags), req.ManagedBy
		out = s.renderGroup(g)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: status, body: out}, nil
}

func (s *Simulation) patchGroup(r *http.Request, p params) (reply, error) {
	var req struct {
		Tags map[string]string `json:"tags"`
	}
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	var out *groupJSON
	err := s.store.Update(func(sub *subscription) error {
		g := sub.group(p["group"])
		if g == nil {
			return errGroupNotFound(p["group"])
		}
		if req.Tags != nil {
			g.Tags = cloneTags(req.Tags)
		}
		j := s.renderGroup(g)
		out = &j
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return okReply(out), nil
}

// listGroupResources lists every resource in the group, as the Terraform
// provider does before it deletes a group.
func (s *Simulation) listGroupResources(r *http.Request, p params) (reply, error) {
	type item struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		Type     string            `json:"type"`
		Location string            `json:"location"`
		Tags     map[string]string `json:"tags,omitempty"`
	}
	var out []item
	var found bool
	s.store.Read(func(sub *subscription) {
		if found = sub.group(p["group"]) != nil; !found {
			return
		}
		for _, res := range s.groupResources(sub, p["group"]) {
			m := res.meta
			out = append(out, item{ID: s.resourceID(m.Group, res.typ, m.Name), Name: m.Name, Type: res.typ, Location: m.Location, Tags: cloneTags(m.Tags)})
		}
	})
	if !found {
		return reply{}, errGroupNotFound(p["group"])
	}
	return listReply(r, out)
}

type typedResource struct {
	typ  string
	meta *meta
}

func (s *Simulation) groupResources(sub *subscription, name string) []typedResource {
	var out []typedResource
	add := func(typ string, items []*meta) {
		for _, m := range items {
			out = append(out, typedResource{typ, m})
		}
	}
	add(typeVNet, metas(inGroup(sub.VNets, name)))
	add(typeNSG, metas(inGroup(sub.NSGs, name)))
	add(typePublicIP, metas(inGroup(sub.PublicIPs, name)))
	add(typeNIC, metas(inGroup(sub.NICs, name)))
	add(typeVM, metas(inGroup(sub.VMs, name)))
	add(typeDisk, metas(inGroup(sub.Disks, name)))
	add(typeImage, metas(inGroup(sub.Images, name)))
	add(typeRegistry, metas(inGroup(sub.Registries, name)))
	return out
}

func metas[T resource](items []T) []*meta {
	out := make([]*meta, 0, len(items))
	for _, it := range items {
		out = append(out, it.base())
	}
	return out
}

// deleteGroup deletes the group and everything in it: virtual machines and
// their machines first, then the other resources.
func (s *Simulation) deleteGroup(r *http.Request, p params) (reply, error) {
	var op *operation
	var vms []destruction
	var images []string
	err := s.store.Update(func(sub *subscription) error {
		g := sub.group(p["group"])
		if g == nil {
			return errGroupNotFound(p["group"])
		}
		op = s.startOperation(sub, "Microsoft.Resources", g.Location)
		if g.Deleting {
			return nil
		}
		g.Deleting = true
		for _, v := range inGroup(sub.VMs, g.Name) {
			v.ProvisioningState = "Deleting"
			vms = append(vms, destruction{group: v.Group, name: v.Name, machine: v.Machine})
		}
		for _, img := range inGroup(sub.Images, g.Name) {
			images = append(images, img.ImageRef)
		}
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	name := p["group"]
	s.background(func(ctx context.Context) { s.destroyGroup(ctx, name, op.ID, vms, images) })
	return s.accepted(r, op), nil
}

func (s *Simulation) destroyGroup(ctx context.Context, name, opID string, vms []destruction, images []string) {
	var failed error
	for _, d := range vms {
		if d.machine == "" {
			continue
		}
		if err := s.machines.Delete(ctx, d.machine); err != nil {
			s.log.Error("delete a virtual machine's machine with its group", "group", name, "vm", d.name, "error", err)
			failed = err
		}
	}
	for _, ref := range images {
		if err := s.machines.RemoveImage(ctx, ref); err != nil {
			s.log.Warn("remove an image's Docker image with its group", "group", name, "image", ref, "error", err)
		}
	}
	err := s.store.Update(func(sub *subscription) error {
		if failed != nil {
			if g := sub.group(name); g != nil {
				g.Deleting = false
			}
			s.finishOperation(sub, opID, failed)
			return nil
		}
		in := func(m *meta) bool { return strings.EqualFold(m.Group, name) }
		sub.VMs = slices.DeleteFunc(sub.VMs, func(v *vm) bool { return in(&v.meta) })
		sub.NICs = slices.DeleteFunc(sub.NICs, func(n *nic) bool { return in(&n.meta) })
		sub.PublicIPs = slices.DeleteFunc(sub.PublicIPs, func(ip *publicIP) bool { return in(&ip.meta) })
		sub.NSGs = slices.DeleteFunc(sub.NSGs, func(n *nsg) bool { return in(&n.meta) })
		sub.VNets = slices.DeleteFunc(sub.VNets, func(v *vnet) bool { return in(&v.meta) })
		sub.Disks = slices.DeleteFunc(sub.Disks, func(d *disk) bool { return in(&d.meta) })
		sub.Images = slices.DeleteFunc(sub.Images, func(i *image) bool { return in(&i.meta) })
		sub.Registries = slices.DeleteFunc(sub.Registries, func(reg *registry) bool { return in(&reg.meta) })
		sub.Deployments = slices.DeleteFunc(sub.Deployments, func(d *deployment) bool { return in(&d.meta) })
		sub.Groups = slices.DeleteFunc(sub.Groups, func(g *group) bool { return strings.EqualFold(g.Name, name) })
		s.finishOperation(sub, opID, nil)
		return nil
	})
	if err != nil {
		s.log.Error("remove a deleted resource group", "group", name, "error", err)
	}
	for _, d := range vms {
		s.firewalls.forget(d.machine)
	}
	s.firewalls.kick()
}

// checkLocation returns the location's canonical name, or an error when the
// simulation does not serve it.
func (s *Simulation) checkLocation(location string) (string, error) {
	l := strings.ToLower(strings.ReplaceAll(location, " ", ""))
	if l == "" {
		return "", errInvalidParameter("The location property is required for this definition.")
	}
	if !slices.Contains(s.cfg.locations, l) {
		return "", errBadRequest("LocationNotAvailableForResourceType", "The provided location '%s' is not available for resource type. List of available regions is '%s'.", location, strings.Join(s.cfg.locations, ","))
	}
	return l, nil
}

// requireGroup returns the group a new resource goes in, or an error when
// it does not exist or is being deleted.
func requireGroup(sub *subscription, name string) (*group, error) {
	g := sub.group(name)
	if g == nil {
		return nil, errGroupNotFound(name)
	}
	if g.Deleting {
		return nil, errConflict("ResourceGroupBeingDeleted", "The resource group '%s' is in deprovisioning state and cannot perform this operation.", g.Name)
	}
	return g, nil
}
