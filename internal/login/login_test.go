package login

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuthServer is a minimal OAuth server with PKCE, like the control
// plane's.
type fakeAuthServer struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	challenge string
	redirect  string
	refreshes int
	// revoked holds the token and client id of each revocation.
	revoked      []string
	revokeStatus int
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	f := &fakeAuthServer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.redirect = req.RedirectURIs[0]
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "cli-1"})
	})
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("resource") != f.srv.URL+APIPath || q.Get("code_challenge_method") != "S256" || q.Has("scope") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		back := q.Get("redirect_uri") + "?" + url.Values{"code": {"code-1"}, "state": {q.Get("state")}, "iss": {f.srv.URL}}.Encode()
		http.Redirect(w, r, back, http.StatusFound)
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("redirect_uri") != f.redirect {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":900,"token_type":"Bearer"}`)
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			f.refreshes++
			_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":900,"token_type":"Bearer"}`)
		}
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked = append(f.revoked, r.Form.Get("token")+" "+r.Form.Get("client_id"))
		if f.revokeStatus != 0 {
			w.WriteHeader(f.revokeStatus)
		}
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"bad token"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"user":{"login":"octocat"}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestLoginGetsTokensForTheAPI(t *testing.T) {
	f := newFakeAuthServer(t)
	var out strings.Builder
	flow := Flow{Server: f.srv.URL + "/", HTTP: f.srv.Client(), Out: &out, OpenBrowser: func(u string) error {
		go func() {
			resp, err := http.Get(u) //nolint:noctx // the test plays the browser
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	creds, err := flow.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if creds.Server != f.srv.URL || creds.ClientID != "cli-1" || creds.AccessToken != "access-1" || creds.RefreshToken != "refresh-1" {
		t.Fatalf("credentials = %+v", creds)
	}
	if !strings.Contains(out.String(), "/oauth/authorize?") {
		t.Fatalf("output = %q, want the authorization URL", out.String())
	}
}

func TestSessionRefreshesAnExpiredTokenAndSavesIt(t *testing.T) {
	f := newFakeAuthServer(t)
	store := Store{Path: filepath.Join(t.TempDir(), "cli", "credentials.json")}
	if err := store.Save(Credentials{Server: f.srv.URL, ClientID: "cli-1", AccessToken: "old", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(store, f.srv.Client())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var me struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := s.Do(context.Background(), http.MethodGet, "/me", nil, &me); err != nil || me.User.Login != "octocat" {
		t.Fatalf("Do = %+v, %v; want octocat after a refresh", me, err)
	}
	saved, _ := store.Load()
	if saved.AccessToken != "access-2" || saved.RefreshToken != "refresh-2" || f.refreshes != 1 {
		t.Fatalf("saved = %+v after %d refreshes, want the rotated tokens", saved, f.refreshes)
	}
	if info, _ := os.Stat(store.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestEndedGrantMeansSignInAgain(t *testing.T) {
	f := newFakeAuthServer(t)
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{Server: f.srv.URL, ClientID: "cli-1", RefreshToken: "revoked", ExpiresAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(store, f.srv.Client())

	if err := s.Do(context.Background(), http.MethodGet, "/me", nil, nil); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v, want ErrNotSignedIn", err)
	}
}

func TestNoCredentialsMeansNotSignedIn(t *testing.T) {
	if _, err := Open(Store{Path: filepath.Join(t.TempDir(), "missing.json")}, http.DefaultClient); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v, want ErrNotSignedIn", err)
	}
}

func TestPlainHTTPWorksOnlyForLocalhost(t *testing.T) {
	for server, ok := range map[string]bool{
		"http://localhost:8080": true, "http://127.0.0.1:8080": true, "https://stagely.example.com": true,
		"http://stagely.example.com": false, "ftp://stagely.example.com": false, "stagely.example.com": false,
	} {
		if err := checkServer(server); (err == nil) != ok {
			t.Fatalf("checkServer(%q) = %v, want allowed %v", server, err, ok)
		}
	}
}

func TestLogoutRevokesTheSignInAndDeletesTheCredentials(t *testing.T) {
	f := newFakeAuthServer(t)
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{Server: f.srv.URL, ClientID: "cli-1", RefreshToken: "refresh-1"}); err != nil {
		t.Fatal(err)
	}

	if err := Logout(context.Background(), store, f.srv.Client()); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := store.Load(); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Load after Logout: err = %v, want ErrNotSignedIn", err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != "refresh-1 cli-1" {
		t.Fatalf("revocations = %q, want the refresh token of cli-1", f.revoked)
	}
}

func TestLogoutDeletesTheCredentialsEvenWhenTheRevocationFails(t *testing.T) {
	f := newFakeAuthServer(t)
	f.revokeStatus = http.StatusServiceUnavailable
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{Server: f.srv.URL, ClientID: "cli-1", RefreshToken: "refresh-1"}); err != nil {
		t.Fatal(err)
	}

	err := Logout(context.Background(), store, f.srv.Client())

	if err == nil || !strings.Contains(err.Error(), "did not revoke") {
		t.Fatalf("Logout err = %v, want one that says the sign-in was not revoked", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Load after Logout: err = %v, want ErrNotSignedIn", err)
	}
}
