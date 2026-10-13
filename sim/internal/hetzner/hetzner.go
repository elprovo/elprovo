// Package hetzner simulates the Hetzner Cloud API v1: locations, server
// types and images, SSH keys, servers and their actions, primary IPs,
// firewalls, networks and snapshots. Each server runs as a machine from the
// machines package, its firewalls are enforced inside the machine, and the
// machine serves Hetzner's instance metadata.
package hetzner

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

	"github.com/elprovo/elprovo/sim/internal/machines"
	"github.com/elprovo/elprovo/sim/internal/state"
)

const (
	cloudLabel      = "hetzner"
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
	Commit(ctx context.Context, id, ref string) (int64, error)
	RemoveImage(ctx context.Context, ref string) error
	Networks(ctx context.Context) (machines.Networks, error)
}

// Simulation serves the simulated Hetzner Cloud API.
type Simulation struct {
	cfg       config
	log       *slog.Logger
	store     *state.Store[project]
	machines  machineBackend
	now       func() time.Time
	limiter   *rateLimiter
	firewalls *firewallSync
	networks  machines.Networks

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the Hetzner simulation's flags on fs. The returned
// function builds the simulation after the flags are parsed; it returns nil
// when -hetzner is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var tokens repeatedFlag
	registerFlags(fs, &cfg, &tokens)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(tokens); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "hetzner")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "hetzner", "state.json"), backend, log)
	}
}

func newSimulation(cfg config, statePath string, backend machineBackend, log *slog.Logger) (*Simulation, error) {
	store, err := state.Open(statePath, project{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{
		cfg:       cfg,
		log:       log,
		store:     store,
		machines:  backend,
		now:       time.Now,
		limiter:   newRateLimiter(cfg.rateLimit),
		firewalls: newFirewallSync(),
	}
	s.bg, s.stopBG = context.WithCancel(context.Background())
	err = store.Update(func(p *project) error {
		if p.UUID == "" {
			p.UUID = newUUID()
			p.NextID = 40000000
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open hetzner state: %w", err)
	}
	return s, nil
}

// Run reconciles stored servers with the machines Docker has, then serves
// the API and keeps machine firewalls in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the hetzner api: %w", err)
	}
	if err := s.reconcile(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("reconcile servers with their machines: %w", err)
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
	s.log.Info("hetzner simulation listening", "addr", ln.Addr().String(), "network", s.cfg.network)

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
