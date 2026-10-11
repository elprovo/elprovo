// Package vultr simulates the Vultr API v2: the account, regions, plans and
// operating systems, SSH keys, instances, VPCs, firewall groups, reserved IPs,
// snapshots and the container registry. Each instance runs as a machine from
// the machines package, firewall groups are enforced inside the machines, and
// a registry's images live in a CNCF Distribution registry that the operator
// runs under the registry's host name. Every VPC's traffic runs on the
// machines' private Docker network, which firewall groups do not filter.
package vultr

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
	cloudLabel      = "vultr"
	shutdownTimeout = 5 * time.Second
)

type machineBackend interface {
	Create(ctx context.Context, spec machines.Spec) (machines.Machine, error)
	List(ctx context.Context, cloud, account string) ([]machines.Machine, error)
	Delete(ctx context.Context, id string) error
	SetFirewall(ctx context.Context, id string, fw *machines.Firewall) error
	SetAddresses(ctx context.Context, id string, addrs []string) error
	Commit(ctx context.Context, id, ref string) (int64, error)
	RemoveImage(ctx context.Context, ref string) error
	Networks(ctx context.Context) (machines.Networks, error)
}

// Simulation serves the simulated Vultr API.
type Simulation struct {
	cfg       config
	log       *slog.Logger
	store     *state.Store[account]
	machines  machineBackend
	now       func() time.Time
	limiter   *rateLimiter
	firewalls *firewallSync
	networks  machines.Networks

	// addressMu orders the changes of machines' reserved IPs, so two
	// requests can't push lists that miss each other's address.
	addressMu sync.Mutex

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the Vultr simulation's flags on fs. The returned function
// builds the simulation after the flags are parsed; it returns nil when
// -vultr is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var apiKeys repeatedFlag
	registerFlags(fs, &cfg, &apiKeys)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(apiKeys); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "vultr")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "vultr", "state.json"), backend, log)
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
		now:       time.Now,
		limiter:   newRateLimiter(cfg.rateLimit),
		firewalls: newFirewallSync(),
	}
	s.bg, s.stopBG = context.WithCancel(context.Background())
	err = store.Update(func(a *account) error {
		if a.ID == "" {
			a.ID = newUUID()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open vultr state: %w", err)
	}
	return s, nil
}

// Run reconciles stored instances with the machines Docker has, then serves
// the API and keeps machine firewalls in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the vultr api: %w", err)
	}
	if err := s.reconcile(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("reconcile instances with their machines: %w", err)
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
	s.log.Info("vultr simulation listening", "addr", ln.Addr().String(), "tls", s.cfg.tlsCert != "", "network", s.cfg.network)

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

// timestamp formats t as Vultr does, such as 2020-10-10T01:56:20+00:00.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-07:00")
}
