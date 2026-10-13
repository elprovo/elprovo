package gcp

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const pushRetryDelay = 5 * time.Second

// pushSync remembers what the simulation last pushed to each instance's
// machine, so it pushes only changes. kick asks the sync loop to look again.
type pushSync struct {
	kicks  chan struct{}
	mu     sync.Mutex
	pushed map[uint64]string
}

func newPushSync() *pushSync {
	return &pushSync{kicks: make(chan struct{}, 1), pushed: map[uint64]string{}}
}

func (p *pushSync) kick() {
	select {
	case p.kicks <- struct{}{}:
	default:
	}
}

func (p *pushSync) applied(id uint64, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushed[id] = key
}

func (p *pushSync) forget(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pushed, id)
}

func (p *pushSync) has(id uint64, key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.pushed[id]
	return ok && v == key
}

func jsonKey(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// syncMachines keeps each running instance's machine on the firewall that
// its network's rules give it and on its static external address. Failed
// pushes are retried.
func (s *Simulation) syncMachines(ctx context.Context) {
	retry := time.NewTimer(pushRetryDelay)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.firewalls.kicks:
		case <-s.addresses.kicks:
		case <-retry.C:
		}
		if !s.pushToMachines(ctx) {
			retry.Reset(pushRetryDelay)
		}
	}
}

type machinePush struct {
	instanceID uint64
	machine    string
	policy     *machines.Firewall
	addresses  []string
}

func (s *Simulation) pushToMachines(ctx context.Context) bool {
	var firewallPushes, addressPushes []machinePush
	s.store.Read(func(w *world) {
		for _, inst := range w.Instances {
			if inst.Status != "RUNNING" || inst.Machine == "" {
				continue
			}
			if p := policyFor(w, inst); !s.firewalls.has(inst.ID, jsonKey(p)) {
				firewallPushes = append(firewallPushes, machinePush{instanceID: inst.ID, machine: inst.Machine, policy: p})
			}
			if a := inst.extraAddresses(); !s.addresses.has(inst.ID, jsonKey(a)) {
				addressPushes = append(addressPushes, machinePush{instanceID: inst.ID, machine: inst.Machine, addresses: a})
			}
		}
	})
	ok := true
	for _, p := range firewallPushes {
		if err := s.machines.SetFirewall(ctx, p.machine, p.policy); err != nil {
			s.log.Warn("push firewall to instance machine", "instance", p.instanceID, "error", err)
			ok = false
			continue
		}
		s.firewalls.applied(p.instanceID, jsonKey(p.policy))
	}
	for _, p := range addressPushes {
		if err := s.machines.SetAddresses(ctx, p.machine, p.addresses); err != nil {
			s.log.Warn("push external address to instance machine", "instance", p.instanceID, "error", err)
			ok = false
			continue
		}
		s.addresses.applied(p.instanceID, jsonKey(p.addresses))
	}
	return ok
}
