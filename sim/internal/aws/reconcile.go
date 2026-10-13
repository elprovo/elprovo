package aws

import (
	"context"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

// reconcile matches stored instances with the machines Docker has, after a
// restart: an instance takes its state and addresses from its machine, an
// instance whose machine is gone is terminated, interrupted work is settled,
// and a machine of this account that no instance owns is deleted.
func (s *Simulation) reconcile(ctx context.Context) error {
	nets, err := s.machines.Networks(ctx)
	if err != nil {
		return err
	}
	s.networks = nets
	var account string
	s.store.Read(func(w *world) { account = w.AccountID })
	found, err := s.machines.List(ctx, cloudLabel, account)
	if err != nil {
		return err
	}
	byInstance := map[string]machines.Machine{}
	for _, m := range found {
		byInstance[m.Resource] = m
	}
	var destroys []destruction
	err = s.store.Update(func(w *world) error {
		now := s.now().UTC()
		for _, i := range w.Instances {
			if i.State == "terminated" {
				continue
			}
			i.Busy = false
			m, ok := byInstance[i.ID]
			if !ok {
				s.log.Warn("instance has no machine; it is terminated", "instance", i.ID)
				i.State, i.StateReason, i.Machine, i.TerminatedAt = "terminated", "Server.InternalError", "", now
				w.releaseInstance(i)
				continue
			}
			delete(byInstance, i.ID)
			i.Machine, i.PrivateIP, i.MachinePublicIP = m.ID, m.PrivateIP, m.PublicIP
			if i.State == "shutting-down" {
				i.Busy = true
				destroys = append(destroys, destruction{instanceID: i.ID, machine: m.ID})
				continue
			}
			i.State = "stopped"
			if m.Running {
				i.State = "running"
			}
		}
		for _, img := range w.Images {
			if img.State == "pending" {
				img.State, img.StateReason = "failed", "Server.InternalError"
			}
		}
		for _, snap := range w.Snapshots {
			if snap.State == "pending" {
				snap.State = "error"
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range byInstance {
		s.log.Warn("deleting a machine that no instance owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	for _, job := range destroys {
		s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	}
	s.firewalls.kick()
	return nil
}
