// Package github simulates GitHub for Stagely: a web host like github.com
// with sign-in, GitHub App authorization and installation, a small
// repository UI and git smart HTTP; an API host like api.github.com with
// the REST endpoints Stagely calls; and signed webhook deliveries. An admin
// API under /_sim/ on the web host sets up users, repositories and apps.
package github

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/state"
)

// Config configures the simulation.
type Config struct {
	// WebAddr and APIAddr are the listen addresses of the web host and the
	// API host.
	WebAddr string
	APIAddr string
	// WebURL and APIURL are the public base URLs of the two hosts. Links,
	// redirects and webhook payloads use them.
	WebURL string
	APIURL string
	// DataDir keeps the state file, the bare repositories and the app keys.
	DataDir string
	// RateLimit is the hourly request budget of each authenticated caller.
	// Zero means GitHub's 5000.
	RateLimit int
	// DefaultUser, when set, is a user created at startup.
	DefaultUser string
	// DefaultApp, when its Name is set, is a GitHub App created at startup
	// and owned by DefaultUser.
	DefaultApp DefaultApp
}

// DefaultApp describes the GitHub App that the simulation creates at
// startup. A restart updates its URLs and keeps its id and keys.
type DefaultApp struct {
	Name          string
	WebhookURL    string
	WebhookSecret string
	CallbackURLs  []string
	SetupURL      string
}

// Sim is the GitHub simulation.
type Sim struct {
	cfg        Config
	log        *slog.Logger
	store      *state.Store[world]
	hooks      *dispatcher
	limits     *rateLimiter
	archiveKey []byte
	repoLocks  sync.Map
	createMu   sync.Mutex
	pages      *template.Template
}

// Flags registers the GitHub simulation's flags on fs. The returned
// function builds the simulation after the flags are parsed; it returns nil
// when -github is off.
func Flags(fs *flag.FlagSet) func(dataDir string, log *slog.Logger) (*Sim, error) {
	enabled := fs.Bool("github", false, "run the GitHub simulation")
	var cfg Config
	fs.StringVar(&cfg.WebAddr, "github-web-addr", ":9100", "listen address of the simulated github.com")
	fs.StringVar(&cfg.APIAddr, "github-api-addr", ":9101", "listen address of the simulated api.github.com")
	fs.StringVar(&cfg.WebURL, "github-web-url", "http://localhost:9100", "public base URL of the simulated github.com")
	fs.StringVar(&cfg.APIURL, "github-api-url", "http://localhost:9101", "public base URL of the simulated api.github.com")
	fs.StringVar(&cfg.DefaultUser, "github-default-user", "", "login of a user to create at startup")
	fs.StringVar(&cfg.DefaultApp.Name, "github-default-app-name", "", "name of a GitHub App to create at startup, owned by -github-default-user")
	fs.StringVar(&cfg.DefaultApp.WebhookURL, "github-default-app-webhook-url", "", "webhook URL of the default app")
	fs.StringVar(&cfg.DefaultApp.WebhookSecret, "github-default-app-webhook-secret", "", "webhook secret of the default app; generated when empty")
	callbacks := fs.String("github-default-app-callback-urls", "", "comma-separated user authorization callback URLs of the default app")
	fs.StringVar(&cfg.DefaultApp.SetupURL, "github-default-app-setup-url", "", "setup URL of the default app, opened after installation")
	return func(dataDir string, log *slog.Logger) (*Sim, error) {
		if !*enabled {
			return nil, nil
		}
		cfg.DataDir = filepath.Join(dataDir, "github")
		cfg.DefaultApp.CallbackURLs = splitList(*callbacks)
		return New(cfg, log.With("simulation", "github"))
	}
}

// New opens the simulation's state under cfg.DataDir, starts webhook
// delivery and creates the default user and app. Call Close, or Run, to
// stop delivery.
func New(cfg Config, log *slog.Logger) (*Sim, error) {
	if cfg.RateLimit == 0 {
		cfg.RateLimit = 5000
	}
	cfg.WebURL = strings.TrimSuffix(cfg.WebURL, "/")
	cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create github data directory: %w", err)
	}
	store, err := state.Open(filepath.Join(cfg.DataDir, "state.json"), world{})
	if err != nil {
		return nil, err
	}
	pages, err := parsePages()
	if err != nil {
		return nil, fmt.Errorf("parse page templates: %w", err)
	}
	s := &Sim{
		cfg:        cfg,
		log:        log,
		store:      store,
		limits:     newRateLimiter(),
		archiveKey: []byte(rand.Text()),
		pages:      pages,
	}
	s.hooks = newDispatcher(store, log)
	if err := s.seed(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Run serves the web host and the API host until ctx ends, then stops
// webhook delivery.
func (s *Sim) Run(ctx context.Context) error {
	defer s.Close()
	servers := []*http.Server{
		{Addr: s.cfg.WebAddr, Handler: s.webHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: s.cfg.APIAddr, Handler: s.apiHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	var listeners []net.Listener
	var lc net.ListenConfig
	for _, srv := range servers {
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			closeAll(listeners)
			return fmt.Errorf("listen on %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
	}
	s.log.Info("github simulation listening", "web", s.cfg.WebURL, "api", s.cfg.APIURL)

	errs := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { errs <- srv.Serve(listeners[i]) }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops webhook delivery and waits for deliveries in flight.
func (s *Sim) Close() { s.hooks.close() }

func closeAll(listeners []net.Listener) {
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

type tx struct {
	view
	deliveries []int64
}

func (s *Sim) view(w *world) view { return view{w: w, web: s.cfg.WebURL, api: s.cfg.APIURL} }

func (s *Sim) read(fn func(v view)) {
	s.store.Read(func(w *world) { fn(s.view(w)) })
}

func (s *Sim) update(fn func(t *tx) error) error {
	var deliveries []int64
	err := s.store.Update(func(w *world) error {
		t := &tx{view: s.view(w)}
		if err := fn(t); err != nil {
			return err
		}
		deliveries = t.deliveries
		return nil
	})
	if err != nil {
		return err
	}
	s.hooks.enqueue(deliveries...)
	return nil
}

func (s *Sim) reposDir() string { return filepath.Join(s.cfg.DataDir, "repos") }

func (s *Sim) repoDir(owner, name string) string {
	return filepath.Join(s.reposDir(), owner, name+".git")
}

func (s *Sim) lockRepo(id int64) func() {
	m, _ := s.repoLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
