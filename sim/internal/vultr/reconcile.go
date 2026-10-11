package vultr

import (
	"context"
	"slices"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

// reconcile matches stored instances with the machines Docker has before the
// API serves: an instance takes its power state and addresses from its
// machine, an instance without a machine or with an interrupted deletion is
// removed, a snapshot that was being taken is dropped, and a machine of this
// account that no instance owns is deleted.
func (s *Simulation) reconcile(ctx context.Context) error {
	nets, err := s.machines.Networks(ctx)
	if err != nil {
		return err
	}
	s.networks = nets
	var accountID string
	s.store.Read(func(a *account) { accountID = a.ID })
	found, err := s.machines.List(ctx, cloudLabel, accountID)
	if err != nil {
		return err
	}
	byInstance := map[string]machines.Machine{}
	for _, m := range found {
		byInstance[m.Resource] = m
	}
	var interrupted []string
	err = s.store.Update(func(a *account) error {
		for _, inst := range slices.Clone(a.Instances) {
			m, ok := byInstance[inst.ID]
			if !ok || inst.Destroying {
				s.log.Warn("removing an instance whose machine is gone or was being deleted", "instance", inst.ID, "label", inst.Label)
				a.removeInstance(inst.ID)
				continue
			}
			delete(byInstance, inst.ID)
			inst.Machine, inst.PrivateIP, inst.PublicIP = m.ID, m.PrivateIP, m.PublicIP
			inst.Status, inst.PowerStatus, inst.ServerStatus = "active", "stopped", "none"
			if m.Running {
				inst.PowerStatus, inst.ServerStatus = "running", "ok"
			}
		}
		for _, snap := range slices.Clone(a.Snapshots) {
			if snap.Status != "complete" {
				interrupted = append(interrupted, snap.ImageRef)
				a.Snapshots = slices.DeleteFunc(a.Snapshots, func(other *snapshot) bool { return other.ID == snap.ID })
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, ref := range interrupted {
		if err := s.machines.RemoveImage(ctx, ref); err != nil {
			return err
		}
	}
	for _, m := range byInstance {
		s.log.Warn("deleting a machine that no instance owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	s.firewalls.kick()
	return nil
}
