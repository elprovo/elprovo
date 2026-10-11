package gateway

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/acme/autocert"
)

func newTestCertManager(t *testing.T, gw *Gateway, directoryURL string, extraHosts ...string) *autocert.Manager {
	t.Helper()
	m, err := NewCertManager(gw, ACMEConfig{DirectoryURL: directoryURL, CacheDir: t.TempDir(), ExtraHosts: extraHosts})
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	return m
}

func TestCertificatesAreOnlyForRoutedAndExtraHosts(t *testing.T) {
	f := newFixture(t)
	m := newTestCertManager(t, f.gw, "https://acme.invalid/directory", "Gateway.Example.com")
	tests := []struct {
		host    string
		allowed bool
	}{
		{host: host, allowed: true},
		{host: "S1.Stagelets.Localhost", allowed: true},
		{host: host + ":80", allowed: true},
		{host: "gateway.example.com", allowed: true},
		{host: "s2.stagelets.localhost", allowed: false},
		{host: "stagelets.localhost", allowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			err := m.HostPolicy(context.Background(), tt.host)
			if allowed := err == nil; allowed != tt.allowed {
				t.Fatalf("HostPolicy(%q) = %v, want allowed %t", tt.host, err, tt.allowed)
			}
		})
	}
}

func TestRemovedRouteGetsNoCertificate(t *testing.T) {
	f := newFixture(t)
	m := newTestCertManager(t, f.gw, "https://acme.invalid/directory")

	f.gw.SetRoutes(nil)

	if err := m.HostPolicy(context.Background(), host); err == nil {
		t.Fatalf("HostPolicy(%q) allowed a host that has no route", host)
	}
}

func TestHandshakeForUnroutedHostNeverReachesACMEServer(t *testing.T) {
	var requests atomic.Int32
	acmeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(acmeServer.Close)
	f := newFixture(t)
	m := newTestCertManager(t, f.gw, acmeServer.URL)

	_, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "attacker.example.com"})

	if err == nil {
		t.Fatal("GetCertificate returned a certificate for an unrouted host")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("ACME server got %d requests, want none", n)
	}
}

func TestHTTPListenerRedirectsToHTTPS(t *testing.T) {
	f := newFixture(t)
	handler := newTestCertManager(t, f.gw, "https://acme.invalid/directory").HTTPHandler(nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+"/orders?page=2", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://"+host+"/orders?page=2" {
		t.Fatalf("response = %d to %q, want 302 to the same URL over HTTPS", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHTTPListenerAnswersChallengesOnlyForRoutedHosts(t *testing.T) {
	f := newFixture(t)
	handler := newTestCertManager(t, f.gw, "https://acme.invalid/directory").HTTPHandler(nil)
	tests := []struct {
		name string
		host string
		want int
	}{
		{name: "unrouted host is refused", host: "attacker.example.com", want: http.StatusForbidden},
		{name: "routed host without a pending challenge", host: host, want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+tt.host+"/.well-known/acme-challenge/token", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestCertManagerRestrictsCacheDirectoryToOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create cache directory: %v", err)
	}

	if _, err := NewCertManager(newFixture(t).gw, ACMEConfig{DirectoryURL: "https://acme.invalid/directory", CacheDir: dir}); err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cache directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("cache directory permissions = %o, want 700", perm)
	}
}

func TestCertManagerRejectsDirectoryCAFileWithoutCertificates(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	_, err := NewCertManager(newFixture(t).gw, ACMEConfig{DirectoryURL: "https://acme.invalid/directory", CacheDir: t.TempDir(), DirectoryCAFile: caFile})

	if err == nil {
		t.Fatal("NewCertManager accepted a CA file without certificates")
	}
}
