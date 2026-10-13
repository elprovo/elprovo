package github

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAppJWTAuthenticatesAsTheApp(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	at := time.Now()

	for _, tc := range []struct {
		name string
		iss  any
	}{
		{"client ID issuer", a.ClientID},
		{"numeric app ID issuer", a.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jwt := signJWT(t, a.PrivateKey, tc.iss, at.Add(-60*time.Second), at.Add(10*time.Minute))
			r := ts.call(http.MethodGet, "/app", jwt, nil)
			if r.status != http.StatusOK {
				t.Fatalf("GET /app = %d %s", r.status, r.body)
			}
			var got appJSON
			r.decode(t, &got)
			if got.ID != a.ID || got.Slug != "elprovo-dev" || got.Owner.Login != "octo" {
				t.Fatalf("GET /app = %+v, want app %d elprovo-dev owned by octo", got, a.ID)
			}
		})
	}
}

func TestAppJWTRejections(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	at := time.Now()

	for _, tc := range []struct {
		name    string
		jwt     string
		message string
	}{
		{
			name:    "expired",
			jwt:     signJWT(t, a.PrivateKey, a.ClientID, at.Add(-15*time.Minute), at.Add(-5*time.Minute)),
			message: "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires",
		},
		{
			name:    "expiry more than ten minutes ahead",
			jwt:     signJWT(t, a.PrivateKey, a.ClientID, at, at.Add(20*time.Minute)),
			message: "'Expiration time' claim ('exp') is too far in the future",
		},
		{
			name:    "issued in the future",
			jwt:     signJWT(t, a.PrivateKey, a.ClientID, at.Add(5*time.Minute), at.Add(9*time.Minute)),
			message: "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued",
		},
		{
			name:    "signed with another key",
			jwt:     signJWT(t, otherKeyPEM(t), a.ClientID, at.Add(-time.Minute), at.Add(5*time.Minute)),
			message: "A JSON web token could not be decoded",
		},
		{
			name:    "unknown issuer",
			jwt:     signJWT(t, a.PrivateKey, "Iv23liUnknownClient", at.Add(-time.Minute), at.Add(5*time.Minute)),
			message: "Integration not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ts.call(http.MethodGet, "/app", tc.jwt, nil)
			var body errorBody
			r.decode(t, &body)
			if r.status != http.StatusUnauthorized || body.Message != tc.message {
				t.Fatalf("GET /app = %d %q, want 401 %q", r.status, body.Message, tc.message)
			}
			if body.DocumentationURL == "" {
				t.Fatalf("error body %s has no documentation_url", r.body)
			}
		})
	}
}

func TestInstallationTokenReachesOnlyItsInstallationRepos(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	ts.createRepo("octo", "web", map[string]string{"README.md": "octo"})
	ts.createRepo("mona", "api", map[string]string{"README.md": "mona"})
	a := ts.createApp("octo", nil)
	octoInst := ts.install(a.Slug, "octo")
	ts.install(a.Slug, "mona")

	tok := ts.installationToken(a, octoInst.ID, nil)
	if !strings.HasPrefix(tok, "ghs_") {
		t.Fatalf("installation token %q does not start with ghs_", tok)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/web", tok, nil); r.status != http.StatusOK {
		t.Fatalf("GET own repo = %d %s, want 200", r.status, r.body)
	}
	if r := ts.call(http.MethodGet, "/repos/mona/api", tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("GET other installation's private repo = %d, want 404", r.status)
	}
}

func TestInstallationTokenCanBeNarrowedToRepositories(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", nil)
	ts.createRepo("octo", "docs", nil)
	ts.createUser("mona")
	ts.createRepo("mona", "api", nil)
	a := ts.createApp("octo", nil)
	inst := ts.install(a.Slug, "octo")

	r := ts.call(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), appJWT(t, a),
		map[string]any{"repositories": []string{"web"}, "permissions": map[string]string{"contents": "read"}})
	if r.status != http.StatusCreated {
		t.Fatalf("create narrowed token = %d %s", r.status, r.body)
	}
	var got installationTokenJSON
	r.decode(t, &got)
	if got.RepositorySelection != "selected" || len(got.Repositories) != 1 || got.Repositories[0].Name != "web" {
		t.Fatalf("narrowed token = %+v, want only octo/web selected", got)
	}
	if left := time.Until(got.ExpiresAt); left < 59*time.Minute || left > time.Hour {
		t.Fatalf("token expires in %s, want one hour", left)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/docs", got.Token, nil); r.status != http.StatusNotFound {
		t.Fatalf("GET repo outside the narrowed set = %d, want 404", r.status)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/web/pulls", got.Token, nil); r.status != http.StatusForbidden {
		t.Fatalf("GET pulls without pull_requests permission = %d, want 403", r.status)
	}

	empty := ts.installationToken(a, inst.ID, map[string]any{"permissions": map[string]string{}})
	if r := ts.call(http.MethodGet, "/repos/octo/web/pulls", empty, nil); r.status != http.StatusOK {
		t.Fatalf("token requested with empty permissions = %d on pulls, want the installation's permissions", r.status)
	}

	r = ts.call(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), appJWT(t, a),
		map[string]any{"repository_ids": []int64{ts.repoID("mona", "api")}})
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("token for a repository outside the installation = %d, want 422", r.status)
	}
}

func (ts *testSim) repoID(owner, name string) int64 {
	var id int64
	ts.sim.read(func(v view) { id = v.w.repo(owner, name).ID })
	return id
}

