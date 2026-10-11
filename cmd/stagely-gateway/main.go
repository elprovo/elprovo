// Command stagely-gateway serves stagelet traffic for one environment.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stagely-dev/stagely/internal/controlplane"
	"github.com/stagely-dev/stagely/internal/gateway"
	"github.com/stagely-dev/stagely/protocol"
	"golang.org/x/crypto/acme"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: stagely-gateway run [flags]")
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		slog.Error("stagely-gateway stopped", "error", err)
		os.Exit(1)
	}
}

type options struct {
	controlPlane    string
	dataDir         string
	enrollmentToken string
	listen          string
	tlsCert         string
	tlsKey          string
	acme            bool
	acmeDirectory   string
	acmeDirectoryCA string
	acmeEmail       string
	acmeExtraHosts  []string
	httpListen      string
}

func parseFlags(args []string) (options, error) {
	var o options
	var extraHosts string
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.StringVar(&o.controlPlane, "control-plane", "ws://localhost:8080/v1/connect", "control plane session URL")
	fs.StringVar(&o.dataDir, "data-dir", defaultDataDir("gateway"), "directory for the gateway key and, with -acme, the certificate cache")
	fs.StringVar(&o.enrollmentToken, "enrollment-token", os.Getenv("STAGELY_ENROLLMENT_TOKEN"), "one-time enrollment token, needed on first start; defaults to STAGELY_ENROLLMENT_TOKEN")
	fs.StringVar(&o.listen, "listen", ":8443", "HTTPS listen address")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "TLS certificate file for the stagelet host names")
	fs.StringVar(&o.tlsKey, "tls-key", "", "TLS key file")
	fs.BoolVar(&o.acme, "acme", false, "get a certificate for each stagelet host name through ACME, instead of -tls-cert and -tls-key")
	fs.StringVar(&o.acmeDirectory, "acme-directory", acme.LetsEncryptURL, "ACME directory URL")
	fs.StringVar(&o.acmeDirectoryCA, "acme-directory-ca", "", "PEM file of extra CA certificates to trust for the ACME directory's HTTPS, for test servers such as Pebble")
	fs.StringVar(&o.acmeEmail, "acme-email", "", "contact email for the ACME account")
	fs.StringVar(&extraHosts, "acme-extra-hosts", "", "comma-separated host names that get certificates in addition to the stagelet host names")
	fs.StringVar(&o.httpListen, "http-listen", ":80", "HTTP listen address with -acme: answers HTTP-01 challenges and redirects other requests to HTTPS")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	o.acmeExtraHosts = splitList(extraHosts)
	switch {
	case o.acme && (o.tlsCert != "" || o.tlsKey != ""):
		return options{}, errors.New("use either -acme or -tls-cert and -tls-key, not both")
	case !o.acme && (o.tlsCert == "" || o.tlsKey == ""):
		return options{}, errors.New("-tls-cert and -tls-key are required unless -acme is set")
	}
	return o, nil
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	link := &gateway.Link{}
	gw := gateway.New(link, 50*time.Second, time.Now, log)
	tlsConfig, challenges, err := certificates(gw, opts)
	if err != nil {
		return err
	}
	go gw.RunConfirmLoop(ctx, 20*time.Second)

	cfg := controlplane.Config{
		URL:             opts.controlPlane,
		Role:            protocol.RoleGateway,
		KeyPath:         filepath.Join(opts.dataDir, "gateway.key"),
		EnrollmentToken: opts.enrollmentToken,
	}
	sessionErr := make(chan error, 1)
	go func() { sessionErr <- controlplane.Run(ctx, cfg, log, link.Serve(gw)) }()

	httpsServer := &http.Server{Addr: opts.listen, Handler: gw, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{httpsServer}
	serveErr := make(chan error, 2)
	go func() { serveErr <- fmt.Errorf("serve https: %w", httpsServer.ListenAndServeTLS("", "")) }()
	log.Info("stagely-gateway listening", "addr", opts.listen)
	if challenges != nil {
		httpServer := &http.Server{Addr: opts.httpListen, Handler: challenges, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, httpServer)
		go func() { serveErr <- fmt.Errorf("serve http: %w", httpServer.ListenAndServe()) }()
		log.Info("stagely-gateway answering ACME HTTP-01 challenges", "addr", opts.httpListen)
	}

	var runErr error
	select {
	case runErr = <-serveErr:
	case runErr = <-sessionErr:
	case <-ctx.Done():
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := []error{runErr}
	for _, s := range servers {
		errs = append(errs, s.Shutdown(shutdownCtx))
	}
	return errors.Join(errs...)
}

func certificates(gw *gateway.Gateway, opts options) (*tls.Config, http.Handler, error) {
	if !opts.acme {
		cert, err := tls.LoadX509KeyPair(opts.tlsCert, opts.tlsKey)
		if err != nil {
			return nil, nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil, nil
	}
	m, err := gateway.NewCertManager(gw, gateway.ACMEConfig{
		DirectoryURL:    opts.acmeDirectory,
		Email:           opts.acmeEmail,
		CacheDir:        filepath.Join(opts.dataDir, "acme"),
		DirectoryCAFile: opts.acmeDirectoryCA,
		ExtraHosts:      opts.acmeExtraHosts,
	})
	if err != nil {
		return nil, nil, err
	}
	return m.TLSConfig(), m.HTTPHandler(nil), nil
}

func splitList(s string) []string {
	var items []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func defaultDataDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".stagely", name)
	}
	return filepath.Join(home, ".stagely", name)
}
