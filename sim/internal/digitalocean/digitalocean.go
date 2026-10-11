// Package digitalocean simulates the DigitalOcean API v2: the account,
// regions, sizes and images, SSH keys, droplets and their actions, tags,
// VPCs, firewalls, snapshots and the container registry. Each droplet runs
// as a machine from the machines package, firewalls are enforced inside the
// machines, and the container registry is a CNCF Distribution registry that
// the operator runs.
package digitalocean

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
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
	cloudLabel      = "digitalocean"
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
	SetFirewall(ctx context.Context, id string, fw *machines.Firewall) error
	SetAddresses(ctx context.Context, id string, addrs []string) error
	Commit(ctx context.Context, id, ref string) (int64, error)
	RemoveImage(ctx context.Context, ref string) error
	Networks(ctx context.Context) (machines.Networks, error)
}

// Simulation serves the simulated DigitalOcean API.
type Simulation struct {
	cfg       config
	log       *slog.Logger
	store     *state.Store[account]
	machines  machineBackend
	catalog   catalog
	now       func() time.Time
	limiter   *rateLimiter
	firewalls *firewallSync
	networks  machines.Networks

	registryHTTP *http.Client

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the DigitalOcean simulation's flags on fs. The returned
// function builds the simulation after the flags are parsed; it returns nil
// when -digitalocean is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var tokens, publish repeatedFlag
	var regions string
	registerFlags(fs, &cfg, &tokens, &publish, &regions)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(tokens, publish, regions); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "digitalocean")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "digitalocean", "state.json"), backend, log)
	}
}

func newSimulation(cfg config, statePath string, backend machineBackend, log *slog.Logger) (*Simulation, error) {
	store, err := state.Open(statePath, account{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{
		cfg:       cfg,
		log:       log,
		store:     store,
		machines:  backend,
		catalog:   catalog{regions: cfg.regions},
		now:       time.Now,
		limiter:   newRateLimiter(cfg.limits),
		firewalls: newFirewallSync(),

		registryHTTP: &http.Client{Timeout: 10 * time.Second},
	}
	s.bg, s.stopBG = context.WithCancel(context.Background())
	err = store.Update(func(a *account) error {
		if a.UUID == "" {
			a.UUID = newUUID()
			a.NextID = 100000000
		}
		for _, region := range cfg.regions {
			if a.defaultVPC(region) == nil {
				a.VPCs = append(a.VPCs, &vpc{
					ID: newUUID(), Name: "default-" + region, Region: region,
					IPRange: freeVPCRange(a), Default: true, CreatedAt: s.now().UTC(),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open digitalocean state: %w", err)
	}
	return s, nil
}

// Run reconciles stored droplets with the machines Docker has, then serves
// the API and keeps machine firewalls in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the digitalocean api: %w", err)
	}
	if err := s.reconcile(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("reconcile droplets with their machines: %w", err)
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	})
	wg.Go(func() { s.syncFirewalls(s.bg) })
	s.log.Info("digitalocean simulation listening", "addr", ln.Addr().String(), "network", s.cfg.network)

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
