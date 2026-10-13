//go:build acme

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elprovo/elprovo/protocol"
	"golang.org/x/crypto/acme/autocert"
)

// The Pebble container resolves these host names to the Docker host and
// validates challenges there on the ports in testdata/pebble-config.json.
// Keep them in step with that file and the test-acme target in the Makefile.
const (
	pebbleDirectory    = "https://localhost:14000/dir"
	pebbleRootsURL     = "https://localhost:15000/roots/0"
	tlsALPNHost        = "tls-alpn.previews.example.com"
	httpHost           = "http.previews.example.com"
	pebbleTLSALPNPort  = "127.0.0.1:14001"
	pebbleHTTPPort     = "127.0.0.1:14002"
	pebbleCAFileEnvVar = "ELPROVO_PEBBLE_CA"
)

func TestACMEIssuesCertificateThroughTLSALPN01(t *testing.T) {
	caFile := pebbleCAFile(t)
	gw := gatewayRouting(tlsALPNHost)
	cacheDir := t.TempDir()
	m := newPebbleCertManager(t, gw, caFile, cacheDir)
	addr := serveTLS(t, pebbleTLSALPNPort, m)

	cert := handshake(t, addr, tlsALPNHost, pebbleIssuerRoots(t, caFile))

	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != tlsALPNHost {
		t.Fatalf("certificate names = %v, want [%s]", cert.DNSNames, tlsALPNHost)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, tlsALPNHost)); err != nil {
		t.Fatalf("certificate is not in the cache: %v", err)
	}
}

func TestACMEIssuesCertificateThroughHTTP01(t *testing.T) {
	caFile := pebbleCAFile(t)
	gw := gatewayRouting(httpHost)
	m := newPebbleCertManager(t, gw, caFile, t.TempDir())
	challengeHandler := m.HTTPHandler(nil)
	var challenges atomic.Int32
	serveHTTP(t, pebbleHTTPPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			challenges.Add(1)
		}
		challengeHandler.ServeHTTP(w, r)
	}))
	// Nothing listens on the TLS-ALPN-01 port, so only HTTP-01 can pass.
	addr := serveTLS(t, "127.0.0.1:0", m)

	cert := handshake(t, addr, httpHost, pebbleIssuerRoots(t, caFile))

	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != httpHost {
		t.Fatalf("certificate names = %v, want [%s]", cert.DNSNames, httpHost)
	}
	if challenges.Load() == 0 {
		t.Fatal("Pebble never requested the HTTP-01 challenge")
	}
}

func pebbleCAFile(t *testing.T) string {
	t.Helper()
	caFile := os.Getenv(pebbleCAFileEnvVar)
	if caFile == "" {
		t.Fatalf("%s is not set; run make test-acme", pebbleCAFileEnvVar)
	}
	return caFile
}

func gatewayRouting(hostName string) *Gateway {
	gw := New(&fakeControlPlane{}, 50*time.Second, time.Now, slog.New(slog.DiscardHandler))
	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: hostName, Upstream: "127.0.0.1:1"}})
	return gw
}

func newPebbleCertManager(t *testing.T, gw *Gateway, caFile, cacheDir string) *autocert.Manager {
	t.Helper()
	m, err := NewCertManager(gw, ACMEConfig{DirectoryURL: pebbleDirectory, CacheDir: cacheDir, DirectoryCAFile: caFile, Email: "ops@example.com"})
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	return m
}

func serveTLS(t *testing.T, addr string, m *autocert.Manager) string {
	t.Helper()
	ln := listen(t, addr)
	serve(t, tls.NewListener(ln, m.TLSConfig()), http.NotFoundHandler())
	return ln.Addr().String()
}

func serveHTTP(t *testing.T, addr string, handler http.Handler) {
	t.Helper()
	serve(t, listen(t, addr), handler)
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	return ln
}

func serve(t *testing.T, ln net.Listener, handler http.Handler) {
	t.Helper()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func handshake(t *testing.T, addr, serverName string, roots *x509.CertPool) *x509.Certificate {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{ServerName: serverName, RootCAs: roots}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("TLS handshake for %s: %v", serverName, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
}

func pebbleIssuerRoots(t *testing.T, caFile string) *x509.CertPool {
	t.Helper()
	client, err := httpClientTrusting(caFile)
	if err != nil {
		t.Fatalf("Pebble HTTP client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pebbleRootsURL, nil)
	if err != nil {
		t.Fatalf("build Pebble roots request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get Pebble issuer root: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get Pebble issuer root: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read Pebble issuer root: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(body) {
		t.Fatal("Pebble issuer root holds no PEM certificate")
	}
	return roots
}
