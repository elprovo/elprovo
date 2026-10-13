// Package aws simulates the parts of AWS that Elprovo, its Terraform module
// and its Packer template use: the EC2 Query API (instances, key pairs, VPCs,
// subnets, internet gateways, route tables, security groups, Elastic IPs,
// AMIs, snapshots and volumes), STS GetCallerIdentity and
// GetFederationToken, and the ECR API. Requests must carry a valid Signature
// Version 4 signature of the configured access key or of a federated
// session's temporary credentials, whose session policy limits them. One
// listener serves every service, routed by the service in the signature's
// scope. Each instance runs as a machine from the machines package, security
// groups are enforced inside the machines, and the image registry is a CNCF
// Distribution registry that the operator runs with the simulation as its
// Docker token service.
package aws

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
	cloudLabel      = "aws"
	shutdownTimeout = 5 * time.Second
)

type machineBackend interface {
	Create(ctx context.Context, spec machines.Spec) (machines.Machine, error)
	Inspect(ctx context.Context, id string) (machines.Machine, error)
	List(ctx context.Context, cloud, account string) ([]machines.Machine, error)
	Start(ctx context.Context, id string) (machines.Machine, error)
	Stop(ctx context.Context, id string, graceful bool) error
	Delete(ctx context.Context, id string) error
	SetFirewall(ctx context.Context, id string, fw *machines.Firewall) error
	SetAddresses(ctx context.Context, id string, addrs []string) error
	Commit(ctx context.Context, id, ref string) (int64, error)
	RemoveImage(ctx context.Context, ref string) error
	Networks(ctx context.Context) (machines.Networks, error)
}

// Simulation serves the simulated AWS endpoints.
type Simulation struct {
	cfg       config
	statePath string
	log       *slog.Logger
	store     *state.Store[world]
	machines  machineBackend
	catalog   catalog
	now       func() time.Time
	firewalls *firewallSync
	networks  machines.Networks

	bg     context.Context
	stopBG context.CancelFunc
	ops    sync.WaitGroup
}

// Flags registers the AWS simulation's flags on fs. The returned function
// builds the simulation after the flags are parsed; it returns nil when -aws
// is off.
func Flags(fs *flag.FlagSet, machineOptions func() (machines.Options, error)) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	var cfg config
	var regions string
	registerFlags(fs, &cfg, &regions)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(regions); err != nil {
			return nil, err
		}
		options, err := machineOptions()
		if err != nil {
			return nil, err
		}
		log = log.With("simulation", "aws")
		backend := machines.New(cfg.network, options, log)
		return newSimulation(cfg, filepath.Join(dataDir, "aws", "state.json"), backend, log)
	}
}

func newSimulation(cfg config, statePath string, backend machineBackend, log *slog.Logger) (*Simulation, error) {
	store, err := state.Open(statePath, world{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{
		cfg:       cfg,
		statePath: statePath,
		log:       log,
		store:     store,
		machines:  backend,
		catalog:   catalog{regions: cfg.regions},
		now:       time.Now,
		firewalls: newFirewallSync(),
	}
	s.bg, s.stopBG = context.WithCancel(context.Background())
	err = store.Update(func(w *world) error {
		if w.AccountID == "" {
			w.AccountID = cfg.accountID
			if w.AccountID == "" {
				w.AccountID = randomAccountID()
			}
		}
		if cfg.accountID != "" && cfg.accountID != w.AccountID {
			return fmt.Errorf("the state belongs to account %s, not %s", w.AccountID, cfg.accountID)
		}
		if w.Tags == nil {
			w.Tags = map[string][]tag{}
		}
		if w.Sessions == nil {
			w.Sessions = map[string]*session{}
		}
		if w.RegistryTokens == nil {
			w.RegistryTokens = map[string]*registryToken{}
		}
		if w.RegistrySigner == nil {
			signer, err := registrytoken.NewSigner(s.now())
			if err != nil {
				return err
			}
			w.RegistrySigner = signer
		}
		for _, region := range cfg.regions {
			if w.defaultVPC(region) == nil {
				s.createDefaultVPC(w, region)
			}
			addStockImages(w, region)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open aws state: %w", err)
	}
	return s, nil
}

// Run reconciles stored instances with the machines Docker has, then serves
// the endpoints and keeps machine firewalls in sync until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listen for the aws endpoints: %w", err)
	}
	if err := s.writeRegistryCert(); err != nil {
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
	wg.Go(func() { s.syncFirewalls(s.bg) })
	var account string
	s.store.Read(func(w *world) { account = w.AccountID })
	s.log.Info("aws simulation listening", "addr", ln.Addr().String(), "network", s.cfg.network, "account", account)

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

func randomAccountID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(9e11))
	return fmt.Sprintf("%012d", n.Int64()+1e11)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// newID returns an EC2 resource id such as i-0123456789abcdef0: the prefix
// and 17 hexadecimal digits.
func newID(prefix string) string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return prefix + "-" + fmt.Sprintf("%x", b)[:17]
}
