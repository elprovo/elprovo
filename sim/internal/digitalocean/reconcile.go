package digitalocean

import (
	"context"
	"strconv"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

func (s *Simulation) reconcile(ctx context.Context) error {
	nets, err := s.machines.Networks(ctx)
	if err != nil {
		return err
	}
	s.networks = nets
	var accountID string
	s.store.Read(func(a *account) { accountID = a.UUID })
	found, err := s.machines.List(ctx, cloudLabel, accountID)
	if err != nil {
		return err
	}
	byDroplet := map[string]machines.Machine{}
	for _, m := range found {
		byDroplet[m.Resource] = m
	}
	err = s.store.Update(func(a *account) error {
		for _, d := range append([]*droplet(nil), a.Droplets...) {
			m, ok := byDroplet[strconv.FormatInt(d.ID, 10)]
			if !ok {
				s.log.Warn("droplet has no machine; removing the droplet", "droplet", d.ID, "name", d.Name)
				a.removeDroplet(d.ID)
				continue
			}
			delete(byDroplet, m.Resource)
			d.Machine, d.PrivateIP, d.PublicIP, d.Locked, d.Status = m.ID, m.PrivateIP, m.PublicIP, false, statusOf(m)
		}
		for _, r := range a.ReservedIPs {
			r.Locked = false
			if a.droplet(r.DropletID) == nil {
				r.DropletID = 0
			}
		}
		now := s.now().UTC()
		for _, act := range a.Actions {
			if act.Status == "in-progress" {
				act.Status, act.CompletedAt = "errored", &now
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range byDroplet {
		s.log.Warn("deleting a machine that no droplet owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	s.firewalls.kick()
	return nil
}
