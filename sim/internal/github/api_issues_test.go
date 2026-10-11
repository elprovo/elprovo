package github

import (
	"fmt"
	"net/http"
	"testing"
)

type pullFixture struct {
	ts    *testSim
	app   appCredentials
	token string
	pull  pullJSON
}

func newPullFixture(t *testing.T) pullFixture {
	t.Helper()
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	ts.pushBranch("octo", "web", "feature", map[string]string{"a.txt": "1"})
	a := ts.createApp("octo", nil)
	tok := ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)
	return pullFixture{ts: ts, app: a, token: tok, pull: ts.openPull("octo", "web", "octo", "feature")}
}

func TestAppCommentIsPostedAsItsBotAndDeliveredSigned(t *testing.T) {
	f := newPullFixture(t)

	r := f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/comments", f.token, map[string]string{"body": "Stagelet is ready"})
	if r.status != http.StatusCreated {
		t.Fatalf("create comment = %d %s", r.status, r.body)
	}
	var created commentJSON
	r.decode(t, &created)
	if created.User.Login != "stagely-dev[bot]" || created.User.Type != "Bot" || created.PerformedViaGitHubApp == nil {
		t.Fatalf("comment author = %+v via %v, want stagely-dev[bot] through the app", created.User, created.PerformedViaGitHubApp)
	}

	d := f.ts.hooks.wait(t, "issue_comment", "created")
	if got, want := d.header.Get("X-Hub-Signature-256"), "sha256="+hmacHex("hook-secret", d.body); got != want {
		t.Fatalf("X-Hub-Signature-256 = %q, want %q", got, want)
	}
	var e issueCommentEvent
	d.decode(t, &e)
	if e.Comment.ID != created.ID || e.Issue.Number != 1 || e.Issue.PullRequest.URL == "" || e.Installation.ID == 0 || e.Repository.FullName != "octo/web" {
		t.Fatalf("issue_comment payload = %+v, want comment %d on pull request #1 with installation and repository", e, created.ID)
	}
}

func TestCommentsCanBeListedEditedAndDeleted(t *testing.T) {
	f := newPullFixture(t)
	f.ts.admin(http.MethodPost, "/_sim/repos/octo/web/issues/1/comments", map[string]string{"user": "octo", "body": "/stagely deploy"}, http.StatusCreated)
	var mine commentJSON
	f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/comments", f.token, map[string]string{"body": "Deploying"}).decode(t, &mine)

	var list []commentJSON
	f.ts.call(http.MethodGet, "/repos/octo/web/issues/1/comments", f.token, nil).decode(t, &list)
	if len(list) != 2 || list[0].Body != "/stagely deploy" || list[0].AuthorAssociation != "OWNER" {
		t.Fatalf("comments = %+v, want octo's command first, then the app's", list)
	}

	path := fmt.Sprintf("/repos/octo/web/issues/comments/%d", mine.ID)
	if r := f.ts.call(http.MethodPatch, path, f.token, map[string]string{"body": "Deployed"}); r.status != http.StatusOK {
		t.Fatalf("edit comment = %d %s", r.status, r.body)
	}
	var edited issueCommentEvent
	f.ts.hooks.wait(t, "issue_comment", "edited").decode(t, &edited)
	if edited.Comment.Body != "Deployed" || edited.Changes == nil || edited.Changes.Body.From != "Deploying" {
		t.Fatalf("edited event = %+v, want body Deployed changed from Deploying", edited)
	}

	if r := f.ts.call(http.MethodDelete, path, f.token, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete comment = %d %s", r.status, r.body)
	}
	f.ts.hooks.wait(t, "issue_comment", "deleted")
	f.ts.call(http.MethodGet, "/repos/octo/web/issues/1/comments", f.token, nil).decode(t, &list)
	if len(list) != 1 {
		t.Fatalf("%d comments after delete, want 1", len(list))
	}
}

func TestCommentWithoutBodyFailsValidation(t *testing.T) {
	f := newPullFixture(t)

	r := f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/comments", f.token, map[string]string{})
	var body errorBody
	r.decode(t, &body)
	if r.status != http.StatusUnprocessableEntity || body.Message != "Validation Failed" || len(body.Errors) != 1 || body.Errors[0].Field != "body" {
		t.Fatalf("empty comment = %d %s, want 422 Validation Failed on body", r.status, r.body)
	}
	if r := f.ts.call(http.MethodPost, "/repos/octo/web/issues/99/comments", f.token, map[string]string{"body": "x"}); r.status != http.StatusNotFound {
		t.Fatalf("comment on a missing pull request = %d, want 404", r.status)
	}
}

