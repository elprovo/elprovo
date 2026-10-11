package digitalocean

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var tagNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_\-:]{1,255}$`)

func canonicalTag(a *account, name string) string {
	for _, t := range a.Tags {
		if strings.EqualFold(t.Name, name) {
			return t.Name
		}
	}
	return name
}

type taggedCount struct {
	Count         int    `json:"count"`
	LastTaggedURI string `json:"last_tagged_uri,omitempty"`
}

type tagResourcesJSON struct {
	Count           int         `json:"count"`
	LastTaggedURI   string      `json:"last_tagged_uri,omitempty"`
	Droplets        taggedCount `json:"droplets"`
	Images          taggedCount `json:"images"`
	Volumes         taggedCount `json:"volumes"`
	VolumeSnapshots taggedCount `json:"volume_snapshots"`
	Databases       taggedCount `json:"databases"`
}

type tagJSON struct {
	Name      string           `json:"name"`
	Resources tagResourcesJSON `json:"resources"`
}

func renderTag(r *http.Request, a *account, name string) tagJSON {
	out := tagJSON{Name: name}
	for _, d := range a.Droplets {
		if slices.Contains(d.Tags, name) {
			out.Resources.Droplets.Count++
			out.Resources.Droplets.LastTaggedURI = baseURL(r) + "/v2/droplets/" + strconv.FormatInt(d.ID, 10)
		}
	}
	for _, snap := range a.Snapshots {
		if slices.Contains(snap.Tags, name) {
			out.Resources.Images.Count++
			out.Resources.Images.LastTaggedURI = baseURL(r) + "/v2/images/" + strconv.FormatInt(snap.ID, 10)
		}
	}
	out.Resources.Count = out.Resources.Droplets.Count + out.Resources.Images.Count
	if out.Resources.Images.LastTaggedURI != "" {
		out.Resources.LastTaggedURI = out.Resources.Images.LastTaggedURI
	} else {
		out.Resources.LastTaggedURI = out.Resources.Droplets.LastTaggedURI
	}
	return out
}

func (s *Simulation) createTag(r *http.Request) (int, any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if !tagNamePattern.MatchString(req.Name) {
		return 0, nil, errBadRequest("tag name must be 1 to 255 letters, digits, '-', '_' or ':'")
	}
	var out tagJSON
	err := s.store.Update(func(a *account) error {
		name := canonicalTag(a, req.Name)
		if a.tag(name) == nil {
			a.Tags = append(a.Tags, &tag{Name: name})
		}
		out = renderTag(r, a, name)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"tag": out}, nil
}

func (s *Simulation) listTags(r *http.Request) (int, any, error) {
	var out []tagJSON
	s.store.Read(func(a *account) {
		for _, t := range a.Tags {
			out = append(out, renderTag(r, a, t.Name))
		}
	})
	return listResponse(r, "tags", out)
}

func (s *Simulation) getTag(r *http.Request) (int, any, error) {
	var out *tagJSON
	s.store.Read(func(a *account) {
		if t := a.tag(r.PathValue("name")); t != nil {
			j := renderTag(r, a, t.Name)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"tag": out}, nil
}

func (s *Simulation) deleteTag(r *http.Request) (int, any, error) {
	name := r.PathValue("name")
	err := s.store.Update(func(a *account) error {
		if a.tag(name) == nil {
			return errNotFound()
		}
		a.Tags = slices.DeleteFunc(a.Tags, func(t *tag) bool { return t.Name == name })
		drop := func(tags []string) []string { return slices.DeleteFunc(tags, func(t string) bool { return t == name }) }
		for _, d := range a.Droplets {
			d.Tags = drop(d.Tags)
		}
		for _, snap := range a.Snapshots {
			snap.Tags = drop(snap.Tags)
		}
		for _, f := range a.Firewalls {
			f.Tags = drop(f.Tags)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

type resourceRef struct {
	ResourceID   string `json:"resource_id"`
	ResourceType string `json:"resource_type"`
}

func (s *Simulation) tagResources(r *http.Request) (int, any, error) {
	return s.changeTagging(r, true)
}

func (s *Simulation) untagResources(r *http.Request) (int, any, error) {
	return s.changeTagging(r, false)
}

func (s *Simulation) changeTagging(r *http.Request, add bool) (int, any, error) {
	var req struct {
		Resources []resourceRef `json:"resources"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	err := s.store.Update(func(a *account) error {
		if a.tag(name) == nil {
			return errNotFound()
		}
		targets := make([]*[]string, 0, len(req.Resources))
		for _, ref := range req.Resources {
			tags, err := taggable(a, ref)
			if err != nil {
				return err
			}
			targets = append(targets, tags)
		}
		for _, tags := range targets {
			if add {
				*tags = appendUnique(*tags, name)
			} else {
				*tags = slices.DeleteFunc(*tags, func(t string) bool { return t == name })
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

func taggable(a *account, ref resourceRef) (*[]string, error) {
	id, err := strconv.ParseInt(ref.ResourceID, 10, 64)
	switch {
	case ref.ResourceType != "droplet" && ref.ResourceType != "image":
		return nil, errUnprocessable("resource type %q is not supported by the simulation; tag droplets or images", ref.ResourceType)
	case err != nil:
		return nil, errNotFound()
	case ref.ResourceType == "droplet":
		if d := a.droplet(id); d != nil {
			return &d.Tags, nil
		}
	default:
		if snap := a.snapshot(id); snap != nil {
			return &snap.Tags, nil
		}
	}
	return nil, errNotFound()
}
