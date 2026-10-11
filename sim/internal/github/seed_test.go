package github

import (
	"log/slog"
	"net/http"
	"os"
	"testing"
)

func TestDefaultUserAndAppSurviveARestart(t *testing.T) {
	seed := func(c *Config) {
		c.DefaultUser = "octo"
		c.DefaultApp = DefaultApp{Name: "Stagely Local", WebhookURL: "http://127.0.0.1:1/hooks", CallbackURLs: []string{callbackURL}, SetupURL: setupURL}
	}
	ts := newTestSim(t, seed)
	var first appCredentials
	ts.admin(http.MethodGet, "/_sim/apps/stagely-local", nil, http.StatusOK).decode(t, &first)
	if first.Owner != "octo" || first.WebhookSecret == "" || first.SetupURL != setupURL || first.CallbackURLs[0] != callbackURL {
		t.Fatalf("seeded app = %+v, want owner octo with the flag values", first)
	}
	key, err := os.ReadFile(first.PrivateKeyPath)
	if err != nil || string(key) != first.PrivateKey {
		t.Fatalf("private key file %s: %v, want the app's PEM", first.PrivateKeyPath, err)
	}

	ts.sim.Close()
	restarted := newTestSim(t, seed, func(c *Config) {
		c.DataDir = ts.cfg.DataDir
		c.DefaultApp.WebhookURL = "http://127.0.0.1:2/hooks"
	})
	var second appCredentials
	restarted.admin(http.MethodGet, "/_sim/apps/stagely-local", nil, http.StatusOK).decode(t, &second)
	if second.ID != first.ID || second.ClientID != first.ClientID || second.PrivateKey != first.PrivateKey || second.WebhookSecret != first.WebhookSecret {
		t.Fatalf("app after restart = %+v, want the same identity and keys as %+v", second, first)
	}
	if second.WebhookURL != "http://127.0.0.1:2/hooks" {
		t.Fatalf("webhook URL after restart = %q, want the new flag value", second.WebhookURL)
	}
	if r := restarted.call(http.MethodGet, "/app", appJWT(t, second), nil); r.status != http.StatusOK {
		t.Fatalf("JWT with the kept key after restart = %d, want 200", r.status)
	}
}

func TestRepositoriesAndPullRequestsSurviveARestart(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	ts.pushBranch("octo", "web", "feature", map[string]string{"a.txt": "1"})
	ts.openPull("octo", "web", "octo", "feature")

	ts.sim.Close()
	restarted := newTestSim(t, func(c *Config) { c.DataDir = ts.cfg.DataDir })
	var tok struct {
		Token string `json:"token"`
	}
	restarted.admin(http.MethodPost, "/_sim/users/octo/tokens", nil, http.StatusCreated).decode(t, &tok)
	var p pullJSON
	r := restarted.call(http.MethodGet, "/repos/octo/web/pulls/1", tok.Token, nil)
	r.decode(t, &p)
	if r.status != http.StatusOK || p.Head.Ref != "feature" {
		t.Fatalf("pull request after restart = %d %+v", r.status, p)
	}
}

func TestDefaultAppNeedsADefaultUser(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir(), DefaultApp: DefaultApp{Name: "Lonely"}}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("New with a default app and no default user succeeded")
	}
}