func TestLabelsAreAddedAndRemovedWithWebhooks(t *testing.T) {
	f := newPullFixture(t)

	var labels []labelJSON
	r := f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/labels", f.token, map[string][]string{"labels": {"stagelet"}})
	r.decode(t, &labels)
	if r.status != http.StatusOK || len(labels) != 1 || labels[0].Name != "stagelet" {
		t.Fatalf("add label = %d %s, want [stagelet]", r.status, r.body)
	}
	var labeled pullRequestEvent
	f.ts.hooks.wait(t, "pull_request", "labeled").decode(t, &labeled)
	if labeled.Label == nil || labeled.Label.Name != "stagelet" || labeled.Sender.Login != "stagely-dev[bot]" {
		t.Fatalf("labeled event label %v sender %s, want stagelet by the bot", labeled.Label, labeled.Sender.Login)
	}

	f.ts.call(http.MethodGet, "/repos/octo/web/issues/1/labels", f.token, nil).decode(t, &labels)
	if len(labels) != 1 {
		t.Fatalf("labels = %+v, want one", labels)
	}
	if r := f.ts.call(http.MethodDelete, "/repos/octo/web/issues/1/labels/stagelet", f.token, nil); r.status != http.StatusOK {
		t.Fatalf("remove label = %d %s", r.status, r.body)
	}
	f.ts.hooks.wait(t, "pull_request", "unlabeled")
	if r := f.ts.call(http.MethodDelete, "/repos/octo/web/issues/1/labels/stagelet", f.token, nil); r.status != http.StatusNotFound {
		t.Fatalf("remove a label that is not set = %d, want 404", r.status)
	}

	f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/labels", f.token, []string{"a", "b"})
	if r := f.ts.call(http.MethodDelete, "/repos/octo/web/issues/1/labels", f.token, nil); r.status != http.StatusNoContent {
		t.Fatalf("remove all labels = %d %s", r.status, r.body)
	}
	f.ts.call(http.MethodGet, "/repos/octo/web/issues/1/labels", f.token, nil).decode(t, &labels)
	if len(labels) != 0 {
		t.Fatalf("labels after removing all = %+v", labels)
	}
}

func TestStatusesAndDeploymentsHonorRequiredContexts(t *testing.T) {
	f := newPullFixture(t)
	sha := f.pull.Head.SHA

	r := f.ts.call(http.MethodPost, "/repos/octo/web/statuses/"+sha, f.token,
		map[string]string{"state": "failure", "context": "ci", "target_url": "https://ci.example/1"})
	var st statusJSON
	r.decode(t, &st)
	if r.status != http.StatusCreated || st.State != "failure" || st.Context != "ci" || st.Creator.Login != "stagely-dev[bot]" {
		t.Fatalf("create status = %d %s", r.status, r.body)
	}
	if r := f.ts.call(http.MethodPost, "/repos/octo/web/statuses/"+sha, f.token, map[string]string{"state": "bogus"}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("status with an invalid state = %d, want 422", r.status)
	}

	if r := f.ts.call(http.MethodPost, "/repos/octo/web/deployments", f.token, map[string]any{"ref": "feature"}); r.status != http.StatusConflict {
		t.Fatalf("deployment with a failed status = %d %s, want 409", r.status, r.body)
	}
	r = f.ts.call(http.MethodPost, "/repos/octo/web/deployments", f.token, map[string]any{
		"ref": "feature", "environment": "pr-1", "required_contexts": []string{}, "transient_environment": true,
	})
	var dep deploymentJSON
	r.decode(t, &dep)
	if r.status != http.StatusCreated || dep.SHA != sha || dep.Environment != "pr-1" || dep.ProductionEnvironment || !dep.TransientEnvironment {
		t.Fatalf("create deployment = %d %s", r.status, r.body)
	}

	r = f.ts.call(http.MethodPost, fmt.Sprintf("/repos/octo/web/deployments/%d/statuses", dep.ID), f.token,
		map[string]string{"state": "success", "environment_url": "https://pr-1.stagely.dev"})
	var ds deploymentStatusJSON
	r.decode(t, &ds)
	if r.status != http.StatusCreated || ds.State != "success" || ds.EnvironmentURL != "https://pr-1.stagely.dev" || ds.Environment != "pr-1" {
		t.Fatalf("create deployment status = %d %s", r.status, r.body)
	}
}

func TestInstallationWithoutWritePermissionCannotComment(t *testing.T) {
	f := newPullFixture(t)
	readOnly := f.ts.installationToken(f.app, f.ts.install(f.app.Slug, "octo").ID,
		map[string]any{"permissions": map[string]string{"issues": "read", "pull_requests": "read"}})

	r := f.ts.call(http.MethodPost, "/repos/octo/web/issues/1/comments", readOnly, map[string]string{"body": "x"})
	var body errorBody
	r.decode(t, &body)
	if r.status != http.StatusForbidden || body.Message != "Resource not accessible by integration" {
		t.Fatalf("comment with read-only token = %d %s, want 403 Resource not accessible by integration", r.status, r.body)
	}
}
