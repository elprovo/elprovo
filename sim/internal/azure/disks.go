package azure

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type disk struct {
	meta
	SKU              string    `json:"sku"`
	SizeGB           int       `json:"size_gb"`
	HyperVGeneration string    `json:"hyper_v_generation"`
	SourceImage      string    `json:"source_image"`
	ManagedBy        string    `json:"managed_by,omitempty"`
	TimeCreated      time.Time `json:"time_created"`
}

// osDiskOf is the managed OS disk that Azure creates with a virtual machine.
func (s *Simulation) osDiskOf(v *vm, plan vmPlan) *disk {
	d := v.Properties.StorageProfile.OSDisk
	source := plan.imageRef.ID
	if source == "" {
		source = "/Subscriptions/" + s.cfg.subscription + "/Providers/Microsoft.Compute/Locations/" + v.Location + "/Publishers/" + plan.imageRef.Publisher +
			"/ArtifactTypes/VMImage/Offers/" + plan.imageRef.Offer + "/Skus/" + plan.imageRef.Sku + "/Versions/" + plan.version
	}
	return &disk{
		meta: meta{Group: v.Group, Name: d.Name, Location: v.Location, GUID: newUUID()},
		SKU:  plan.diskSKU, SizeGB: d.DiskSizeGB, HyperVGeneration: plan.hyperV, SourceImage: source,
		ManagedBy: s.idFor(typeVM, &v.meta), TimeCreated: s.now().UTC(),
	}
}

func (s *Simulation) renderDisk(d *disk) resourceJSON {
	tier := "Standard"
	if strings.HasPrefix(d.SKU, "Premium") {
		tier = "Premium"
	}
	state := "Unattached"
	if d.ManagedBy != "" {
		state = "Attached"
	}
	creation := map[string]any{"createOption": "FromImage", "imageReference": map[string]string{"id": d.SourceImage}}
	props := map[string]any{
		"osType": "Linux", "hyperVGeneration": d.HyperVGeneration, "creationData": creation, "diskSizeGB": d.SizeGB,
		"diskSizeBytes": int64(d.SizeGB) << 30, "uniqueId": d.GUID, "diskState": state, "provisioningState": statusSucceeded,
		"timeCreated": timestamp(d.TimeCreated), "networkAccessPolicy": "AllowAll", "publicNetworkAccess": "Enabled",
		"encryption": map[string]string{"type": "EncryptionAtRestWithPlatformKey"}, "diskIOPSReadWrite": 500, "diskMBpsReadWrite": 60,
	}
	return resourceJSON{
		ID: s.idFor(typeDisk, &d.meta), Name: d.Name, Type: typeDisk, Location: d.Location, Tags: cloneTags(d.Tags),
		SKU: sku{Name: d.SKU, Tier: tier}, ManagedBy: d.ManagedBy, Properties: props,
	}
}

