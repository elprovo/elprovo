// Package gcp simulates the Google Cloud APIs that Elprovo uses: the OAuth 2.0
// token endpoint for service account keys, Compute Engine (zones, regions,
// machine types, images, instances, disks, networks, subnetworks, firewall
// rules, static addresses and operations), Artifact Registry repositories,
// and the Docker token service of Artifact Registry's registry hosts. Each
// instance runs as a machine from the machines package, and firewall rules
// are enforced inside the machines.
package gcp

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
	"github.com/elprovo/elprovo/sim/internal/registrytoken"
	"github.com/elprovo/elprovo/sim/internal/state"
)

const (
	cloudLabel      = "gcp"
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

// Simulation serves the simulated Google Cloud APIs.
type Simulation struct {
	cfg       config
	dataDir   string
	log       *slog.Logger
	store     *state.Store[world]
	machines  machineBackend
	catalog   catalog
	now       func() time.Time
	firewalls *pushSync
	addresses *pushSync

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the Google Cloud simulation's flags on fs. The returned
// function builds the simulation after the flags are parsed; it returns nil
// when -gcp is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var tokens repeatedFlag
	var zones string
	registerFlags(fs, &cfg, &tokens, &zones)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(tokens, zones); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "gcp")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "gcp"), backend, log)
	}
}

func newSimulation(cfg config, dataDir string, backend machineBackend, log *slog.Logger) (*Simulation, error) {
	statePath := ""
	if dataDir != "" {
		statePath = filepath.Join(dataDir, "state.json")
	}
	store, err := state.Open(statePath, world{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{
		cfg:       cfg,
		dataDir:   dataDir,
		log:       log,
		store:     store,
		machines:  backend,
		catalog:   newCatalog(cfg.zones),
		now:       time.Now,
		firewalls: newPushSync(),
		addresses: newPushSync(),
	}
	s.bg, s.stopBG = context.WithCancel(context.Background())
	if err := store.Update(s.initWorld); err != nil {
		return nil, fmt.Errorf("open gcp state: %w", err)
	}
	return s, nil
}

// initWorld gives a new project its number, its service account key, the
// registry's token signing key and the default network that new Google
// Cloud projects have. A project keeps no default network once it deleted
// it.
func (s *Simulation) initWorld(w *world) error {
	isNew := w.Account == ""
	if isNew {
		w.Account = newUUID()
		n, err := rand.Int(rand.Reader, big.NewInt(9e11))
		if err != nil {
			return err
		}
		w.ProjectNumber = 1e11 + n.Int64()
		w.NextID = 1e18 + uint64(n.Int64())*1000
	}
	if w.Tokens == nil {
		w.Tokens = map[string]issuedToken{}
	}
	if len(w.Keys) == 0 {
		key, err := newServiceAccountKey(s.cfg.project, w.ProjectNumber, s.now())
		if err != nil {
			return err
		}
		w.Keys = append(w.Keys, key)
		w.Accounts = append(w.Accounts, &iamAccount{
			Email: key.Email, UniqueID: key.ClientID, DisplayName: "elprovo-sim", CreatedAt: s.now().UTC(),
		})
	}
	if w.Registry == nil {
		signer, err := registrytoken.NewSigner(s.now())
		if err != nil {
			return err
		}
		w.Registry = signer
	}
	if isNew {
		s.addDefaultNetwork(w)
	}
	return nil
}

// Run reconciles stored instances with the machines Docker has, then serves
// the APIs and keeps machine firewalls and addresses in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the google cloud apis: %w", err)
	}
	if err := s.writeFiles(); err != nil {
		_ = ln.Close()
		return err
	}
	if err := s.reconcile(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("reconcile instances with their machines: %w", err)
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	})
	wg.Go(func() { s.syncMachines(s.bg) })
	s.log.Info("google cloud simulation listening", "addr", ln.Addr().String(), "project", s.cfg.project,
		"network", s.cfg.network, "credentials", filepath.Join(s.dataDir, keyFileName))

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	s.stopBG()
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
