package gcp

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// disk is a zonal persistent disk. While an instance uses it, its content is
// the instance's machine. A detached disk keeps its content in the Docker
// image ImageRef: the image it was made from, or the commit of the machine
// it was detached from. An empty ImageRef is the base machine image.
type disk struct {
	ID            uint64            `json:"id"`
	Name          string            `json:"name"`
	Zone          string            `json:"zone"`
	SizeGB        int               `json:"size_gb"`
	Type          string            `json:"type"`
	SourceImage   string            `json:"source_image"`
	SourceImageID uint64            `json:"source_image_id"`
	Architecture  string            `json:"architecture"`
	Licenses      []string          `json:"licenses"`
	Labels        map[string]string `json:"labels"`
	User          uint64            `json:"user,omitempty"`
	ImageRef      string            `json:"image_ref"`
	CreatedAt     time.Time         `json:"created_at"`
}

type diskJSON struct {
	Kind                   string            `json:"kind"`
	ID                     string            `json:"id"`
	CreationTimestamp      string            `json:"creationTimestamp"`
	Name                   string            `json:"name"`
	SizeGb                 string            `json:"sizeGb"`
	Zone                   string            `json:"zone"`
	Status                 string            `json:"status"`
	SelfLink               string            `json:"selfLink"`
	SourceImage            string            `json:"sourceImage"`
	SourceImageID          string            `json:"sourceImageId"`
	Type                   string            `json:"type"`
	Licenses               []string          `json:"licenses,omitempty"`
	GuestOsFeatures        []map[string]any  `json:"guestOsFeatures"`
	Users                  []string          `json:"users,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	LabelFingerprint       string            `json:"labelFingerprint"`
	PhysicalBlockSizeBytes string            `json:"physicalBlockSizeBytes"`
	Architecture           string            `json:"architecture,omitempty"`
}

func (s *Simulation) renderDisk(w *world, d *disk) diskJSON {
	out := diskJSON{
		Kind: "compute#disk", ID: idString(d.ID), CreationTimestamp: timestamp(d.CreatedAt), Name: d.Name,
		SizeGb: strconv.Itoa(d.SizeGB), Zone: s.link("zones", d.Zone), Status: "READY", SelfLink: s.link("zones", d.Zone, "disks", d.Name),
		SourceImage: d.SourceImage, SourceImageID: idString(d.SourceImageID), Type: s.link("zones", d.Zone, "diskTypes", d.Type),
		Licenses: d.Licenses, GuestOsFeatures: guestOSFeatures(), Labels: d.Labels, LabelFingerprint: fingerprint(jsonKey(d.Labels)),
		PhysicalBlockSizeBytes: "4096", Architecture: d.Architecture,
	}
	if inst := w.instanceByID(d.User); inst != nil {
		out.Users = []string{s.link("zones", inst.Zone, "instances", inst.Name)}
	}
	return out
}

// newBootDisk makes the boot disk that an instance insert asks for and
// returns the Docker image its machine boots.
func (s *Simulation) newBootDisk(w *world, inst *instance, req attachedDiskRequest) (*disk, string, error) {
	params := req.InitializeParams
	name := params.DiskName
	if name == "" {
		name = inst.Name
	}
	if err := checkName("disk", name); err != nil {
		return nil, "", err
	}
	if w.disk(inst.Zone, name) != nil {
		return nil, "", errAlreadyExists("projects/" + s.cfg.project + "/zones/" + inst.Zone + "/disks/" + name)
	}
	img, err := s.resolveImage(w, params.SourceImage)
	if err != nil {
		return nil, "", err
	}
	size := int(params.DiskSizeGb)
	if size == 0 {
		size = img.sizeGB
	}
	if size < img.sizeGB || size > 65536 {
		return nil, "", errInvalid("Invalid value for field 'resource.disks[0].initializeParams.diskSizeGb': '%d'. Requested disk size cannot be smaller than the image size (%d GB).", size, img.sizeGB)
	}
	diskType := defaultDiskType
	if params.DiskType != "" {
		t := parseRef(params.DiskType)
		if !s.belongs(t, "diskTypes", inst.Zone) || !slices.Contains(diskTypes, t.name) {
			return nil, "", errInvalid("Invalid value for field 'resource.disks[0].initializeParams.diskType': '%s'.", params.DiskType)
		}
		diskType = t.name
	}
	if err := checkLabels(params.Labels); err != nil {
		return nil, "", err
	}
	inst.BootDisk, inst.DeviceName, inst.AutoDelete = name, req.DeviceName, req.AutoDelete
	if inst.DeviceName == "" {
		inst.DeviceName = "persistent-disk-0"
	}
	d := &disk{
		Name: name, Zone: inst.Zone, SizeGB: size, Type: diskType, SourceImage: img.link, SourceImageID: img.id,
		Architecture: img.architecture, Licenses: img.licenses, Labels: params.Labels, User: inst.ID, ImageRef: img.ref,
	}
	return d, img.ref, nil
}

func (s *Simulation) listDisks(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []diskJSON
	s.store.Read(func(w *world) {
		for _, d := range w.Disks {
			if d.Zone == zone {
				out = append(out, s.renderDisk(w, d))
			}
		}
	})
	return page(r, "compute#diskList", s.link("zones", zone, "disks"), out)
}

func (s *Simulation) getDisk(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	var out *diskJSON
	s.store.Read(func(w *world) {
		if d := w.disk(zone, r.PathValue("name")); d != nil {
			rendered := s.renderDisk(w, d)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/disks/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteDisk(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	var ref string
	err = s.store.Update(func(w *world) error {
		d := w.disk(zone, name)
		if d == nil {
			return errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/disks/" + name)
		}
		link := s.link("zones", zone, "disks", name)
		if inst := w.instanceByID(d.User); inst != nil {
			return errInUse("The disk resource '%s' is already being used by '%s'", link, s.link("zones", inst.Zone, "instances", inst.Name))
		}
		w.Disks = slices.DeleteFunc(w.Disks, func(other *disk) bool { return other == d })
		ref = d.ImageRef
		op := s.newOperation(w, "zone", zone, "delete", link, d.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.releaseImageRef(ctx, ref) })
	return http.StatusOK, out, nil
}

// removeDisksOf removes the disks that the instance uses and returns the
// Docker images they held.
func removeDisksOf(w *world, instanceID uint64) []string {
	var refs []string
	w.Disks = slices.DeleteFunc(w.Disks, func(d *disk) bool {
		if d.User != instanceID {
			return false
		}
		refs = append(refs, d.ImageRef)
		return true
	})
	return refs
}

func (s *Simulation) releaseImageRefs(ctx context.Context, refs []string) {
	for _, ref := range refs {
		s.releaseImageRef(ctx, ref)
	}
}

// releaseImageRef removes a Docker image of a disk or an image when nothing
// boots from it any more.
func (s *Simulation) releaseImageRef(ctx context.Context, ref string) {
	if ref == "" {
		return
	}
	inUse := false
	s.store.Read(func(w *world) { inUse = w.imageRefInUse(ref) })
	if inUse {
		return
	}
	if err := s.machines.RemoveImage(ctx, ref); err != nil {
		s.log.Warn("remove an unused disk image", "image", ref, "error", err)
	}
}
