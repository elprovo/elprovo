// Package testrepo makes throwaway Git repositories for tests.
package testrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a Git repository in a test's temporary directory.
type Repo struct {
	t   testing.TB
	Dir string
}

// New makes an empty repository on branch main. It points Git at no user or
// system configuration, so that the tests do not depend on the machine.
func New(t testing.TB) Repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	r := Repo{t: t, Dir: t.TempDir()}
	r.Git("init", "-q", "-b", "main")
	return r
}

// Commit writes files, deletes the paths in remove, commits everything and
// returns the commit ID.
func (r Repo) Commit(files map[string]string, remove ...string) string {
	r.t.Helper()
	for name, content := range files {
		path := filepath.Join(r.Dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	for _, name := range remove {
		r.Git("rm", "-q", "-r", "--", name)
	}
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", "change")
	return r.Git("rev-parse", "HEAD")
}

// Git runs git in the repository and returns its trimmed output.
func (r Repo) Git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
