package vultr

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

const snapshotNotFound = "Invalid snapshot ID."

type snapshotJSON struct {
	ID             string `json:"id"`
	DateCreated    string `json:"date_created"`
	Description    string `json:"description"`
	Size           int64  `json:"size"`
	CompressedSize int64  `json:"compressed_size"`
	Status         string `json:"status"`
	OSID           int    `json:"os_id"`
	AppID          int    `json:"app_id"`
}

func renderSnapshot(snap *snapshot) snapshotJSON {
	return snapshotJSON{
		ID: snap.ID, DateCreated: timestamp(snap.CreatedAt), Description: snap.Description, Size: snap.Size,
		CompressedSize: snap.Size, Status: snap.Status, OSID: snap.OSID,
	}
}

// createSnapshot starts saving the instance's machine as an image, which new
// instances can boot from once the snapshot is complete.
func (s *Simulation) createSnapshot(r *http.Request) (int, any, error) {
	var req struct {
		InstanceID  string `json:"instance_id"`
		Description string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out snapshotJSON
	var machine string
	err := s.store.Update(func(a *account) error {
		inst := a.instance(req.InstanceID)
		switch {
		case inst == nil || inst.Destroying:
			return errBadRequest("Invalid instance_id.")
		case inst.Machine == "":
			return errBadRequest("The instance is still being created.")
		}
		id := newUUID()
		snap := &snapshot{
			ID: id, Description: req.Description, Status: "pending", OSID: inst.OSID, CreatedAt: s.now(),
			ImageRef: fmt.Sprintf("stagely-sim-vultr-snapshot:%s-%s", a.ID[:8], id),
		}
		a.Snapshots = append(a.Snapshots, snap)
		machine, out = inst.Machine, renderSnapshot(snap)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.takeSnapshot(ctx, out.ID, machine) })
	return http.StatusCreated, map[string]any{"snapshot": out}, nil
}

// takeSnapshot commits the machine and completes the snapshot. A failed
// commit removes the snapshot, so a client waiting for it gets a 404.
func (s *Simulation) takeSnapshot(ctx context.Context, id, machine string) {
	var ref string
	s.store.Read(func(a *account) {
		if snap := a.snapshot(id); snap != nil {
			ref = snap.ImageRef
		}
	})
	size, err := s.machines.Commit(ctx, machine, ref)
	if err != nil {
		s.log.Error("snapshot instance machine", "snapshot", id, "error", err)
	}
	updateErr := s.store.Update(func(a *account) error {
		if err != nil {
			a.Snapshots = slices.DeleteFunc(a.Snapshots, func(snap *snapshot) bool { return snap.ID == id })
			return nil
		}
		if snap := a.snapshot(id); snap != nil {
			snap.Status, snap.Size = "complete", size
		}
		return nil
	})
	if updateErr != nil {
		s.log.Error("record snapshot", "snapshot", id, "error", updateErr)
	}
}

func (s *Simulation) listSnapshots(r *http.Request) (int, any, error) {
	description := r.URL.Query().Get("description")
	var out []snapshotJSON
	s.store.Read(func(a *account) {
		for _, snap := range a.Snapshots {
			if description == "" || snap.Description == description {
				out = append(out, renderSnapshot(snap))
			}
		}
	})
	return listResponse(r, "snapshots", out)
}

func (s *Simulation) getSnapshot(r *http.Request) (int, any, error) {
	var out *snapshotJSON
	s.store.Read(func(a *account) {
		if snap := a.snapshot(r.PathValue("id")); snap != nil {
			j := renderSnapshot(snap)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(snapshotNotFound)
	}
	return http.StatusOK, map[string]any{"snapshot": out}, nil
}

func (s *Simulation) updateSnapshot(r *http.Request) (int, any, error) {
	var req struct {
		Description *string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Description == nil {
		return 0, nil, errBadRequest("description is required.")
	}
	err := s.store.Update(func(a *account) error {
		snap := a.snapshot(r.PathValue("id"))
		if snap == nil {
			return errNotFound(snapshotNotFound)
		}
		snap.Description = *req.Description
		return nil
	})
	return http.StatusNoContent, nil, err
}

// deleteSnapshot removes a complete snapshot and its image.
func (s *Simulation) deleteSnapshot(r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	var ref string
	var err error
	s.store.Read(func(a *account) {
		snap := a.snapshot(id)
		switch {
		case snap == nil:
			err = errNotFound(snapshotNotFound)
		case snap.Status != "complete":
			err = errBadRequest("The snapshot is still being taken.")
		default:
			ref = snap.ImageRef
		}
	})
	if err != nil {
		return 0, nil, err
	}
	if err := s.machines.RemoveImage(r.Context(), ref); err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(a *account) error {
		a.Snapshots = slices.DeleteFunc(a.Snapshots, func(snap *snapshot) bool { return snap.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}
