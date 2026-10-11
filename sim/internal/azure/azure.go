// Package azure simulates the parts of Microsoft Azure that Stagely uses:
// Microsoft Entra ID's client credentials token endpoint, the Azure Resource
// Manager metadata document for custom clouds, and Resource Manager with
// resource groups, virtual networks and subnets, network security groups,
// public IP addresses, network interfaces, virtual machines, managed disks
// and images, container registries and template deployments. Each virtual
// machine runs as a machine from the machines package, and its network
// security groups are enforced inside the machine.
package azure

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
	"github.com/stagely-dev/stagely/sim/internal/state"
)

const (
	cloudLabel      = "azure"
	shutdownTimeout = 5 * time.Second
)

type machineBackend interface {
	Create(ctx context.Context, spec machines.Spec) (machines.Machine, error)
	Inspect(ctx context.Context, id string) (machines.Machine, error)
	List(ctx context.Context, cloud, account string) ([]machines.Machine, error)
	Start(ctx context.Context, id string) (machines.Machine, error)
	Stop(ctx context.Context, id string, graceful bool) error
	Restart(ctx context.Context, id string, graceful bool) (machines.Machine, error)
	Delete(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error)
	SetFirewall(ctx context.Context, id string, fw *machines.Firewall) error
	SetAddresses(ctx context.Context, id string, addrs []string) error
	Commit(ctx context.Context, id, ref string) (int64, error)
	RemoveImage(ctx context.Context, ref string) error
	Networks(ctx context.Context) (machines.Networks, error)
}

// Simulation serves the simulated Azure endpoints.
type Simulation struct {
	cfg       config
	log       *slog.Logger
	store     *state.Store[subscription]
	machines  machineBackend
	now       func() time.Time
	firewalls *firewallSync
	networks  machines.Networks
	routes    []route

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the Azure simulation's flags on fs. The returned function
// builds the simulation after the flags are parsed; it returns nil when
// -azure is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var locations string
	registerFlags(fs, &cfg, &locations)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(locations); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "azure")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "azure", "state.json"), backend, log)
	}
}

func newSimulation(cfg config, statePath string, backend machineBackend, log *slog.Logger) (*Simulation, error) {
	store, err := state.Open(statePath, subscription{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{cfg: cfg, log: log, store: store, machines: backend, now: time.Now, firewalls: newFirewallSync()}
	s.routes = s.armRoutes()
	s.bg, s.stopBG = context.WithCancel(context.Background())
	err = store.Update(func(sub *subscription) error {
		if sub.Account == "" {
			sub.Account = newUUID()
		}
		if len(sub.SigningKey) == 0 {
			sub.SigningKey = make([]byte, 32)
			_, _ = rand.Read(sub.SigningKey)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open azure state: %w", err)
	}
	return s, nil
}

// Run reconciles stored virtual machines with the machines Docker has, then
// serves the endpoints and keeps machine firewalls in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the azure endpoints: %w", err)
	}
	if err := s.reconcile(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("reconcile virtual machines with their machines: %w", err)
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		var err error
		if s.cfg.tlsCert != "" {
			err = srv.ServeTLS(ln, s.cfg.tlsCert, s.cfg.tlsKey)
		} else {
			err = srv.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	})
	wg.Go(func() { s.syncFirewalls(s.bg) })
	s.log.Info("azure simulation listening", "addr", ln.Addr().String(), "tls", s.cfg.tlsCert != "", "network", s.cfg.network)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = errors.Join(serveErr, srv.Shutdown(shutdownCtx))
	s.close()
	wg.Wait()
	return err
}

func (s *Simulation) close() {
	s.stopBG()
	s.ops.Wait()
}

func (s *Simulation) background(fn func(ctx context.Context)) {
	s.ops.Go(func() { fn(s.bg) })
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
