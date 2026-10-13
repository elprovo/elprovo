package github

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestPullRequestPagesDriveTheWholeReviewCycle(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	a := ts.createApp("octo", nil)
	ts.install(a.Slug, "octo")
	head := ts.pushBranch("octo", "web", "feature", map[string]string{"feature.txt": "new"})
	client := browser(t)
	ts.signIn(client, "octo")
	pull := ts.web.URL + "/octo/web/pull/1"

	repoPage := do(t, client, http.MethodGet, ts.web.URL+"/octo/web", nil, nil)
	if repoPage.status != http.StatusOK || !strings.Contains(string(repoPage.body), `<option value="feature">`) {
		t.Fatalf("repository page = %d, want a form offering the feature branch", repoPage.status)
	}
	r := postForm(t, client, ts.web.URL+"/octo/web/pulls", url.Values{"head": {"feature"}, "base": {"main"}, "title": {"Add feature"}})
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/octo/web/pull/1" {
		t.Fatalf("create pull request = %d Location %q", r.status, r.header.Get("Location"))
	}
	ts.hooks.wait(t, "pull_request", "opened")

	postForm(t, client, pull+"/comments", url.Values{"body": {"Looks good"}})
	postForm(t, client, pull+"/labels", url.Values{"name": {"preview"}})
	page := string(do(t, client, http.MethodGet, pull, nil, nil).body)
	if !strings.Contains(page, "Looks good") || !strings.Contains(page, `<span class="label">preview</span>`) {
		t.Fatalf("pull request page does not show the comment and the label:\n%s", page)
	}
	ts.hooks.wait(t, "issue_comment", "created")
	ts.hooks.wait(t, "pull_request", "labeled")

	postForm(t, client, pull+"/labels/delete", url.Values{"name": {"preview"}})
	ts.hooks.wait(t, "pull_request", "unlabeled")
	postForm(t, client, pull+"/state", url.Values{"state": {"closed"}})
	ts.hooks.wait(t, "pull_request", "closed")
	postForm(t, client, pull+"/state", url.Values{"state": {"open"}})
	ts.hooks.wait(t, "pull_request", "reopened")

	if r := postForm(t, client, pull+"/merge", nil); r.status != http.StatusSeeOther {
		t.Fatalf("merge = %d %s", r.status, r.body)
	}
	var merged pullRequestEvent
	ts.hooks.wait(t, "pull_request", "closed").decode(t, &merged)
	if !merged.PullRequest.Merged || merged.PullRequest.MergedBy == nil || merged.PullRequest.MergeCommitSHA == nil {
		t.Fatalf("closed event after merge = %+v, want merged with a merge commit", merged.PullRequest)
	}
	if !strings.Contains(string(do(t, client, http.MethodGet, pull, nil, nil).body), `id="pull-state">merged`) {
		t.Fatal("pull request page does not show the merged state")
	}
	var push pushEvent
	ts.hooks.wait(t, "push", "").decode(t, &push)
	for push.Ref != "refs/heads/main" {
		ts.hooks.wait(t, "push", "").decode(t, &push)
	}
	if push.HeadCommit == nil || push.HeadCommit.ID != *merged.PullRequest.MergeCommitSHA || strings.Join(push.HeadCommit.Added, ",") != "feature.txt" {
		t.Fatalf("push to main after merge has head_commit %+v, want the merge commit adding feature.txt", push.HeadCommit)
	}

	tok := ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)
	raw := do(t, http.DefaultClient, http.MethodGet, ts.api.URL+"/repos/octo/web/contents/feature.txt?ref=main", nil,
		http.Header{"Authorization": {"Bearer " + tok}, "Accept": {"application/vnd.github.raw+json"}})
	if raw.status != http.StatusOK || string(raw.body) != "new" {
		t.Fatalf("main after merge has feature.txt = %d %q, want the merged file", raw.status, raw.body)
	}
	var commit commitJSON
	ts.call(http.MethodGet, "/repos/octo/web/commits/main", tok, nil).decode(t, &commit)
	if len(commit.Parents) != 2 || commit.Parents[1].SHA != head {
		t.Fatalf("main head parents = %+v, want a merge commit with %s", commit.Parents, head)
	}
}