func (s *Simulation) getDisk(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if d := lookup(sub.Disks, p["group"], p["name"]); d != nil {
			j := s.renderDisk(d)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeDisk, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listDisks(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, d := range inGroup(sub.Disks, p["group"]) {
			out = append(out, s.renderDisk(d))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deleteDisk(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		d := lookup(sub.Disks, p["group"], p["name"])
		if d == nil {
			return nil
		}
		if d.ManagedBy != "" {
			return errConflict("OperationNotAllowed", "Disk %s is attached to VM %s.", d.Name, d.ManagedBy)
		}
		sub.Disks = without(sub.Disks, d.Group, d.Name)
		op = s.doneOperation(sub, "Microsoft.Compute", d.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}

type image struct {
	meta
	SourceVM          string    `json:"source_vm"`
	SizeGB            int       `json:"size_gb"`
	HyperVGeneration  string    `json:"hyper_v_generation"`
	ImageRef          string    `json:"image_ref"`
	ProvisioningState string    `json:"provisioning_state"`
	TimeCreated       time.Time `json:"time_created"`
}

type imageProperties struct {
	SourceVirtualMachine *subResource `json:"sourceVirtualMachine"`
	StorageProfile       *struct {
		OSDisk        any   `json:"osDisk"`
		DataDisks     []any `json:"dataDisks"`
		ZoneResilient bool  `json:"zoneResilient"`
	} `json:"storageProfile"`
	HyperVGeneration string `json:"hyperVGeneration"`
}

func (s *Simulation) renderImage(img *image) resourceJSON {
	props := map[string]any{
		"sourceVirtualMachine": map[string]string{"id": img.SourceVM},
		"storageProfile": map[string]any{
			"osDisk": map[string]any{
				"osType": "Linux", "osState": "Generalized", "diskSizeGB": img.SizeGB, "caching": "ReadWrite", "storageAccountType": "Standard_LRS",
			},
			"dataDisks": []any{}, "zoneResilient": false,
		},
		"provisioningState": img.ProvisioningState, "hyperVGeneration": img.HyperVGeneration,
	}
	return resourceJSON{ID: s.idFor(typeImage, &img.meta), Name: img.Name, Type: typeImage, Location: img.Location, Tags: cloneTags(img.Tags), Properties: props}
}

// putImage captures a managed image from a generalized, stopped virtual
// machine by committing its machine into a Docker image.
func (s *Simulation) putImage(r *http.Request, p params) (reply, error) {
	var req resourceRequest[imageProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	props := req.Properties
	switch {
	case !vmNamePattern.MatchString(p["name"]):
		return reply{}, errInvalidParameter("Image name '%s' is invalid.", p["name"])
	case props.SourceVirtualMachine == nil:
		return reply{}, errUnsupported("An image without a source virtual machine")
	case props.StorageProfile != nil && (props.StorageProfile.OSDisk != nil || len(props.StorageProfile.DataDisks) > 0):
		return reply{}, errUnsupported("An image from disks or snapshots")
	case props.StorageProfile != nil && props.StorageProfile.ZoneResilient:
		return reply{}, errUnsupported("A zone-resilient image")
	}
	var out resourceJSON
	var op *operation
	var machine, ref string
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		if lookup(sub.Images, g.Name, p["name"]) != nil {
			return errConflict("OperationNotAllowed", "Image %s already exists; the simulation does not update images.", p["name"])
		}
		v := s.vmByID(sub, props.SourceVirtualMachine.ID)
		switch {
		case v == nil:
			return errNotFound(typeVM, props.SourceVirtualMachine.ID, "")
		case !v.Generalized:
			return errConflict("OperationNotAllowed", "The source virtual machine %s must be generalized to create an image from it.", props.SourceVirtualMachine.ID)
		case v.Location != location:
			return errBadRequest("InvalidParameter", "The image must be in the location of its source virtual machine, %s.", v.Location)
		}
		img := &image{
			meta:     meta{Group: g.Name, Name: p["name"], Location: location, Tags: cloneTags(req.Tags), GUID: newUUID()},
			SourceVM: s.idFor(typeVM, &v.meta), SizeGB: v.Properties.StorageProfile.OSDisk.DiskSizeGB, HyperVGeneration: v.HyperVGeneration,
			ProvisioningState: "Creating", TimeCreated: s.now().UTC(),
		}
		img.ImageRef = "elprovo-sim-az-image:" + sub.Account[:8] + "-" + img.GUID
		sub.Images = append(sub.Images, img)
		op = s.startOperation(sub, "Microsoft.Compute", location)
		machine, ref = v.Machine, img.ImageRef
		out = s.renderImage(img)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	group, name := p["group"], p["name"]
	s.background(func(ctx context.Context) {
		_, err := s.machines.Commit(ctx, machine, ref)
		if err != nil {
			s.log.Error("capture an image", "image", name, "error", err)
		}
		_ = s.store.Update(func(sub *subscription) error {
			if img := lookup(sub.Images, group, name); img != nil {
				img.ProvisioningState = statusSucceeded
				if err != nil {
					img.ProvisioningState = statusFailed
				}
			}
			s.finishOperation(sub, op.ID, err)
			return nil
		})
	})
	return reply{status: http.StatusCreated, body: out, headers: s.asyncHeaders(r, op)}, nil
}

func (s *Simulation) getImage(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if img := lookup(sub.Images, p["group"], p["name"]); img != nil {
			j := s.renderImage(img)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeImage, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listImages(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, img := range inGroup(sub.Images, p["group"]) {
			out = append(out, s.renderImage(img))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deleteImage(r *http.Request, p params) (reply, error) {
	var op *operation
	var ref string
	err := s.store.Update(func(sub *subscription) error {
		img := lookup(sub.Images, p["group"], p["name"])
		if img == nil {
			return nil
		}
		if img.ProvisioningState == "Creating" {
			return errConflict("OperationNotAllowed", "Image %s is still being created.", img.Name)
		}
		op = s.startOperation(sub, "Microsoft.Compute", img.Location)
		ref = img.ImageRef
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	group, name := p["group"], p["name"]
	s.background(func(ctx context.Context) {
		err := s.machines.RemoveImage(ctx, ref)
		_ = s.store.Update(func(sub *subscription) error {
			if err == nil {
				sub.Images = without(sub.Images, group, name)
			}
			s.finishOperation(sub, op.ID, err)
			return nil
		})
	})
	return s.accepted(r, op), nil
}

func (s *Simulation) imageByID(sub *subscription, id string) *image {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typeImage || r.child != "" {
		return nil
	}
	return lookup(sub.Images, r.group, r.name)
}
