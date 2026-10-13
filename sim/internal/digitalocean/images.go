package digitalocean

import (
	"context"
	"net/http"
	"slices"
	"strconv"
)

func (s *Simulation) getAccount(*http.Request) (int, any, error) {
	var uuid string
	s.store.Read(func(a *account) { uuid = a.UUID })
	return http.StatusOK, map[string]any{"account": map[string]any{
		"droplet_limit": dropletLimit, "floating_ip_limit": 3, "reserved_ip_limit": 3, "volume_limit": 100,
		"email": "sim@elprovo.local", "name": "elprovo-sim", "uuid": uuid, "email_verified": true,
		"status": "active", "status_message": "", "team": map[string]string{"uuid": uuid, "name": "elprovo-sim"},
	}}, nil
}

func (s *Simulation) listRegions(r *http.Request) (int, any, error) {
	return listResponse(r, "regions", s.catalog.allRegions())
}

func (s *Simulation) listSizes(r *http.Request) (int, any, error) {
	return listResponse(r, "sizes", s.catalog.allSizes())
}

func listResponse[T any](r *http.Request, key string, items []T) (int, any, error) {
	page, l, m, err := paginate(r, items)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{key: page, "links": l, "meta": m}, nil
}

func imageOf(snap *snapshot) imageJSON {
	return imageJSON{
		ID: snap.ID, Name: snap.Name, Type: "snapshot", Distribution: "Ubuntu", Public: false,
		Regions: []string{snap.Region}, MinDiskSize: snap.MinDiskSize, SizeGigabytes: snap.SizeGigabytes,
		CreatedAt: timestamp(snap.CreatedAt), Description: "", Tags: nonNil(snap.Tags), Status: "available", ErrorMessage: "",
	}
}

func (s *Simulation) listImages(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	var images []imageJSON
	if q.Get("private") != "true" && q.Get("tag_name") == "" && (q.Get("type") == "" || q.Get("type") == "distribution") {
		images = append(images, s.catalog.distributionImage())
	}
	if q.Get("type") == "" {
		s.store.Read(func(a *account) {
			for _, snap := range a.Snapshots {
				if tag := q.Get("tag_name"); tag == "" || slices.Contains(snap.Tags, tag) {
					images = append(images, imageOf(snap))
				}
			}
		})
	}
	return listResponse(r, "images", images)
}

func (s *Simulation) findImage(a *account, ref string) (imageJSON, *snapshot, bool) {
	dist := s.catalog.distributionImage()
	if ref == *dist.Slug || ref == strconv.FormatInt(dist.ID, 10) {
		return dist, nil, true
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return imageJSON{}, nil, false
	}
	if snap := a.snapshot(id); snap != nil {
		return imageOf(snap), snap, true
	}
	return imageJSON{}, nil, false
}

func (s *Simulation) getImage(r *http.Request) (int, any, error) {
	var img imageJSON
	var found bool
	s.store.Read(func(a *account) { img, _, found = s.findImage(a, r.PathValue("id")) })
	if !found {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"image": img}, nil
}

func (s *Simulation) deleteImage(r *http.Request) (int, any, error) {
	if r.PathValue("id") == strconv.FormatInt(ubuntuImage.ID, 10) {
		return 0, nil, errForbidden("You do not have access for the attempted action.")
	}
	return s.removeSnapshot(r.Context(), r.PathValue("id"))
}

type snapshotJSON struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	CreatedAt     string   `json:"created_at"`
	Regions       []string `json:"regions"`
	ResourceID    string   `json:"resource_id"`
	ResourceType  string   `json:"resource_type"`
	MinDiskSize   int      `json:"min_disk_size"`
	SizeGigabytes float64  `json:"size_gigabytes"`
	Tags          []string `json:"tags"`
}

func snapshotOf(snap *snapshot) snapshotJSON {
	return snapshotJSON{
		ID: strconv.FormatInt(snap.ID, 10), Name: snap.Name, CreatedAt: timestamp(snap.CreatedAt), Regions: []string{snap.Region},
		ResourceID: strconv.FormatInt(snap.DropletID, 10), ResourceType: "droplet", MinDiskSize: snap.MinDiskSize,
		SizeGigabytes: snap.SizeGigabytes, Tags: nonNil(snap.Tags),
	}
}

func (s *Simulation) listSnapshots(r *http.Request) (int, any, error) {
	var out []snapshotJSON
	if t := r.URL.Query().Get("resource_type"); t == "" || t == "droplet" {
		s.store.Read(func(a *account) {
			for _, snap := range a.Snapshots {
				out = append(out, snapshotOf(snap))
			}
		})
	}
	return listResponse(r, "snapshots", out)
}

func (s *Simulation) getSnapshot(r *http.Request) (int, any, error) {
	var out *snapshotJSON
	s.store.Read(func(a *account) {
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
			if snap := a.snapshot(id); snap != nil {
				j := snapshotOf(snap)
				out = &j
			}
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"snapshot": out}, nil
}

func (s *Simulation) deleteSnapshot(r *http.Request) (int, any, error) {
	return s.removeSnapshot(r.Context(), r.PathValue("id"))
}

func (s *Simulation) removeSnapshot(ctx context.Context, idText string) (int, any, error) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return 0, nil, errNotFound()
	}
	var ref string
	s.store.Read(func(a *account) {
		if snap := a.snapshot(id); snap != nil {
			ref = snap.ImageRef
		}
	})
	if ref == "" {
		return 0, nil, errNotFound()
	}
	if err := s.machines.RemoveImage(ctx, ref); err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(a *account) error {
		a.Snapshots = slices.DeleteFunc(a.Snapshots, func(snap *snapshot) bool { return snap.ID == id })
		for _, d := range a.Droplets {
			d.SnapshotIDs = slices.DeleteFunc(d.SnapshotIDs, func(v int64) bool { return v == id })
		}
		return nil
	})
	return http.StatusNoContent, nil, err
}

func (s *Simulation) listDropletSnapshots(r *http.Request) (int, any, error) {
	type dropletSnapshot struct {
		ID            int64    `json:"id"`
		Name          string   `json:"name"`
		CreatedAt     string   `json:"created_at"`
		Regions       []string `json:"regions"`
		MinDiskSize   int      `json:"min_disk_size"`
		SizeGigabytes float64  `json:"size_gigabytes"`
		Type          string   `json:"type"`
	}
	var out []dropletSnapshot
	found := false
	s.store.Read(func(a *account) {
		d := a.dropletByPath(r.PathValue("id"))
		if d == nil {
			return
		}
		found = true
		for _, snap := range a.Snapshots {
			if snap.DropletID == d.ID {
				out = append(out, dropletSnapshot{snap.ID, snap.Name, timestamp(snap.CreatedAt), []string{snap.Region}, snap.MinDiskSize, snap.SizeGigabytes, "snapshot"})
			}
		}
	})
	if !found {
		return 0, nil, errNotFound()
	}
	return listResponse(r, "snapshots", out)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}