func TestRepositoryActionsNeedWriteAccess(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	ts.pushBranch("octo", "web", "feature", map[string]string{"a.txt": "1"})
	ts.openPull("octo", "web", "octo", "feature")
	client := browser(t)
	ts.signIn(client, "mona")

	if r := do(t, client, http.MethodGet, ts.web.URL+"/octo/web", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("private repository page for a stranger = %d, want 404", r.status)
	}
	ts.admin(http.MethodPut, "/_sim/repos/octo/web/collaborators/mona", map[string]string{"permission": "read"}, http.StatusNoContent)
	if r := postForm(t, client, ts.web.URL+"/octo/web/pull/1/merge", nil); r.status != http.StatusForbidden {
		t.Fatalf("merge by a reader = %d, want 403", r.status)
	}
	if r := postForm(t, client, ts.web.URL+"/octo/web/pull/1/comments", url.Values{"body": {"hi"}}); r.status != http.StatusSeeOther {
		t.Fatalf("comment by a reader = %d, want 303", r.status)
	}
}

func TestSignUpCreatesAUserAndSignsIn(t *testing.T) {
	ts := newTestSim(t)
	client := browser(t)

	r := postForm(t, client, ts.web.URL+"/signup", url.Values{"login": {"newbie"}, "name": {"New Bie"}, "return_to": {"/"}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("sign up = %d %s", r.status, r.body)
	}
	home := do(t, client, http.MethodGet, ts.web.URL+"/", nil, nil)
	if !strings.Contains(string(home.body), `id="signed-in-user">newbie`) {
		t.Fatal("home page does not show the new user as signed in")
	}
	login := do(t, client, http.MethodGet, ts.web.URL+"/login", nil, nil)
	if !strings.Contains(string(login.body), "Sign in as newbie") {
		t.Fatal("sign-in page does not list the new user")
	}
	if r := postForm(t, client, ts.web.URL+"/signup", url.Values{"login": {"newbie"}}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate sign up = %d, want 422", r.status)
	}
}

func TestReturnToStaysOnTheSimulation(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")

	for _, returnTo := range []string{"//evil.test/x", "/\\evil.test", "/\t/evil.test", "https://evil.test/", "repos"} {
		r := postForm(t, browser(t), ts.web.URL+"/session", url.Values{"login": {"octo"}, "return_to": {returnTo}})
		if r.header.Get("Location") != "/" {
			t.Fatalf("sign in with return_to %q redirected to %q, want /", returnTo, r.header.Get("Location"))
		}
	}
	r := postForm(t, browser(t), ts.web.URL+"/session", url.Values{"login": {"octo"}, "return_to": {"/octo/web?tab=pulls"}})
	if r.header.Get("Location") != "/octo/web?tab=pulls" {
		t.Fatalf("sign in with a local return_to redirected to %q", r.header.Get("Location"))
	}
}

func TestConcurrentMergesWriteOneMergeCommit(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	ts.pushBranch("octo", "web", "feature", map[string]string{"a.txt": "1"})
	ts.openPull("octo", "web", "octo", "feature")
	var tok struct {
		Token string `json:"token"`
	}
	ts.admin(http.MethodPost, "/_sim/users/octo/tokens", nil, http.StatusCreated).decode(t, &tok)
	var before commitJSON
	ts.call(http.MethodGet, "/repos/octo/web/commits/main", tok.Token, nil).decode(t, &before)

	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			r := do(t, http.DefaultClient, http.MethodPut, ts.web.URL+"/_sim/repos/octo/web/pulls/1/merge", jsonBody(t, map[string]string{"user": "octo"}), nil)
			statuses <- r.status
		})
	}
	wg.Wait()
	close(statuses)
	var got []int
	for s := range statuses {
		got = append(got, s)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusOK, http.StatusMethodNotAllowed}) {
		t.Fatalf("concurrent merge statuses = %v, want one 200 and one 405", got)
	}

	var p pullJSON
	ts.call(http.MethodGet, "/repos/octo/web/pulls/1", tok.Token, nil).decode(t, &p)
	var main commitJSON
	ts.call(http.MethodGet, "/repos/octo/web/commits/main", tok.Token, nil).decode(t, &main)
	if p.MergeCommitSHA == nil || main.SHA != *p.MergeCommitSHA || main.Parents[0].SHA != before.SHA {
		t.Fatalf("main is %s with first parent %s; want the one merge commit %v on top of %s", main.SHA, main.Parents[0].SHA, p.MergeCommitSHA, before.SHA)
	}
}

