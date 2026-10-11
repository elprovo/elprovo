package github

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=octo", "GIT_AUTHOR_EMAIL=octo@example.com",
		"GIT_COMMITTER_NAME=octo", "GIT_COMMITTER_EMAIL=octo@example.com",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (ts *testSim) remote(login, password, owner, repo string) string {
	u, _ := url.Parse(ts.web.URL + "/" + owner + "/" + repo + ".git")
	if login != "" {
		u.User = url.UserPassword(login, password)
	}
	return u.String()
}

func (ts *testSim) clone(login, owner, repo string) string {
	ts.t.Helper()
	dir := filepath.Join(ts.t.TempDir(), repo)
	mustGit(ts.t, "", "clone", "--quiet", ts.remote(login, "any-password", owner, repo), dir)
	return dir
}

func commitFiles(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mustGit(t, dir, "add", "--all")
	mustGit(t, dir, "commit", "--quiet", "-m", "Change files")
	return mustGit(t, dir, "rev-parse", "HEAD")
}

func (ts *testSim) pushBranch(owner, repo, branch string, files map[string]string) string {
	ts.t.Helper()
	dir := ts.clone(owner, owner, repo)
	mustGit(ts.t, dir, "checkout", "--quiet", "-b", branch)
	sha := commitFiles(ts.t, dir, files)
	mustGit(ts.t, dir, "push", "--quiet", "origin", branch)
	return sha
}

func TestPushToAPullRequestBranchSendsPushAndSynchronize(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	a := ts.createApp("octo", nil)
	ts.install(a.Slug, "octo")
	ts.pushBranch("octo", "web", "feature", map[string]string{"a.txt": "1"})
	pr := ts.openPull("octo", "web", "octo", "feature")
	ts.hooks.wait(t, "pull_request", "opened")

	dir := ts.clone("octo", "octo", "web")
	mustGit(t, dir, "checkout", "--quiet", "feature")
	newHead := commitFiles(t, dir, map[string]string{"a.txt": "2", "b.txt": "new"})
	mustGit(t, dir, "push", "--quiet", "origin", "feature")

	var push pushEvent
	ts.hooks.wait(t, "push", "").decode(t, &push)
	for push.Ref != "refs/heads/feature" || push.After != newHead {
		ts.hooks.wait(t, "push", "").decode(t, &push)
	}
	if push.Before != pr.Head.SHA || push.Created || push.Forced || len(push.Commits) != 1 || push.Sender.Login != "octo" {
		t.Fatalf("push = before %s created %v forced %v %d commits sender %s; want a fast-forward of one commit by octo",
			push.Before, push.Created, push.Forced, len(push.Commits), push.Sender.Login)
	}
	if c := push.Commits[0]; c.ID != newHead || strings.Join(c.Added, ",") != "b.txt" || strings.Join(c.Modified, ",") != "a.txt" {
		t.Fatalf("pushed commit = %+v, want %s adding b.txt and modifying a.txt", c, newHead)
	}

	var sync pullRequestEvent
	ts.hooks.wait(t, "pull_request", "synchronize").decode(t, &sync)
	if sync.Number != pr.Number || sync.Before != pr.Head.SHA || sync.After != newHead || sync.PullRequest.Head.SHA != newHead {
		t.Fatalf("synchronize = #%d %s..%s head %s, want #%d %s..%s", sync.Number, sync.Before, sync.After,
			sync.PullRequest.Head.SHA, pr.Number, pr.Head.SHA, newHead)
	}
}

func TestPrivateRepositoryNeedsCredentialsToClone(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createUser("mona")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})

	if out, err := runGit(t, "", "clone", ts.remote("", "", "octo", "web"), filepath.Join(t.TempDir(), "anon")); err == nil {
		t.Fatalf("anonymous clone of a private repository succeeded: %s", out)
	}
	if out, err := runGit(t, "", "clone", ts.remote("mona", "x", "octo", "web"), filepath.Join(t.TempDir(), "mona")); err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("clone by a user without access = %v %s, want repository not found", err, out)
	}

	var tok struct {
		Token string `json:"token"`
	}
	ts.admin(http.MethodPost, "/_sim/users/octo/tokens", nil, http.StatusCreated).decode(t, &tok)
	if !strings.HasPrefix(tok.Token, "ghp_") {
		t.Fatalf("personal token %q does not start with ghp_", tok.Token)
	}
	mustGit(t, "", "clone", "--quiet", ts.remote("octo", tok.Token, "octo", "web"), filepath.Join(t.TempDir(), "octo"))
}

func TestTagPushReportsTheTaggedCommitAsHeadCommit(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	a := ts.createApp("octo", nil)
	ts.install(a.Slug, "octo")
	dir := ts.clone("octo", "octo", "web")
	head := mustGit(t, dir, "rev-parse", "HEAD")

	mustGit(t, dir, "tag", "-a", "v1", "-m", "Release v1")
	mustGit(t, dir, "push", "--quiet", "origin", "v1")

	var push pushEvent
	ts.hooks.wait(t, "push", "").decode(t, &push)
	if push.Ref != "refs/tags/v1" || !push.Created || len(push.Commits) != 0 || push.HeadCommit == nil || push.HeadCommit.ID != head || push.HeadCommit.Distinct {
		t.Fatalf("tag push = ref %s created %v %d commits head %+v; want refs/tags/v1 with head_commit %s", push.Ref, push.Created, len(push.Commits), push.HeadCommit, head)
	}
}

func TestPushToAMissingRepositoryCreatesIt(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "--initial-branch=main")
	commitFiles(t, dir, map[string]string{"README.md": "pushed"})

	mustGit(t, dir, "push", "--quiet", ts.remote("octo", "x", "octo", "pushed"), "main")

	var got repoJSON
	var tok struct {
		Token string `json:"token"`
	}
	ts.admin(http.MethodPost, "/_sim/users/octo/tokens", nil, http.StatusCreated).decode(t, &tok)
	r := ts.call(http.MethodGet, "/repos/octo/pushed", tok.Token, nil)
	r.decode(t, &got)
	if r.status != http.StatusOK || !got.Private || got.DefaultBranch != "main" {
		t.Fatalf("pushed repository = %d %+v, want a private repository with main", r.status, got)
	}
}
