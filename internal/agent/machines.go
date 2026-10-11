package agent

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/stagely-dev/stagely/protocol"
)

// workerPool hands out build workers. The pool has a fixed set of names, so
// it never holds more workers than its size; a worker is created the first
// time its name is taken and kept for later builds.
type workerPool struct {
	free chan string
}

func newWorkerPool(prefix string, size int) *workerPool {
	p := &workerPool{free: make(chan string, size)}
	for n := 1; n <= size; n++ {
		p.free <- workerName(prefix, n)
	}
	return p
}

func (p *workerPool) acquire(ctx context.Context) (string, error) {
	select {
	case name := <-p.free:
		return name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *workerPool) release(name string) {
	p.free <- name
}

// ensureMachine creates the machine unless it exists. The journal holds the
// intent while the create runs. A failed create deletes whatever the
// provider made, so no half-created machine stays behind.
func (r *Runner) ensureMachine(ctx context.Context, name string) error {
	exists, err := r.Machines.Exists(ctx, name)
	if err != nil || exists {
		return err
	}
	if err := r.setMachineState(ctx, name, protocol.MachineCreating); err != nil {
		return err
	}
	if err := r.Machines.Create(ctx, name); err != nil {
		if cleanupErr := r.deleteMachine(context.WithoutCancel(ctx), name); cleanupErr != nil {
			r.Log.Error("delete half-created machine", "machine", name, "error", cleanupErr)
		}
		return err
	}
	return r.setMachineState(ctx, name, "")
}

// deleteMachine deletes the machine; the provider confirms it is gone. The
// journal holds the intent until then, so a restart retries the delete.
func (r *Runner) deleteMachine(ctx context.Context, name string) error {
	if err := r.setMachineState(ctx, name, protocol.MachineDeleting); err != nil {
		return err
	}
	if err := r.Machines.Delete(ctx, name); err != nil {
		return err
	}
	return r.setMachineState(ctx, name, "")
}

func (r *Runner) setMachineState(ctx context.Context, name, state string) error {
	if err := r.Journal.SetMachine(ctx, name, state); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if state == "" {
		delete(r.machines, name)
		return nil
	}
	r.machines[name] = state
	return nil
}

func (r *Runner) machineState(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.machines[name]; ok {
		return state
	}
	return protocol.MachineReady
}

// reportInventory sends the machines this agent created, now and then every
// InventoryEvery, until the session ends. Build workers above the pool size
// are deleted, because the pool no longer hands them out.
func (r *Runner) reportInventory(ctx context.Context, s Session) {
	for {
		inv, extra, err := r.inventory(ctx)
		if err != nil {
			r.Log.Warn("list machines", "error", err)
		} else if err := s.Send(ctx, protocol.TypeInventory, inv); err != nil {
			return
		}
		for _, name := range extra {
			r.Log.Info("deleting build worker above the pool size", "machine", name)
			if err := r.deleteMachine(ctx, name); err != nil {
				r.Log.Warn("delete extra build worker", "machine", name, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.InventoryEvery):
		}
	}
}

func (r *Runner) inventory(ctx context.Context) (protocol.Inventory, []string, error) {
	names, err := r.Machines.List(ctx)
	if err != nil {
		return protocol.Inventory{}, nil, err
	}
	slices.Sort(names)
	inv := protocol.Inventory{Machines: []protocol.Machine{}}
	var extra []string
	for _, name := range slices.Compact(names) {
		kind, stageletID, ok := r.machineKind(name)
		if !ok {
			continue
		}
		inv.Machines = append(inv.Machines, protocol.Machine{Name: name, Kind: kind, StageletID: stageletID, Status: r.machineState(name)})
		if kind == protocol.MachineBuildWorker && workerIndex(r.MachinePrefix, name) > r.BuildWorkers {
			extra = append(extra, name)
		}
	}
	return inv, extra, nil
}

func workerIndex(prefix, name string) int {
	m := buildWorkerPattern.FindStringSubmatch(name[len(prefix):])
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
