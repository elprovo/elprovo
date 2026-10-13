package hetzner

import (
	"context"
	"slices"
	"strconv"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

// reconcile matches stored servers with the machines Docker has. A server
// takes its status and private address from its machine; a server whose
// machine is gone is removed; an action that was running when the
// simulation stopped fails; a snapshot that was being taken is dropped; and
// a machine of this project that no server owns is deleted.
func (s *Simulation) reconcile(ctx context.Context) error {
	nets, err := s.machines.Networks(ctx)
	if err != nil {
		return err
	}
	s.networks = nets
	var projectID string
	s.store.Read(func(p *project) { projectID = p.UUID })
	found, err := s.machines.List(ctx, cloudLabel, projectID)
	if err != nil {
		return err
	}
	byServer := map[string]machines.Machine{}
	for _, m := range found {
		byServer[m.Resource] = m
	}
	var orphanImages []string
	err = s.store.Update(func(p *project) error {
		for _, srv := range slices.Clone(p.Servers) {
			m, ok := byServer[strconv.FormatInt(srv.ID, 10)]
			if !ok {
				s.log.Warn("server has no machine; removing the server", "server", srv.ID, "name", srv.Name)
				p.removeServer(srv.ID)
				continue
			}
			delete(byServer, m.Resource)
			srv.Machine, srv.PrivateIP, srv.Status = m.ID, m.PrivateIP, statusOf(m)
		}
		for _, img := range p.Images {
			if img.Status == "creating" {
				orphanImages = append(orphanImages, img.Ref)
			}
		}
		p.Images = slices.DeleteFunc(p.Images, func(img *image) bool { return img.Status == "creating" })
		for _, a := range p.Actions {
			if a.Status == "running" {
				s.finish(a, &actionError{Code: "action_failed", Message: "the simulation stopped while the action ran"})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, ref := range orphanImages {
		if err := s.machines.RemoveImage(ctx, ref); err != nil {
			return err
		}
	}
	for _, m := range byServer {
		s.log.Warn("deleting a machine that no server owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	s.firewalls.kick()
	return nil
}
