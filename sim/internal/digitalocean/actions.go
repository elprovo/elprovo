package digitalocean

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

type actionJSON struct {
	ID           int64      `json:"id"`
	Status       string     `json:"status"`
	Type         string     `json:"type"`
	StartedAt    string     `json:"started_at"`
	CompletedAt  *string    `json:"completed_at"`
	ResourceID   int64      `json:"resource_id"`
	ResourceType string     `json:"resource_type"`
	Region       regionJSON `json:"region"`
	RegionSlug   string     `json:"region_slug"`
}

func (s *Simulation) renderAction(act *action) actionJSON {
	out := actionJSON{
		ID: act.ID, Status: act.Status, Type: act.Type, StartedAt: timestamp(act.StartedAt), ResourceID: act.ResourceID,
		ResourceType: act.ResourceType, Region: s.catalog.region(act.Region), RegionSlug: act.Region,
	}
	if act.CompletedAt != nil {
		done := timestamp(*act.CompletedAt)
		out.CompletedAt = &done
	}
	return out
}

func (s *Simulation) newAction(a *account, typ string, d *droplet) *action {
	act := &action{ID: a.nextID(), Status: "in-progress", Type: typ, StartedAt: s.now().UTC(), ResourceID: d.ID, ResourceType: "droplet", Region: d.Region}
	a.Actions = append(a.Actions, act)
	return act
}

func (s *Simulation) complete(act *action) {
	now := s.now().UTC()
	act.Status, act.CompletedAt = "completed", &now
}

func (s *Simulation) finishAction(actionID int64, status string, update func(d *droplet)) {
	err := s.store.Update(func(a *account) error {
		act := a.action(actionID)
		if act == nil {
			return nil
		}
		now := s.now().UTC()
		act.Status, act.CompletedAt = status, &now
		if d := a.droplet(act.ResourceID); d != nil {
			update(d)
			d.Locked = false
		}
		return nil
	})
	if err != nil {
		s.log.Error("record action result", "action", actionID, "error", err)
	}
	s.firewalls.kick()
}

type actionRequest struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Size string `json:"size"`
}

func (s *Simulation) startAction(a *account, d *droplet, req actionRequest) (*action, func(context.Context), error) {
	if d.Locked {
		return nil, nil, errUnprocessable("Droplet already has a pending event.")
	}
	if err := s.checkAction(a, d, req); err != nil {
		return nil, nil, err
	}
	act := s.newAction(a, req.Type, d)
	actionID, dropletID, machine, status := act.ID, d.ID, d.Machine, d.Status
	switch req.Type {
	case "rename":
		d.Name = req.Name
	case "resize":
		d.Size = req.Size
	case "enable_backups":
		d.Features = appendUnique(d.Features, "backups")
	case "disable_backups":
		d.Features = slices.DeleteFunc(d.Features, func(f string) bool { return f == "backups" })
	case "snapshot":
		snapID, name := a.nextID(), req.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", d.Name, s.now().Unix())
		}
		ref := fmt.Sprintf("stagely-sim-do-snapshot:%s-%d", a.UUID[:8], snapID)
		d.Locked = true
		return act, func(ctx context.Context) { s.takeSnapshot(ctx, actionID, dropletID, machine, snapID, name, ref) }, nil
	default:
		d.Locked = true
		kind := req.Type
		return act, func(ctx context.Context) { s.changePower(ctx, actionID, dropletID, machine, kind, status) }, nil
	}
	s.complete(act)
	return act, nil, nil
}

func (s *Simulation) checkAction(a *account, d *droplet, req actionRequest) error {
	if d.Machine == "" && req.Type != "rename" {
		return errUnprocessable("Droplet has no running machine; delete it and create a new one.")
	}
	switch req.Type {
	case "power_off", "shutdown":
		if d.Status == "off" {
			return errUnprocessable("Droplet is already powered off.")
		}
	case "power_on":
		if d.Status == "active" {
			return errUnprocessable("Droplet is already powered on.")
		}
	case "reboot":
		if d.Status != "active" {
			return errUnprocessable("Droplet must be powered on to reboot.")
		}
	case "power_cycle", "snapshot", "enable_backups", "disable_backups":
	case "rename":
		if len(req.Name) > 255 || !dropletNamePattern.MatchString(req.Name) {
			return errUnprocessable("Name is invalid")
		}
	case "resize":
		if _, ok := s.catalog.size(req.Size); !ok {
			return errUnprocessable("You specified an invalid size for Droplet resize.")
		}
		if d.Status != "off" {
			return errUnprocessable("Droplet must be powered off to resize.")
		}
	default:
		return errUnprocessable("action type %q is not supported by the simulation", req.Type)
	}
	return nil
}

