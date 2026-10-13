package hetzner

import (
	"net/http"
	"slices"
	"strconv"
)

type createdFromJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type imageJSON struct {
	ID           int64             `json:"id"`
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	Name         *string           `json:"name"`
	Description  string            `json:"description"`
	ImageSize    *float64          `json:"image_size"`
	DiskSize     int               `json:"disk_size"`
	Created      string            `json:"created"`
	CreatedFrom  *createdFromJSON  `json:"created_from"`
	BoundTo      *int64            `json:"bound_to"`
	OSFlavor     string            `json:"os_flavor"`
	OSVersion    *string           `json:"os_version"`
	RapidDeploy  bool              `json:"rapid_deploy"`
	Protection   protectionJSON    `json:"protection"`
	Deprecated   *string           `json:"deprecated"`
	Deprecation  *struct{}         `json:"deprecation"`
	Deleted      *string           `json:"deleted"`
	Labels       map[string]string `json:"labels"`
	Architecture string            `json:"architecture"`
}

type protectionJSON struct {
	Delete bool `json:"delete"`
}

func (img systemImage) render() imageJSON {
	name, version := img.name, img.osVersion
	return imageJSON{
		ID: img.id, Type: "system", Status: "available", Name: &name, Description: img.description, DiskSize: 5,
		Created: timestamp(systemImagesCreated), OSFlavor: "ubuntu", OSVersion: &version, RapidDeploy: true,
		Labels: map[string]string{}, Architecture: img.architecture,
	}
}

func (img *image) render() imageJSON {
	size, version := img.ImageSize, "24.04"
	return imageJSON{
		ID: img.ID, Type: "snapshot", Status: img.Status, Description: img.Description, ImageSize: &size, DiskSize: img.DiskSize,
		Created: timestamp(img.Created), CreatedFrom: &createdFromJSON{ID: img.ServerID, Name: img.ServerName},
		OSFlavor: "ubuntu", OSVersion: &version, Labels: labelsOrEmpty(img.Labels), Architecture: img.Architecture,
	}
}

// resolvedImage is an image a server boots: a system image or a snapshot,
// whose Ref names the machine image.
type resolvedImage struct {
	id           int64
	architecture string
	ref          string
	diskSize     int
}

// findImage finds an image by id, or a system image by name and
// architecture.
func findImage(p *project, id int64, name, architecture string) (resolvedImage, bool) {
	for _, img := range systemImages {
		if (id != 0 && img.id == id) || (name != "" && img.name == name && img.architecture == architecture) {
			return resolvedImage{id: img.id, architecture: img.architecture, diskSize: 5}, true
		}
	}
	if snap := p.image(id); snap != nil && snap.Status == "available" {
		return resolvedImage{id: snap.ID, architecture: snap.Architecture, ref: snap.Ref, diskSize: snap.DiskSize}, true
	}
	return resolvedImage{}, false
}

func renderImageByID(p *project, id int64) *imageJSON {
	for _, img := range systemImages {
		if img.id == id {
			out := img.render()
			return &out
		}
	}
	if snap := p.image(id); snap != nil {
		out := snap.render()
		return &out
	}
	return nil
}

func (s *Simulation) listImages(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	types, statuses, arch, name := q["type"], q["status"], q.Get("architecture"), q.Get("name")
	if len(q["bound_to"]) > 0 {
		types = []string{"backup"}
	}
	matches := func(img imageJSON) bool {
		imgName := ""
		if img.Name != nil {
			imgName = *img.Name
		}
		return (len(types) == 0 || slices.Contains(types, img.Type)) &&
			(len(statuses) == 0 || slices.Contains(statuses, img.Status)) &&
			(arch == "" || img.Architecture == arch) && (name == "" || imgName == name) && keep(img.Labels)
	}
	var out []imageJSON
	for _, img := range systemImages {
		if j := img.render(); matches(j) {
			out = append(out, j)
		}
	}
	s.store.Read(func(p *project) {
		for _, img := range p.Images {
			if j := img.render(); matches(j) {
				out = append(out, j)
			}
		}
	})
	return listResponse(r, "images", out, sortFields[imageJSON]{
		"id":      byID(func(i imageJSON) int64 { return i.ID }),
		"created": byString(func(i imageJSON) string { return i.Created }),
		"name": byString(func(i imageJSON) string {
			if i.Name == nil {
				return ""
			}
			return *i.Name
		}),
	})
}

func (s *Simulation) getImage(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "image")
	if err != nil {
		return 0, nil, err
	}
	var out *imageJSON
	s.store.Read(func(p *project) { out = renderImageByID(p, id) })
	if out == nil {
		return 0, nil, errNotFound("image")
	}
	return http.StatusOK, map[string]any{"image": out}, nil
}

func (s *Simulation) updateImage(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "image")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Description *string            `json:"description"`
		Type        string             `json:"type"`
		Labels      *map[string]string `json:"labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out imageJSON
	err = s.store.Update(func(p *project) error {
		img := p.image(id)
		if img == nil {
			if renderImageByID(p, id) != nil {
				return errCode(http.StatusForbidden, "forbidden", "system images can not be changed")
			}
			return errNotFound("image")
		}
		if req.Type != "" && req.Type != "snapshot" {
			return errUnsupported("the simulation keeps snapshots as snapshots; type %q is not supported", req.Type)
		}
		if req.Description != nil {
			img.Description = *req.Description
		}
		if req.Labels != nil {
			img.Labels = *req.Labels
		}
		out = img.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"image": out}, nil
}

// deleteImage deletes a snapshot and the machine image it was committed to.
// A snapshot that is still being taken is locked.
func (s *Simulation) deleteImage(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "image")
	if err != nil {
		return 0, nil, err
	}
	var ref, status string
	var system bool
	s.store.Read(func(p *project) {
		if img := p.image(id); img != nil {
			ref, status = img.Ref, img.Status
		} else {
			system = renderImageByID(p, id) != nil
		}
	})
	switch {
	case system:
		return 0, nil, errCode(http.StatusForbidden, "forbidden", "system images can not be deleted")
	case ref == "":
		return 0, nil, errNotFound("image")
	case status == "creating":
		return 0, nil, errLocked("image")
	}
	if err := s.machines.RemoveImage(r.Context(), ref); err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(p *project) error {
		p.Images = slices.DeleteFunc(p.Images, func(img *image) bool { return img.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}

func snapshotRef(projectUUID string, id int64) string {
	return "elprovo-sim-hz-snapshot:" + projectUUID[:8] + "-" + strconv.FormatInt(id, 10)
}
