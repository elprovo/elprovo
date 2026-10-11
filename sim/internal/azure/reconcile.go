package azure

import (
	"context"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

// reconcile matches stored virtual machines with the machines Docker has,
// fails the work that a restart interrupted, and deletes this simulation's
// machines that no virtual machine owns.
func (s *Simulation) reconcile(ctx context.Context) error {
	nets, err := s.machines.Networks(ctx)
	if err != nil {
		return err
	}
	s.networks = nets
	var account string
	s.store.Read(func(sub *subscription) { account = sub.Account })
	found, err := s.machines.List(ctx, cloudLabel, account)
	if err != nil {
		return err
	}
	byVM := map[string]machines.Machine{}
	for _, m := range found {
		byVM[m.Resource] = m
	}
	interrupted := &armError{status: 500, code: "OperationInterrupted", message: "The simulation restarted while the operation ran."}
	err = s.store.Update(func(sub *subscription) error {
		for _, v := range append([]*vm(nil), sub.VMs...) {
			m, ok := byVM[v.VMID]
			if !ok {
				if v.Machine != "" {
					s.log.Warn("virtual machine has no machine; removing the virtual machine", "group", v.Group, "vm", v.Name)
					sub.VMs = without(sub.VMs, v.Group, v.Name)
				} else if v.ProvisioningState == "Creating" {
					v.ProvisioningState = statusFailed
				}
				continue
			}
			delete(byVM, v.VMID)
			v.Machine = m.ID
			if v.ProvisioningState == "Creating" || v.ProvisioningState == "Deleting" {
				v.ProvisioningState = statusFailed
			}
			switch {
			case m.Running:
				v.PowerState = "running"
			case v.PowerState != "deallocated":
				v.PowerState = "stopped"
			}
		}
		for _, img := range sub.Images {
			if img.ProvisioningState == "Creating" {
				img.ProvisioningState = statusFailed
			}
		}
		for _, d := range sub.Deployments {
			if d.ProvisioningState == "Running" {
				d.ProvisioningState, d.Error = statusFailed, opErrorOf(interrupted)
			}
		}
		for _, g := range sub.Groups {
			g.Deleting = false
		}
		for _, op := range sub.Operations {
			if op.Status == statusInProgress {
				s.finishOperation(sub, op.ID, interrupted)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range byVM {
		s.log.Warn("deleting a machine that no virtual machine owns", "machine", m.Name)
		if err := s.machines.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	s.firewalls.kick()
	return nil
}