func (s *Simulation) changePower(ctx context.Context, actionID, dropletID int64, machine, kind, status string) {
	var m machines.Machine
	var err error
	switch {
	case kind == "power_off" || kind == "shutdown":
		err = s.machines.Stop(ctx, machine, kind == "shutdown")
	case kind == "power_on" || (kind == "power_cycle" && status == "off"):
		m, err = s.machines.Start(ctx, machine)
	default:
		m, err = s.machines.Restart(ctx, machine, kind == "reboot")
	}
	s.firewalls.forget(dropletID)
	if err != nil {
		s.log.Error("droplet power action", "droplet", dropletID, "action", kind, "error", err)
		observed, inspectErr := s.machines.Inspect(ctx, machine)
		s.finishAction(actionID, "errored", func(d *droplet) {
			if inspectErr == nil {
				d.Status = statusOf(observed)
			}
		})
		return
	}
	s.finishAction(actionID, "completed", func(d *droplet) {
		d.Status = "active"
		if kind == "power_off" || kind == "shutdown" {
			d.Status = "off"
		} else {
			d.PrivateIP, d.PublicIP = m.PrivateIP, m.PublicIP
		}
	})
}

func statusOf(m machines.Machine) string {
	if m.Running {
		return "active"
	}
	return "off"
}

func (s *Simulation) takeSnapshot(ctx context.Context, actionID, dropletID int64, machine string, snapID int64, name, ref string) {
	size, err := s.machines.Commit(ctx, machine, ref)
	if err != nil {
		s.log.Error("snapshot droplet", "droplet", dropletID, "error", err)
		s.finishAction(actionID, "errored", func(*droplet) {})
		return
	}
	err = s.store.Update(func(a *account) error {
		d := a.droplet(dropletID)
		if d == nil {
			return nil
		}
		dropletSize, _ := s.catalog.size(d.Size)
		a.Snapshots = append(a.Snapshots, &snapshot{
			ID: snapID, Name: name, Region: d.Region, CreatedAt: s.now().UTC(), DropletID: d.ID,
			MinDiskSize: dropletSize.Disk, SizeGigabytes: math.Round(float64(size)/1e7) / 100, ImageRef: ref,
		})
		d.SnapshotIDs = append(d.SnapshotIDs, snapID)
		return nil
	})
	if err != nil {
		s.log.Error("record snapshot", "droplet", dropletID, "error", err)
	}
	s.finishAction(actionID, "completed", func(*droplet) {})
}

func (s *Simulation) postDropletAction(r *http.Request) (int, any, error) {
	var req actionRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out actionJSON
	var op func(context.Context)
	err := s.store.Update(func(a *account) error {
		d := a.dropletByPath(r.PathValue("id"))
		if d == nil {
			return errNotFound()
		}
		act, run, err := s.startAction(a, d, req)
		if err != nil {
			return err
		}
		out, op = s.renderAction(act), run
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if op != nil {
		s.background(op)
	}
	return http.StatusCreated, map[string]any{"action": out}, nil
}

var tagActionTypes = []string{"power_cycle", "power_on", "power_off", "shutdown", "enable_backups", "disable_backups", "snapshot"}

func (s *Simulation) postTagAction(r *http.Request) (int, any, error) {
	name := r.URL.Query().Get("tag_name")
	var req actionRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if name == "" {
		return 0, nil, errUnprocessable("tag_name is required")
	}
	if !slices.Contains(tagActionTypes, req.Type) {
		return 0, nil, errUnprocessable("action type %q can not be applied by tag", req.Type)
	}
	out := []actionJSON{}
	var ops []func(context.Context)
	err := s.store.Update(func(a *account) error {
		tagged := a.dropletsTagged(name)
		for _, d := range tagged {
			if d.Locked {
				return errUnprocessable("Droplet %d already has a pending event.", d.ID)
			}
			if err := s.checkAction(a, d, req); err != nil {
				return err
			}
		}
		for _, d := range tagged {
			act, run, err := s.startAction(a, d, req)
			if err != nil {
				return err
			}
			out = append(out, s.renderAction(act))
			if run != nil {
				ops = append(ops, run)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	for _, op := range ops {
		s.background(op)
	}
	return http.StatusCreated, map[string]any{"actions": out}, nil
}

func (s *Simulation) actionsOf(keep func(*action) bool) []actionJSON {
	var out []actionJSON
	s.store.Read(func(a *account) {
		for i := len(a.Actions) - 1; i >= 0; i-- {
			if keep(a.Actions[i]) {
				out = append(out, s.renderAction(a.Actions[i]))
			}
		}
	})
	return out
}

func (s *Simulation) listActions(r *http.Request) (int, any, error) {
	return listResponse(r, "actions", s.actionsOf(func(*action) bool { return true }))
}

func (s *Simulation) getAction(r *http.Request) (int, any, error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	found := s.actionsOf(func(act *action) bool { return act.ID == id })
	if len(found) == 0 {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"action": found[0]}, nil
}

func (s *Simulation) listDropletActions(r *http.Request) (int, any, error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	exists := false
	s.store.Read(func(a *account) { exists = a.droplet(id) != nil })
	if !exists {
		return 0, nil, errNotFound()
	}
	return listResponse(r, "actions", s.actionsOf(func(act *action) bool { return act.ResourceType == "droplet" && act.ResourceID == id }))
}

func (s *Simulation) getDropletAction(r *http.Request) (int, any, error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	actionID, _ := strconv.ParseInt(r.PathValue("action"), 10, 64)
	found := s.actionsOf(func(act *action) bool {
		return act.ID == actionID && act.ResourceType == "droplet" && act.ResourceID == id
	})
	if len(found) == 0 {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"action": found[0]}, nil
}