func TestListAppInstallationsPaginatesWithLinkHeader(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	for _, login := range []string{"one", "two", "three"} {
		ts.createUser(login)
		ts.install(a.Slug, login)
	}

	first := ts.call(http.MethodGet, "/app/installations?per_page=2", appJWT(t, a), nil)
	var page1 []installationJSON
	first.decode(t, &page1)
	wantFirst := fmt.Sprintf(`<%s/app/installations?page=2&per_page=2>; rel="next", <%s/app/installations?page=2&per_page=2>; rel="last"`, ts.api.URL, ts.api.URL)
	if len(page1) != 2 || first.header.Get("Link") != wantFirst {
		t.Fatalf("page 1: %d installations, Link %q; want 2 and %q", len(page1), first.header.Get("Link"), wantFirst)
	}

	second := ts.call(http.MethodGet, "/app/installations?per_page=2&page=2", appJWT(t, a), nil)
	var page2 []installationJSON
	second.decode(t, &page2)
	wantSecond := fmt.Sprintf(`<%s/app/installations?page=1&per_page=2>; rel="prev", <%s/app/installations?page=1&per_page=2>; rel="first"`, ts.api.URL, ts.api.URL)
	if len(page2) != 1 || second.header.Get("Link") != wantSecond {
		t.Fatalf("page 2: %d installations, Link %q; want 1 and %q", len(page2), second.header.Get("Link"), wantSecond)
	}

	beyond := ts.call(http.MethodGet, "/app/installations?page=9223372036854775807", appJWT(t, a), nil)
	if beyond.status != http.StatusOK || string(beyond.body) != "[]\n" {
		t.Fatalf("page far past the end = %d %q, want 200 and an empty list", beyond.status, beyond.body)
	}
}

func TestDeletingAnInstallationRevokesTokensAndNotifiesTheApp(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", nil)
	a := ts.createApp("octo", nil)
	inst := ts.install(a.Slug, "octo")
	tok := ts.installationToken(a, inst.ID, nil)

	r := ts.call(http.MethodDelete, fmt.Sprintf("/app/installations/%d", inst.ID), appJWT(t, a), nil)
	if r.status != http.StatusAccepted {
		t.Fatalf("DELETE installation = %d %s, want 202", r.status, r.body)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/web", tok, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("token after uninstall = %d, want 401", r.status)
	}
	var e installationEvent
	ts.hooks.wait(t, "installation", "deleted").decode(t, &e)
	if e.Installation.ID != inst.ID {
		t.Fatalf("deleted event for installation %d, want %d", e.Installation.ID, inst.ID)
	}
}

func TestSuspendedInstallationCannotMintTokens(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	inst := ts.install(a.Slug, "octo")
	path := fmt.Sprintf("/app/installations/%d", inst.ID)

	if r := ts.call(http.MethodPut, path+"/suspended", appJWT(t, a), nil); r.status != http.StatusNoContent {
		t.Fatalf("suspend = %d %s", r.status, r.body)
	}
	ts.hooks.wait(t, "installation", "suspend")
	if r := ts.call(http.MethodPost, path+"/access_tokens", appJWT(t, a), nil); r.status != http.StatusForbidden {
		t.Fatalf("token for suspended installation = %d, want 403", r.status)
	}
	if r := ts.call(http.MethodDelete, path+"/suspended", appJWT(t, a), nil); r.status != http.StatusNoContent {
		t.Fatalf("unsuspend = %d %s", r.status, r.body)
	}
	ts.hooks.wait(t, "installation", "unsuspend")
	ts.installationToken(a, inst.ID, nil)
}

func TestAppEndpointsRequireAJWT(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	tok := ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)

	if r := ts.call(http.MethodGet, "/app", tok, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("GET /app with an installation token = %d, want 401", r.status)
	}
	if r := ts.call(http.MethodGet, "/app", "ghs_unknown", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("GET /app with an unknown token = %d, want 401", r.status)
	}
}

func TestInstallationRepositoriesListsCoveredRepos(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", nil)
	ts.createRepo("octo", "docs", nil)
	a := ts.createApp("octo", nil)
	tok := ts.installationToken(a, ts.install(a.Slug, "octo", "docs").ID, nil)

	var got installationReposJSON
	ts.call(http.MethodGet, "/installation/repositories", tok, nil).decode(t, &got)
	if got.TotalCount != 1 || got.RepositorySelection != "selected" || got.Repositories[0].FullName != "octo/docs" {
		t.Fatalf("installation repositories = %+v, want only octo/docs", got)
	}
}

func TestRateLimitHeadersCountDownAndBlockAtZero(t *testing.T) {
	ts := newTestSim(t, func(c *Config) { c.RateLimit = 2 })
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	jwt := appJWT(t, a)

	first := ts.call(http.MethodGet, "/app", jwt, nil)
	if first.header.Get("X-RateLimit-Limit") != "2" || first.header.Get("X-RateLimit-Remaining") != "1" || first.header.Get("X-RateLimit-Reset") == "" {
		t.Fatalf("rate limit headers = %v, want limit 2 remaining 1 and a reset time", first.header)
	}
	ts.call(http.MethodGet, "/app", jwt, nil)
	third := ts.call(http.MethodGet, "/app", jwt, nil)
	if third.status != http.StatusForbidden || third.header.Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("third request = %d remaining %q, want 403 and 0", third.status, third.header.Get("X-RateLimit-Remaining"))
	}
}
