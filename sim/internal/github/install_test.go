package github

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const setupURL = "https://elprovo.test/github/setup"

func TestInstallationFlowRedirectsToSetupURLWithState(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createOrg("acme", "octo")
	ts.createRepo("acme", "web", nil)
	ts.createRepo("acme", "docs", nil)
	a := ts.createApp("octo", map[string]any{"setup_url": setupURL})
	client := browser(t)

	r := do(t, client, http.MethodGet, ts.web.URL+"/apps/"+a.Slug+"/installations/new?state=st-1", nil, nil)
	redirectQuery(t, r, "/login?")
	ts.signIn(client, "octo")
	page := do(t, client, http.MethodGet, ts.web.URL+"/apps/"+a.Slug+"/installations/new?state=st-1", nil, nil)
	if page.status != http.StatusOK || !strings.Contains(string(page.body), `value="acme"`) || !strings.Contains(string(page.body), "acme/docs") {
		t.Fatalf("install page = %d, want account acme with its repositories", page.status)
	}

	webID := strconv.FormatInt(ts.repoID("acme", "web"), 10)
	r = postForm(t, client, ts.web.URL+"/apps/"+a.Slug+"/installations/new", url.Values{
		"state": {"st-1"}, "account": {"acme"}, "repository_selection": {"selected"}, "repository_ids": {webID},
	})
	q := redirectQuery(t, r, setupURL+"?")
	if q.Get("setup_action") != "install" || q.Get("state") != "st-1" || q.Get("installation_id") == "" {
		t.Fatalf("setup redirect query = %v, want install with state st-1", q)
	}
	var created installationEvent
	ts.hooks.wait(t, "installation", "created").decode(t, &created)
	if strconv.FormatInt(created.Installation.ID, 10) != q.Get("installation_id") || len(created.Repositories) != 1 ||
		created.Repositories[0].FullName != "acme/web" || created.Sender.Login != "octo" || created.Installation.RepositorySelection != "selected" {
		t.Fatalf("installation created = %+v, want acme/web selected by octo", created)
	}

	r = postForm(t, client, ts.web.URL+"/apps/"+a.Slug+"/installations/new", url.Values{
		"state": {"st-2"}, "account": {"acme"}, "repository_selection": {"all"},
	})
	if q := redirectQuery(t, r, setupURL+"?"); q.Get("setup_action") != "update" || q.Get("state") != "st-2" {
		t.Fatalf("update redirect query = %v, want update with state st-2", q)
	}
	var added installationReposEvent
	ts.hooks.wait(t, "installation_repositories", "added").decode(t, &added)
	if added.RepositorySelection != "all" || len(added.RepositoriesAdded) != 1 || added.RepositoriesAdded[0].FullName != "acme/docs" {
		t.Fatalf("installation_repositories added = %+v, want acme/docs", added)
	}
}

func TestInstallationFlowRejectsAccountsTheUserDoesNotAdminister(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	a := ts.createApp("octo", nil)
	client := browser(t)
	ts.signIn(client, "octo")

	r := postForm(t, client, ts.web.URL+"/apps/"+a.Slug+"/installations/new", url.Values{"account": {"mona"}, "repository_selection": {"all"}})
	if r.status != http.StatusForbidden {
		t.Fatalf("install on another user's account = %d, want 403", r.status)
	}
}
