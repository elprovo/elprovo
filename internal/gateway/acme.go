package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// ACMEConfig says how the gateway gets TLS certificates through ACME.
type ACMEConfig struct {
	// DirectoryURL is the ACME server's directory URL.
	DirectoryURL string
	// Email is the contact address of the ACME account. It may be empty.
	Email string
	// CacheDir holds the ACME account key and the certificates.
	CacheDir string
	// DirectoryCAFile, when set, names a PEM file of CA certificates that
	// the gateway trusts for the ACME server's own HTTPS, in addition to the
	// system roots. Test servers such as Pebble need it.
	DirectoryCAFile string
	// ExtraHosts get certificates in addition to the routed stagelet hosts.
	ExtraHosts []string
}

// NewCertManager returns a manager that gets one certificate for each host
// name in g's route table and in cfg.ExtraHosts. It uses the TLS-ALPN-01
// challenge, and the HTTP-01 challenge once the caller serves its
// HTTPHandler. It refuses to request a certificate for any other host name.
// It creates cfg.CacheDir and makes it accessible to the owner only.
func NewCertManager(g *Gateway, cfg ACMEConfig) (*autocert.Manager, error) {
	if cfg.DirectoryURL == "" || cfg.CacheDir == "" {
		return nil, errors.New("ACME needs a directory URL and a cache directory")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ACME cache directory: %w", err)
	}
	if err := os.Chmod(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict ACME cache directory: %w", err)
	}
	client := &acme.Client{DirectoryURL: cfg.DirectoryURL}
	if cfg.DirectoryCAFile != "" {
		httpClient, err := httpClientTrusting(cfg.DirectoryCAFile)
		if err != nil {
			return nil, err
		}
		client.HTTPClient = httpClient
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.CacheDir),
		HostPolicy: g.certHostPolicy(cfg.ExtraHosts),
		Client:     client,
		Email:      cfg.Email,
	}, nil
}

func (g *Gateway) certHostPolicy(extraHosts []string) autocert.HostPolicy {
	extra := make(map[string]bool, len(extraHosts))
	for _, h := range extraHosts {
		extra[hostName(h)] = true
	}
	return func(_ context.Context, host string) error {
		if _, ok := g.route(host); ok || extra[hostName(host)] {
			return nil
		}
		return fmt.Errorf("no route for host %q", host)
	}
}

func httpClientTrusting(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ACME directory CA file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA certificates: %w", err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ACME directory CA file %s holds no PEM certificates", caFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	return &http.Client{Transport: transport}, nil
}
