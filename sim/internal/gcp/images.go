package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// image is a custom image of the project. It boots the Docker image
// ImageRef, shared with the disk it was made from.
type image struct {
	ID           uint64            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Family       string            `json:"family"`
	Labels       map[string]string `json:"labels"`
	SourceDisk   string            `json:"source_disk"`
	SourceDiskID uint64            `json:"source_disk_id"`
	SizeGB       int               `json:"size_gb"`
	Architecture string            `json:"architecture"`
	Licenses     []string          `json:"licenses"`
	Deprecated   *deprecation      `json:"deprecated,omitempty"`
	ImageRef     string            `json:"image_ref"`
	CreatedAt    time.Time         `json:"created_at"`
}

type deprecation struct {
	State       string `json:"state"`
	Replacement string `json:"replacement,omitempty"`
	Deprecated  string `json:"deprecated,omitempty"`
	Obsolete    string `json:"obsolete,omitempty"`
	Deleted     string `json:"deleted,omitempty"`
}

type imageJSON struct {
	Kind              string            `json:"kind"`
	ID                string            `json:"id"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Family            string            `json:"family,omitempty"`
	SourceType        string            `json:"sourceType"`
	Status            string            `json:"status"`
	ArchiveSizeBytes  string            `json:"archiveSizeBytes"`
	DiskSizeGb        string            `json:"diskSizeGb"`
	Licenses          []string          `json:"licenses,omitempty"`
	SelfLink          string            `json:"selfLink"`
	Labels            map[string]string `json:"labels,omitempty"`
	LabelFingerprint  string            `json:"labelFingerprint"`
	GuestOsFeatures   []map[string]any  `json:"guestOsFeatures"`
	StorageLocations  []string          `json:"storageLocations"`
	Architecture      string            `json:"architecture,omitempty"`
	SourceDisk        string            `json:"sourceDisk,omitempty"`
	SourceDiskID      string            `json:"sourceDiskId,omitempty"`
	Deprecated        *deprecation      `json:"deprecated,omitempty"`
}

func publicImageLink(p publicImage) string {
	return computeBase + "projects/" + p.project + "/global/images/" + p.name
}

func publicImageLicense(p publicImage) string {
	return computeBase + "projects/" + p.project + "/global/licenses/" + strings.TrimSuffix(strings.TrimSuffix(p.family, "-amd64"), "-arm64")
}

func renderPublicImage(p publicImage) imageJSON {
	return imageJSON{
		Kind: "compute#image", ID: idString(p.id), CreationTimestamp: timestamp(catalogCreated), Name: p.name,
		Description: "Canonical, Ubuntu, 24.04 LTS, " + strings.ToLower(p.architecture) + " noble image", Family: p.family,
		SourceType: "RAW", Status: "READY", ArchiveSizeBytes: "2330708352", DiskSizeGb: strconv.Itoa(publicImageSizeGB),
		Licenses: []string{publicImageLicense(p)}, SelfLink: publicImageLink(p), LabelFingerprint: fingerprint("null"),
		GuestOsFeatures: guestOSFeatures(), StorageLocations: []string{"eu", "us", "asia"}, Architecture: p.architecture,
	}
}

func (s *Simulation) renderImage(img *image) imageJSON {
	return imageJSON{
		Kind: "compute#image", ID: idString(img.ID), CreationTimestamp: timestamp(img.CreatedAt), Name: img.Name,
		Description: img.Description, Family: img.Family, SourceType: "RAW", Status: "READY", ArchiveSizeBytes: "0",
		DiskSizeGb: strconv.Itoa(img.SizeGB), Licenses: img.Licenses, SelfLink: s.link("global", "images", img.Name),
		Labels: img.Labels, LabelFingerprint: fingerprint(jsonKey(img.Labels)), GuestOsFeatures: guestOSFeatures(),
		StorageLocations: []string{"us"}, Architecture: img.Architecture, SourceDisk: img.SourceDisk,
		SourceDiskID: idString(img.SourceDiskID), Deprecated: img.Deprecated,
	}
}

// imageInfo is what an instance needs from the image it boots.
type imageInfo struct {
	link         string
	id           uint64
	sizeGB       int
	architecture string
	licenses     []string
	ref          string
}

// resolveImage finds the image that a sourceImage names: an image of the
// project or of a public image project, by name or as family/<family>.
func (s *Simulation) resolveImage(w *world, value string) (imageInfo, error) {
	r := parseRef(value)
	family, byFamily := strings.CutPrefix(r.name, "family/")
	notFound := errNotFound("projects/" + r.project + "/global/images/" + r.name)
	if r.kind != "" && r.kind != "images" {
		return imageInfo{}, errInvalid("Invalid value for field 'resource.disks[0].initializeParams.sourceImage': '%s'.", value)
	}
	if r.project == "" || r.project == s.cfg.project {
		img := w.image(r.name)
		if byFamily {
			img = familyImage(w, family)
		}
		if img == nil || (img.Deprecated != nil && (img.Deprecated.State == "OBSOLETE" || img.Deprecated.State == "DELETED")) {
			return imageInfo{}, notFound
		}
		return imageInfo{s.link("global", "images", img.Name), img.ID, img.SizeGB, img.Architecture, img.Licenses, img.ImageRef}, nil
	}
	i := slices.IndexFunc(publicImages, func(p publicImage) bool {
		return p.project == r.project && ((byFamily && p.family == family) || (!byFamily && p.name == r.name))
	})
	if i < 0 {
		return imageInfo{}, notFound
	}
	p := publicImages[i]
	return imageInfo{publicImageLink(p), p.id, publicImageSizeGB, p.architecture, []string{publicImageLicense(p)}, ""}, nil
}

// familyImage is the newest image of the family that is not deprecated.
func familyImage(w *world, family string) *image {
	var newest *image
	for _, img := range w.Images {
		if img.Family == family && img.Deprecated == nil && (newest == nil || img.CreatedAt.After(newest.CreatedAt)) {
			newest = img
		}
	}
	return newest
}

func isPublicProject(project string) bool {
	return slices.ContainsFunc(publicImages, func(p publicImage) bool { return p.project == project })
}

func (s *Simulation) listImages(r *http.Request) (int, any, error) {
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	project := r.PathValue("project")
	var out []imageJSON
	switch {
	case isPublicProject(project):
		for _, p := range publicImages {
			if p.project == project {
				out = append(out, renderPublicImage(p))
			}
		}
	case project == s.cfg.project:
		s.store.Read(func(w *world) {
			for _, img := range w.Images {
				out = append(out, s.renderImage(img))
			}
		})
	default:
		return 0, nil, errNotFound("projects/" + project)
	}
	return page(r, "compute#imageList", computeBase+"projects/"+project+"/global/images", out)
}

func (s *Simulation) getImage(r *http.Request) (int, any, error) {
	return s.findImage(r, r.PathValue("name"), "")
}

func (s *Simulation) getImageFromFamily(r *http.Request) (int, any, error) {
	return s.findImage(r, "", r.PathValue("family"))
}

// findImage answers an image of the project or of a public image project, by
// name or, when family is set, the family's newest image.
func (s *Simulation) findImage(r *http.Request, name, family string) (int, any, error) {
	project := r.PathValue("project")
	path := "projects/" + project + "/global/images/" + name
	if family != "" {
		path = "projects/" + project + "/global/images/family/" + family
	}
	var out *imageJSON
	switch {
	case isPublicProject(project):
		for _, p := range publicImages {
			if p.project == project && (p.name == name || (family != "" && p.family == family)) {
				rendered := renderPublicImage(p)
				out = &rendered
			}
		}
	case project == s.cfg.project:
		s.store.Read(func(w *world) {
			img := w.image(name)
			if family != "" {
				img = familyImage(w, family)
			}
			if img != nil {
				rendered := s.renderImage(img)
				out = &rendered
			}
		})
	}
	if out == nil {
		return 0, nil, errNotFound(path)
	}
	return http.StatusOK, out, nil
}

type imageRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Family      string            `json:"family"`
	Labels      map[string]string `json:"labels"`
	SourceDisk  string            `json:"sourceDisk"`
	SourceType  string            `json:"sourceType"`
	ForceCreate bool              `json:"forceCreate"`
}

var (
	imageFields        = []string{"kind", "name", "description", "family", "labels", "sourceDisk", "sourceType", "forceCreate"}
	ignoredImageFields = []string{
		"architecture", "licenses", "guestOsFeatures", "storageLocations", "shieldedInstanceInitialState", "imageEncryptionKey",
		"sourceImage", "sourceSnapshot", "rawDisk", "sourceDiskEncryptionKey",
	}
)

func (s *Simulation) insertImage(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var raw map[string]json.RawMessage
	if err := decodeBody(r, &raw); err != nil {
		return 0, nil, err
	}
	for key, value := range raw {
		if !slices.Contains(imageFields, key) && (!slices.Contains(ignoredImageFields, key) || !emptyJSON(value)) {
			return 0, nil, errUnsupported("The image field %s", key)
		}
	}
	var req imageRequest
	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, nil, &apiError{http.StatusBadRequest, "parseError", "Invalid JSON payload received. " + err.Error()}
	}
	if err := checkName("image", req.Name); err != nil {
		return 0, nil, err
	}
	switch {
	case req.Family != "" && !namePattern.MatchString(req.Family):
		return 0, nil, errInvalid("Invalid value for field 'resource.family': '%s'.", req.Family)
	case req.SourceType != "" && req.SourceType != "RAW":
		return 0, nil, errInvalid("Invalid value for field 'resource.sourceType': '%s'.", req.SourceType)
	case req.ForceCreate:
		return 0, nil, errUnsupported("Creating an image from a disk in use (forceCreate)")
	case req.SourceDisk == "":
		return 0, nil, errUnsupported("An image without a source disk")
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	src := parseRef(req.SourceDisk)
	if src.project != "" && src.project != s.cfg.project || src.kind != "disks" || !s.catalog.hasZone(src.location) {
		return 0, nil, errInvalid("Invalid value for field 'resource.sourceDisk': '%s'.", req.SourceDisk)
	}
	var out operationJSON
	err := s.store.Update(func(w *world) error {
		if w.image(req.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/global/images/" + req.Name)
		}
		d := w.disk(src.location, src.name)
		if d == nil {
			return errNotFound("projects/" + s.cfg.project + "/zones/" + src.location + "/disks/" + src.name)
		}
		link := s.link("zones", d.Zone, "disks", d.Name)
		if inst := w.instanceByID(d.User); inst != nil {
			return errInUse("The disk resource '%s' is already being used by '%s'", link, s.link("zones", inst.Zone, "instances", inst.Name))
		}
		img := &image{
			ID: w.nextID(), Name: req.Name, Description: req.Description, Family: req.Family, Labels: req.Labels,
			SourceDisk: link, SourceDiskID: d.ID, SizeGB: d.SizeGB, Architecture: d.Architecture, Licenses: d.Licenses,
			ImageRef: d.ImageRef, CreatedAt: s.now(),
		}
		w.Images = append(w.Images, img)
		op := s.newOperation(w, "global", "", "insert", s.link("global", "images", img.Name), img.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteImage(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	var ref string
	err := s.store.Update(func(w *world) error {
		img := w.image(name)
		if img == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/images/" + name)
		}
		w.Images = slices.DeleteFunc(w.Images, func(other *image) bool { return other == img })
		ref = img.ImageRef
		op := s.newOperation(w, "global", "", "delete", s.link("global", "images", name), img.ID)
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

func (s *Simulation) deprecateImage(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var req deprecation
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	switch req.State {
	case "", "ACTIVE", "DEPRECATED", "OBSOLETE", "DELETED":
	default:
		return 0, nil, errInvalid("Invalid value for field 'deprecationStatusResource.state': '%s'.", req.State)
	}
	name := r.PathValue("name")
	var out operationJSON
	err := s.store.Update(func(w *world) error {
		img := w.image(name)
		if img == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/images/" + name)
		}
		img.Deprecated = nil
		if req.State != "" && req.State != "ACTIVE" {
			img.Deprecated = &req
		}
		op := s.newOperation(w, "global", "", "deprecate", s.link("global", "images", name), img.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}