func TestANewUserCreatesRepositoriesThatTheirAllRepositoriesInstallationCovers(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	a := ts.createApp("octo", nil)
	inst := ts.install(a.Slug, "mona")
	client := browser(t)
	ts.signIn(client, "mona")

	if page := do(t, client, http.MethodGet, ts.web.URL+"/new", nil, nil); page.status != http.StatusOK || !strings.Contains(string(page.body), `<option value="mona">`) {
		t.Fatalf("new repository page = %d, want mona as an owner", page.status)
	}
	empty := postForm(t, client, ts.web.URL+"/new", url.Values{"owner": {"mona"}, "name": {"app"}, "private": {"on"}})
	withReadme := postForm(t, client, ts.web.URL+"/new", url.Values{"owner": {"mona"}, "name": {"notes"}, "auto_init": {"on"}})

	if empty.status != http.StatusSeeOther || empty.header.Get("Location") != "/mona/app" || withReadme.header.Get("Location") != "/mona/notes" {
		t.Fatalf("create repositories = %d %q, %q", empty.status, empty.header.Get("Location"), withReadme.header.Get("Location"))
	}
	if page := string(do(t, client, http.MethodGet, ts.web.URL+"/mona/app", nil, nil).body); !strings.Contains(page, `id="empty-repository"`) {
		t.Fatal("the empty repository's page does not tell how to push to it")
	}
	if page := string(do(t, client, http.MethodGet, ts.web.URL+"/mona/notes", nil, nil).body); strings.Contains(page, `id="empty-repository"`) || !strings.Contains(page, `<option value="main"`) {
		t.Fatal("the repository with a README has no main branch")
	}
	var listed struct {
		Repositories []repoJSON `json:"repositories"`
	}
	ts.call(http.MethodGet, "/installation/repositories", ts.installationToken(a, inst.ID, nil), nil).decode(t, &listed)
	names := []string{}
	for _, r := range listed.Repositories {
		names = append(names, r.FullName)
	}
	if !slices.Contains(names, "mona/app") || !slices.Contains(names, "mona/notes") {
		t.Fatalf("installation repositories = %v, want the new repositories", names)
	}
	if r := postForm(t, client, ts.web.URL+"/new", url.Values{"owner": {"octo"}, "name": {"theirs"}}); r.status != http.StatusForbidden {
		t.Fatalf("create a repository for another user = %d, want 403", r.status)
	}
}

func TestOrganizationAdminsManageItsPeople(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	ts.createOrg("acme", "octo")
	ts.createRepo("acme", "web", map[string]string{"README.md": "x"})
	admin, member := browser(t), browser(t)
	ts.signIn(admin, "octo")
	ts.signIn(member, "mona")
	people := ts.web.URL + "/orgs/acme/people"
	if r := do(t, member, http.MethodGet, people, nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("people page for a non-member = %d, want 404", r.status)
	}

	if r := postForm(t, admin, people, url.Values{"login": {"mona"}, "role": {"member"}}); r.status != http.StatusSeeOther {
		t.Fatalf("add mona = %d %s", r.status, r.body)
	}

	if r := do(t, member, http.MethodGet, ts.web.URL+"/acme/web", nil, nil); r.status != http.StatusOK {
		t.Fatalf("acme/web for the new member = %d, want 200", r.status)
	}
	if page := string(do(t, member, http.MethodGet, people, nil, nil).body); !strings.Contains(page, "mona") || strings.Contains(page, "Save member") {
		t.Fatal("a member sees the people page without the admin form")
	}
	if r := postForm(t, member, people, url.Values{"login": {"mona"}, "role": {"admin"}}); r.status != http.StatusForbidden {
		t.Fatalf("a member changes roles = %d, want 403", r.status)
	}
	for name, form := range map[string]url.Values{
		"remove the last admin": {"action": {"remove"}, "login": {"octo"}},
		"demote the last admin": {"login": {"octo"}, "role": {"member"}},
	} {
		if r := postForm(t, admin, people, form); r.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422", name, r.status)
		}
	}
	if r := postForm(t, admin, people, url.Values{"action": {"remove"}, "login": {"mona"}}); r.status != http.StatusSeeOther {
		t.Fatalf("remove mona = %d %s", r.status, r.body)
	}
	if r := do(t, member, http.MethodGet, ts.web.URL+"/acme/web", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("acme/web after mona left = %d, want 404", r.status)
	}
}
