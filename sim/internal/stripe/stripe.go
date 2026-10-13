// Package stripe simulates the parts of the Stripe API that Elprovo uses to
// sell its subscription plans: customers, products and prices, Checkout
// Sessions with a hosted payment page, subscriptions, invoices, the billing
// portal, test clocks, and events delivered to signed webhook endpoints.
package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/elprovo/elprovo/sim/internal/state"
)

const (
	sessionSweepInterval = 5 * time.Second
	shutdownTimeout      = 5 * time.Second
)

// Simulation serves the simulated Stripe API on one listener and the hosted
// Checkout, billing portal and invoice pages, plus the /_sim/ admin API, on
// another.
type Simulation struct {
	cfg      config
	log      *slog.Logger
	store    *state.Store[account]
	now      func() time.Time
	hooks    *dispatcher
	pages    *template.Template
	inflight inflightKeys
}

// Flags registers the Stripe simulation's flags on fs. The returned function
// builds the simulation after the flags are parsed; it returns nil when
// -stripe is off.
func Flags(fs *flag.FlagSet) func(dataDir string, log *slog.Logger) (*Simulation, error) {
	cfg := config{deliveryRetries: []time.Duration{time.Second, 10 * time.Second, time.Minute, 10 * time.Minute}}
	var keys, hooks repeatedFlag
	registerFlags(fs, &cfg, &keys, &hooks)
	return func(dataDir string, log *slog.Logger) (*Simulation, error) {
		if !cfg.enabled {
			return nil, nil
		}
		if err := cfg.finish(keys, hooks); err != nil {
			return nil, err
		}
		return newSimulation(cfg, filepath.Join(dataDir, "stripe", "state.json"), log)
	}
}

func newSimulation(cfg config, statePath string, log *slog.Logger) (*Simulation, error) {
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(statePath, account{})
	if err != nil {
		return nil, err
	}
	s := &Simulation{
		cfg:      cfg,
		log:      log.With("simulation", "stripe"),
		store:    store,
		now:      time.Now,
		pages:    pages,
		inflight: inflightKeys{keys: map[string]bool{}},
	}
	s.hooks = newDispatcher(cfg.deliveryRetries, s.log, s.deliveryTarget, s.markDelivered)
	err = store.Update(func(a *account) error {
		a.ensureMaps()
		seedCatalog(a, cfg.seedPrices(), s.now())
		syncFlagEndpoints(a, cfg.webhooks, s.now())
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("seed stripe state: %w", err)
	}
	return s, nil
}

// Run serves the API and the hosted pages, delivers webhooks and expires
// Checkout Sessions until ctx ends.
func (s *Simulation) Run(ctx context.Context) error {
	var lc net.ListenConfig
	apiLn, err := lc.Listen(ctx, "tcp", s.cfg.apiAddr)
	if err != nil {
		return fmt.Errorf("listen for the stripe api: %w", err)
	}
	webLn, err := lc.Listen(ctx, "tcp", s.cfg.webAddr)
	if err != nil {
		_ = apiLn.Close()
		return fmt.Errorf("listen for the stripe pages: %w", err)
	}
	apiServer := &http.Server{Handler: s.apiHandler(), ReadHeaderTimeout: 10 * time.Second}
	webServer := &http.Server{Handler: s.webHandler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			cancel()
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { serve(apiServer, apiLn) })
	wg.Go(func() { serve(webServer, webLn) })
	wg.Go(func() { s.hooks.run(ctx) })
	wg.Go(func() { s.sweepSessions(ctx) })
	s.log.Info("stripe simulation listening", "api", apiLn.Addr().String(), "pages", webLn.Addr().String(), "pages_url", s.cfg.webURL)

	<-ctx.Done()
	shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stop()
	shutdownErr := errors.Join(apiServer.Shutdown(shutdownCtx), webServer.Shutdown(shutdownCtx))
	wg.Wait()
	close(errs)
	for err := range errs {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	return shutdownErr
}

func (s *Simulation) sweepSessions(ctx context.Context) {
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := s.mutate(requestInfo{}, func(t *tx) (any, error) {
				t.expireDueSessions()
				return nil, nil
			})
			if err != nil {
				s.log.Error("expire checkout sessions", "error", err)
			}
		}
	}
}

func (s *Simulation) mutate(info requestInfo, fn func(t *tx) (any, error)) (json.RawMessage, error) {
	var out json.RawMessage
	var deliveries []delivery
	err := s.store.Update(func(a *account) error {
		t := &tx{acct: a, cfg: &s.cfg, now: s.now(), req: info}
		obj, err := fn(t)
		if err != nil {
			return err
		}
		if obj != nil {
			if out, err = renderExpanded(a, obj, info.expand); err != nil {
				return err
			}
		}
		deliveries = t.deliveries
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.hooks.enqueue(deliveries...)
	return out, nil
}

func (s *Simulation) read(expand []string, fn func(a *account) (any, error)) (json.RawMessage, error) {
	var out json.RawMessage
	var err error
	s.store.Read(func(a *account) {
		var obj any
		if obj, err = fn(a); err == nil {
			out, err = renderExpanded(a, obj, expand)
		}
	})
	return out, err
}

func (s *Simulation) deliveryTarget(d delivery) (deliveryTarget, bool) {
	var target deliveryTarget
	var ok bool
	s.store.Read(func(a *account) {
		e := find(a.Events, d.eventID)
		ep := find(a.WebhookEndpoints, d.endpointID)
		if e == nil || ep == nil || !ep.receives(e.Type) {
			return
		}
		payload := *e
		if ep.APIVersion != "" {
			payload.APIVersion = string(ep.APIVersion)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		target, ok = deliveryTarget{url: ep.URL, secret: ep.Secret, body: body}, true
	})
	return target, ok
}

func (s *Simulation) markDelivered(d delivery) {
	err := s.store.Update(func(a *account) error {
		if e := find(a.Events, d.eventID); e != nil && e.PendingWebhooks > 0 {
			e.PendingWebhooks--
		}
		return nil
	})
	if err != nil {
		s.log.Error("record webhook delivery", "event", d.eventID, "error", err)
	}
}

type inflightKeys struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (k *inflightKeys) acquire(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys[key] {
		return false
	}
	k.keys[key] = true
	return true
}

func (k *inflightKeys) release(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.keys, key)
}
