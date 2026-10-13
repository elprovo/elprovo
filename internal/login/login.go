// Package login signs an operator in to a Elprovo control plane through its
// OAuth server, keeps the tokens on disk, and gives an HTTP client that
// sends them. The CLI registers itself as a public client, uses PKCE and a
// loopback redirect, and asks for a token for the control plane's JSON API.
package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// APIPath is the control plane's JSON API, the resource the CLI's tokens
// are for.
const APIPath = "/api/v1"

// ErrNotSignedIn means there are no stored credentials, or the grant behind
// them has ended.
var ErrNotSignedIn = errors.New("not signed in to Elprovo; run `elprovo login`")

// Credentials are what the CLI keeps between runs.
type Credentials struct {
	Server       string    `json:"server"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Flow is one sign-in. OpenBrowser shows the authorization page; when it
// fails, the URL is still printed to Out.
type Flow struct {
	Server      string
	HTTP        *http.Client
	OpenBrowser func(url string) error
	Out         io.Writer
}

// Run registers the CLI with the control plane, sends the operator to the
// consent page, waits for the redirect, and exchanges the code for tokens.
func (f Flow) Run(ctx context.Context) (Credentials, error) {
	server := strings.TrimRight(f.Server, "/")
	if err := checkServer(server); err != nil {
		return Credentials{}, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Credentials{}, fmt.Errorf("listen for the sign-in redirect: %w", err)
	}
	defer func() { _ = listener.Close() }()
	redirectURI := "http://" + listener.Addr().String() + "/callback"

	clientID, err := f.register(ctx, server, redirectURI)
	if err != nil {
		return Credentials{}, err
	}
	verifier, state := randomString(), randomString()
	authorize := server + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}, "state": {state},
		"resource": {server + APIPath},
	}.Encode()
	_, _ = fmt.Fprintf(f.Out, "Opening your browser to sign in. If it does not open, visit:\n\n  %s\n\n", authorize)
	if f.OpenBrowser != nil {
		_ = f.OpenBrowser(authorize)
	}

	code, err := waitForCode(ctx, listener, state, server)
	if err != nil {
		return Credentials{}, err
	}
	tokens, err := f.token(ctx, server, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}, "resource": {server + APIPath},
	})
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Server: server, ClientID: clientID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: tokens.expiresAt()}, nil
}

func (f Flow) register(ctx context.Context, server, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name": "elprovo CLI", "redirect_uris": []string{redirectURI},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/oauth/register", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := doJSON(f.HTTP, req, &out); err != nil {
		return "", fmt.Errorf("register the CLI with %s: %w", server, err)
	}
	return out.ClientID, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (t tokenResponse) expiresAt() time.Time {
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

func (f Flow) token(ctx context.Context, server string, form url.Values) (tokenResponse, error) {
	return requestToken(ctx, f.HTTP, server, form)
}

func requestToken(ctx context.Context, client *http.Client, server string, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out tokenResponse
	if err := doJSON(client, req, &out); err != nil {
		return tokenResponse{}, fmt.Errorf("get tokens: %w", err)
	}
	if out.AccessToken == "" {
		return tokenResponse{}, errors.New("get tokens: the server returned no access token")
	}
	return out, nil
}

// waitForCode serves the loopback redirect until it brings a code for
// state, or ctx ends.
func waitForCode(ctx context.Context, listener net.Listener, state, issuer string) (string, error) {
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	var once sync.Once
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			http.Error(w, "This sign-in link does not belong to this elprovo login.", http.StatusBadRequest)
			return
		case strings.TrimRight(q.Get("iss"), "/") != issuer:
			res.err = fmt.Errorf("the redirect came from issuer %q, not %q", q.Get("iss"), issuer)
		case q.Get("error") != "":
			res.err = fmt.Errorf("sign-in failed: %s", q.Get("error"))
		case q.Get("code") == "":
			res.err = errors.New("sign-in failed: the redirect carried no code")
		default:
			res.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		message := "Signed in. You can close this tab and go back to the terminal."
		if res.err != nil {
			message = "Sign-in failed: " + res.err.Error()
		}
		_, _ = fmt.Fprintf(w, "<!doctype html><title>Elprovo</title><p>%s</p>", html.EscapeString(message))
		once.Do(func() { results <- res })
	})}
	go func() { _ = srv.Serve(listener) }()
	defer func() { _ = srv.Close() }()
	select {
	case res := <-results:
		return res.code, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Store keeps credentials in one file that only the user can read.
type Store struct {
	Path string
}

// Load returns the stored credentials, or ErrNotSignedIn.
func (s Store) Load() (Credentials, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credentials{}, ErrNotSignedIn
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("read credentials %s: %w", s.Path, err)
	}
	return c, nil
}

// Save replaces the stored credentials. It writes a new file that only the
// user can read and renames it over the old one.
func (s Store) Save(c Credentials) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	f, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr, os.Rename(f.Name(), s.Path)); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

// checkServer refuses plain HTTP to anything but the operator's own
// machine, so codes and tokens never cross a network in the clear.
func checkServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("server %q must be an http or https URL", server)
	}
	host := u.Hostname()
	if u.Scheme == "http" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("server %q must use https; plain http works only for localhost", server)
	}
	return nil
}

// Logout revokes the stored sign-in on the control plane, so its tokens stop
// working everywhere, and deletes the stored credentials. When the control
// plane does not confirm the revocation, it still deletes them and returns
// an error that says the sign-in works until it expires.
func Logout(ctx context.Context, store Store, client *http.Client) error {
	creds, err := store.Load()
	if errors.Is(err, ErrNotSignedIn) {
		return nil
	}
	if err != nil {
		return err
	}
	revokeErr := revoke(ctx, client, creds)
	if err := store.Delete(); err != nil {
		return err
	}
	if revokeErr != nil {
		return fmt.Errorf("deleted the credentials on this machine, but the control plane did not revoke the sign-in, so its tokens work for up to an hour: %w", revokeErr)
	}
	return nil
}

// revoke ends the grant behind the credentials' refresh token (RFC 7009).
func revoke(ctx context.Context, client *http.Client, c Credentials) error {
	form := url.Values{"token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/oauth/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: status %d", resp.StatusCode)
	}
	return nil
}

// Delete removes the stored credentials. Deleting nothing succeeds.
func (s Store) Delete() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete credentials: %w", err)
	}
	return nil
}

// Session sends API requests with the stored access token, refreshing it
// shortly before it expires and saving the rotated tokens.
type Session struct {
	Store Store
	HTTP  *http.Client

	mu    sync.Mutex
	creds Credentials
}

// Open loads the stored credentials into a session.
func Open(store Store, client *http.Client) (*Session, error) {
	creds, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Session{Store: store, HTTP: client, creds: creds}, nil
}

// Server returns the control plane the session signed in to.
func (s *Session) Server() string {
	return s.creds.Server
}

// Do sends a request to path under the API with a JSON body (or none) and
// decodes the JSON answer into out, when out is not nil.
func (s *Session) Do(ctx context.Context, method, path string, body, out any) error {
	token, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.creds.Server+APIPath+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doJSON(s.HTTP, req, out)
}

func (s *Session) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Until(s.creds.ExpiresAt) > time.Minute {
		return s.creds.AccessToken, nil
	}
	tokens, err := requestToken(ctx, s.HTTP, s.creds.Server, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {s.creds.ClientID}, "refresh_token": {s.creds.RefreshToken},
		"resource": {s.creds.Server + APIPath},
	})
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
		return "", ErrNotSignedIn
	}
	if err != nil {
		return "", err
	}
	s.creds.AccessToken, s.creds.RefreshToken, s.creds.ExpiresAt = tokens.AccessToken, tokens.RefreshToken, tokens.expiresAt()
	if err := s.Store.Save(s.creds); err != nil {
		return "", err
	}
	return s.creds.AccessToken, nil
}

// APIError is an error answer from the control plane.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("the control plane answered %d", e.Status)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func parseError(status int, data []byte) error {
	var api struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &api) == nil && api.Error.Code != "" {
		return &APIError{Status: status, Code: api.Error.Code, Message: api.Error.Message}
	}
	var oauthErr struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(data, &oauthErr) == nil && oauthErr.Error != "" {
		return &APIError{Status: status, Code: oauthErr.Error, Message: oauthErr.Description}
	}
	return &APIError{Status: status}
}

func randomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
