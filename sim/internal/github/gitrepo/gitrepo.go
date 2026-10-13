// Package gitrepo reads and writes bare git repositories by running the git
// command with argument lists. It isolates git from the user's global and
// system configuration, so hooks and settings on the host do not apply.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound means that a ref, commit or path does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict means that two commits cannot be merged without conflicts.
var ErrConflict = errors.New("merge conflict")

// ZeroSHA is the object name git uses for a ref that does not exist.
const ZeroSHA = "0000000000000000000000000000000000000000"

var isolatedEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_LITERAL_PATHSPECS=1",
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
}

// Repo is a bare repository on disk.
type Repo struct {
	dir string
}

// Signature names the author and committer of a commit that the
// simulation writes.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// Open returns the bare repository at dir without checking it.
func Open(dir string) *Repo { return &Repo{dir: dir} }

// Init creates an empty bare repository at dir whose HEAD points at
// defaultBranch.
func Init(ctx context.Context, dir, defaultBranch string) (*Repo, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("create repository parent: %w", err)
	}
	if _, err := run(ctx, "", nil, "init", "--quiet", "--bare", "--initial-branch="+defaultBranch, dir); err != nil {
		return nil, err
	}
	return Open(dir), nil
}

// SeedFromDir commits every file under src, except ignored ones, as the
// first commit of branch.
func (r *Repo) SeedFromDir(ctx context.Context, src, branch, message string, sig Signature) (string, error) {
	tmp, err := os.MkdirTemp("", "elprovo-sim-index-*")
	if err != nil {
		return "", fmt.Errorf("create index directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	if _, err := run(ctx, src, env, "--git-dir="+r.dir, "--work-tree="+src, "add", "--all"); err != nil {
		return "", err
	}
	tree, err := r.git(ctx, env, "write-tree")
	if err != nil {
		return "", err
	}
	sha, err := r.commitTree(ctx, line(tree), message, sig)
	if err != nil {
		return "", err
	}
	return sha, r.UpdateRef(ctx, "refs/heads/"+branch, sha, "")
}

// ResolveCommit returns the commit that ref names. ref may be a branch,
// a tag or an object name.
func (r *Repo) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", ErrNotFound
	}
	out, err := r.git(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if exitCode(err) == 1 {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return line(out), nil
}

// Refs returns every branch and tag with the commit it points at.
func (r *Repo) Refs(ctx context.Context) (map[string]string, error) {
	out, err := r.git(ctx, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, name, ok := strings.Cut(l, " ")
		if ok {
			refs[name] = sha
		}
	}
	return refs, nil
}

// Branches returns branch names, without the refs/heads/ prefix, in order.
func (r *Repo) Branches(ctx context.Context) ([]string, error) {
	out, err := r.git(ctx, nil, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// UpdateRef points ref at newSHA. When oldSHA is set, the update succeeds
// only while ref still points at oldSHA.
func (r *Repo) UpdateRef(ctx context.Context, ref, newSHA, oldSHA string) error {
	args := []string{"update-ref", ref, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	_, err := r.git(ctx, nil, args...)
	return err
}

// IsAncestor reports whether commit a is reachable from commit b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.git(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if exitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

// CountCommits returns how many commits head has that base does not.
func (r *Repo) CountCommits(ctx context.Context, base, head string) (int, error) {
	out, err := r.git(ctx, nil, "rev-list", "--count", head, "--not", base)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(line(out))
}

// Merge writes a merge commit of head into base and returns it. It does not
// move any ref.
func (r *Repo) Merge(ctx context.Context, base, head, message string, sig Signature) (string, error) {
	out, err := r.git(ctx, nil, "merge-tree", "--write-tree", "--no-messages", base, head)
	if exitCode(err) == 1 {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	return r.commitTree(ctx, line(out), message, sig, base, head)
}

// Archive writes a gzip-compressed tar of commit to w. Every path in the
// archive starts with prefix and a slash.
func (r *Repo) Archive(ctx context.Context, w io.Writer, commit, prefix string) error {
	cmd := r.command(ctx, nil, "archive", "--format=tar.gz", "--prefix="+prefix+"/", commit)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (r *Repo) commitTree(ctx context.Context, tree, message string, sig Signature, parents ...string) (string, error) {
	date := sig.When.Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email, "GIT_COMMITTER_DATE=" + date,
	}
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := r.git(ctx, env, args...)
	if err != nil {
		return "", err
	}
	return line(out), nil
}

func (r *Repo) git(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return run(ctx, "", env, append([]string{"--git-dir=" + r.dir}, args...)...)
}

func (r *Repo) command(ctx context.Context, env []string, args ...string) *exec.Cmd {
	return command(ctx, "", env, append([]string{"--git-dir=" + r.dir}, args...)...)
}

func run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, env, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &commandError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

func command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(hostEnv(), isolatedEnv...), env...)
	return cmd
}

func hostEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

type commandError struct {
	args   []string
	err    error
	stderr string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *commandError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

func line(out []byte) string { return strings.TrimSpace(string(out)) }
