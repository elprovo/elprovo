package github

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testSim struct {
	t     *testing.T
	cfg   Config
	sim   *Sim
	web   *httptest.Server
	api   *httptest.Server
	hooks *hookReceiver
}

func newTestSim(t *testing.T, configure ...func(*Config)) *testSim {
	t.Helper()
	ts := &testSim{t: t, hooks: newHookReceiver(t)}
	ts.web = httptest.NewUnstartedServer(nil)
	ts.api = httptest.NewUnstartedServer(nil)
	ts.cfg = Config{
		WebURL:  "http://" + ts.web.Listener.Addr().String(),
		APIURL:  "http://" + ts.api.Listener.Addr().String(),
		DataDir: t.TempDir(),
	}
	for _, c := range configure {
		c(&ts.cfg)
	}
	ts.start()
	return ts
}

func (ts *testSim) start() {
	ts.t.Helper()
	s, err := New(ts.cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		ts.t.Fatalf("New: %v", err)
	}
	ts.t.Cleanup(s.Close)
	ts.sim = s
	ts.web.Config.Handler = s.webHandler()
	ts.api.Config.Handler = s.apiHandler()
	ts.web.Start()
	ts.api.Start()
	ts.t.Cleanup(ts.web.Close)
	ts.t.Cleanup(ts.api.Close)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func noRedirects(jar http.CookieJar) *http.Client {
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func do(t *testing.T, client *http.Client, method, rawURL string, body io.Reader, header http.Header) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return bytes.NewReader(raw)
}

func (ts *testSim) admin(method, path string, body any, want int) response {
	ts.t.Helper()
	r := do(ts.t, http.DefaultClient, method, ts.web.URL+path, jsonBody(ts.t, body), nil)
	if r.status != want {
		ts.t.Fatalf("%s %s = %d %s, want %d", method, path, r.status, r.body, want)
	}
	return r
}

func (ts *testSim) call(method, path, token string, body any) response {
	ts.t.Helper()
	header := http.Header{"Accept": {"application/vnd.github+json"}}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	return do(ts.t, noRedirects(nil), method, ts.api.URL+path, jsonBody(ts.t, body), header)
}

func (ts *testSim) createUser(login string) {
	ts.t.Helper()
	ts.admin(http.MethodPost, "/_sim/users", map[string]string{"login": login, "name": login, "email": login + "@example.com"}, http.StatusCreated)
}

func (ts *testSim) createOrg(login, adminLogin string) {
	ts.t.Helper()
	ts.admin(http.MethodPost, "/_sim/orgs", map[string]string{"login": login}, http.StatusCreated)
	ts.admin(http.MethodPut, "/_sim/orgs/"+login+"/members/"+adminLogin, map[string]string{"role": "admin"}, http.StatusNoContent)
}

func (ts *testSim) createRepo(owner, name string, files map[string]string) repoJSON {
	ts.t.Helper()
	body := map[string]any{"owner": owner, "name": name, "private": true}
	if files != nil {
		body["source_dir"] = writeFiles(ts.t, files)
	}
	var out repoJSON
	ts.admin(http.MethodPost, "/_sim/repos", body, http.StatusCreated).decode(ts.t, &out)
	return out
}

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func (ts *testSim) createApp(owner string, extra map[string]any) appCredentials {
	ts.t.Helper()
	body := map[string]any{"name": "Elprovo Dev", "owner": owner, "webhook_url": ts.hooks.srv.URL, "webhook_secret": "hook-secret"}
	for k, v := range extra {
		body[k] = v
	}
	var out appCredentials
	ts.admin(http.MethodPost, "/_sim/apps", body, http.StatusCreated).decode(ts.t, &out)
	return out
}

func (ts *testSim) install(slug, account string, repos ...string) installationJSON {
	ts.t.Helper()
	body := map[string]any{"account": account, "repository_selection": "all"}
	if len(repos) > 0 {
		body["repository_selection"] = "selected"
		body["repositories"] = repos
	}
	r := do(ts.t, http.DefaultClient, http.MethodPost, ts.web.URL+"/_sim/apps/"+slug+"/installations", jsonBody(ts.t, body), nil)
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		ts.t.Fatalf("install = %d %s", r.status, r.body)
	}
	var out installationJSON
	r.decode(ts.t, &out)
	return out
}

func (ts *testSim) installationToken(a appCredentials, installationID int64, body any) string {
	ts.t.Helper()
	r := ts.call(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), appJWT(ts.t, a), body)
	if r.status != http.StatusCreated {
		ts.t.Fatalf("create installation token = %d %s", r.status, r.body)
	}
	var out installationTokenJSON
	r.decode(ts.t, &out)
	return out.Token
}

func (ts *testSim) openPull(owner, repo, user, head string) pullJSON {
	ts.t.Helper()
	var out pullJSON
	ts.admin(http.MethodPost, "/_sim/repos/"+owner+"/"+repo+"/pulls",
		map[string]string{"user": user, "head": head, "title": "Add " + head}, http.StatusCreated).decode(ts.t, &out)
	return out
}

func appJWT(t *testing.T, a appCredentials) string {
	t.Helper()
	at := time.Now()
	return signJWT(t, a.PrivateKey, a.ClientID, at.Add(-60*time.Second), at.Add(9*time.Minute))
}

func signJWT(t *testing.T, keyPEM string, iss any, iat, exp time.Time) string {
	t.Helper()
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		t.Fatal("app key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse app key: %v", err)
	}
	seg := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := seg(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		seg(map[string]any{"iss": iss, "iat": iat.Unix(), "exp": exp.Unix()})
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func otherKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return noRedirects(jar)
}

func (ts *testSim) signIn(client *http.Client, login string) {
	ts.t.Helper()
	r := postForm(ts.t, client, ts.web.URL+"/session", url.Values{"login": {login}})
	if r.status != http.StatusSeeOther {
		ts.t.Fatalf("sign in = %d %s", r.status, r.body)
	}
}

func postForm(t *testing.T, client *http.Client, rawURL string, form url.Values) response {
	t.Helper()
	return do(t, client, http.MethodPost, rawURL, strings.NewReader(form.Encode()),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
}

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type hookDelivery struct {
	header http.Header
	body   []byte
}

func (d hookDelivery) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(d.body, v); err != nil {
		t.Fatalf("decode delivery: %v", err)
	}
}

type hookReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []hookDelivery
	used   []bool
	status int
	notify chan struct{}
}

func newHookReceiver(t *testing.T) *hookReceiver {
	h := &hookReceiver{status: http.StatusOK, notify: make(chan struct{}, 1)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, hookDelivery{header: r.Header.Clone(), body: body})
		h.used = append(h.used, false)
		status := h.status
		h.mu.Unlock()
		select {
		case h.notify <- struct{}{}:
		default:
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookReceiver) answer(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = status
}

func (h *hookReceiver) take(event, action string) (hookDelivery, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, d := range h.got {
		if h.used[i] || d.header.Get("X-GitHub-Event") != event {
			continue
		}
		var payload struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(d.body, &payload)
		if payload.Action == action {
			h.used[i] = true
			return d, true
		}
	}
	return hookDelivery{}, false
}

func (h *hookReceiver) wait(t *testing.T, event, action string) hookDelivery {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if d, ok := h.take(event, action); ok {
			return d
		}
		select {
		case <-h.notify:
		case <-deadline:
			t.Fatalf("no %s %q webhook arrived", event, action)
		}
	}
}
