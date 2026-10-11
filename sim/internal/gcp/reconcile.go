package gcp

import (
	"context"
	"slices"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

// reconcile matches stored instances with the machines Docker has: an
// instance takes its addresses and state from its machine, an instance whose
// machine is gone is removed with its auto-delete disk, an operation that
// was running when the simulation stopped ends with an error, and a machine
// of this project that no instance owns is deleted. It creates the Docker
// networks when they are missing, so clients can join them before the first
// instance exists.
func (s *Simulation) reconcile(ctx context.Context) error {
	if _, err := s.machines.Networks(ctx); err != nil {
		return err
	}
	var account string
	s.store.Read(func(w *world) { account = w.Account })
	found, err := s.machines.List(ctx, cloudLabel, account)
	if err != nil {
		return err
	}
	byInstance := map[string]machines.Machine{}
	for _, m := range found {
		byInstance[m.Resource] = m
	}
	var released []string
	err = s.store.Update(func(w *world) error {
		for _, inst := range slices.Clone(w.Instances) {
			m, ok := byInstance[idString(inst.ID)]
			if !ok {
				s.log.Warn("instance has no machine; removing the instance", "instance", inst.Name, "zone", inst.Zone)
				w.Instances = slices.DeleteFunc(w.Instances, func(i *instance) bool { return i == inst })
				for _, d := range w.Disks {
					if d.User == inst.ID && !inst.AutoDelete {
						d.User = 0
					}
				}
				released = append(released, removeDisksOf(w, inst.ID)...)
				continue
			}
			delete(byInstance, m.Resource)
			inst.Machine, inst.PrivateIP, inst.PublicIP = m.ID, m.PrivateIP, m.PublicIP
			inst.Status = "TERMINATED"
			if m.Running {
				inst.Status = "RUNNING"
			}
		}
		for _, op := range w.Operations {
			if op.Status != "DONE" {
				s.done(op, &opError{"INTERNAL_ERROR", "The simulation stopped before the operation finished.", 500})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.releaseImageRefs(ctx, released)
	for _, m := range byInstance {
		s.log.Warn("deleting a machine that no instance owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	s.firewalls.kick()
	s.addresses.kick()
	return nil
}
