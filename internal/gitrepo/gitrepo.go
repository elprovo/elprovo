// Package gitrepo reads commits from a Git repository with the git command,
// and writes the files of a commit into a directory.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is a Git repository on disk.
type Repo struct {
	root string
}

// Open finds the repository that contains dir.
func Open(ctx context.Context, dir string) (Repo, error) {
	out, err := git(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repo{}, fmt.Errorf("%s is not inside a Git repository: %w", dir, err)
	}
	return Repo{root: strings.TrimSpace(string(out))}, nil
}

// Root is the repository's top-level directory.
func (r Repo) Root() string { return r.root }

// Resolve returns the full ID of the commit that rev names.
func (r Repo) Resolve(ctx context.Context, rev string) (string, error) {
	out, err := git(ctx, r.root, nil, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%q does not name a commit", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeBase returns the best common ancestor of two commits.
func (r Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := git(ctx, r.root, nil, nil, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("%s and %s have no common ancestor: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// AddedOrModified returns the files that are added or modified from base to
// head. A renamed file counts as added.
func (r Repo) AddedOrModified(ctx context.Context, base, head string) ([]string, error) {
	out, err := git(ctx, r.root, nil, nil, "diff", "--name-only", "--no-renames", "--diff-filter=AM", "-z", base, head, "--")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// Files returns the paths of the files in dir, and in its subdirectories, in
// commit. dir is relative to the repository root.
func (r Repo) Files(ctx context.Context, commit, dir string) ([]string, error) {
	out, err := git(ctx, r.root, nil, nil, "ls-tree", "-r", "-z", "--name-only", commit, "--", dir+"/")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// ReadFile returns the content of file in commit. It reports false when the
// commit has no such file, and fails when the file is larger than max bytes.
func (r Repo) ReadFile(ctx context.Context, commit, file string, max int64) ([]byte, bool, error) {
	entry, err := git(ctx, r.root, nil, nil, "ls-tree", "-z", commit, "--", file)
	if err != nil {
		return nil, false, err
	}
	if len(entry) == 0 {
		return nil, false, nil
	}
	// The entry is "<mode> <type> <object>\t<path>".
	fields := strings.Fields(strings.SplitN(string(entry), "\t", 2)[0])
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, false, fmt.Errorf("%s in commit %s is not a file", file, short(commit))
	}
	object := fields[2]
	sizeText, err := git(ctx, r.root, nil, nil, "cat-file", "-s", object)
	if err != nil {
		return nil, false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeText)), 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("git reported the size of %s as %q", object, sizeText)
	}
	if size > max {
		return nil, false, fmt.Errorf("%s in commit %s has %d bytes; the limit is %d", file, short(commit), size, max)
	}
	data, err := git(ctx, r.root, nil, nil, "cat-file", "blob", object)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Checkout writes the files of commit for which keep returns true into dest,
// as a checkout of the commit writes them. Submodules are left out. dest must
// exist. scratch is a directory for the temporary index.
func (r Repo) Checkout(ctx context.Context, commit, dest, scratch string, keep func(file string) bool) error {
	index, err := os.CreateTemp(scratch, "index-")
	if err != nil {
		return err
	}
	index.Close()
	defer os.Remove(index.Name())
	env := []string{"GIT_INDEX_FILE=" + index.Name()}

	if _, err := git(ctx, r.root, env, nil, "read-tree", commit); err != nil {
		return err
	}
	out, err := git(ctx, r.root, env, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	var files bytes.Buffer
	for _, entry := range splitNUL(out) {
		// An entry is "<mode> <object> <stage>\t<path>".
		meta, file, ok := strings.Cut(entry, "\t")
		if !ok {
			return fmt.Errorf("git ls-files printed an entry without a path: %q", entry)
		}
		if strings.HasPrefix(meta, "160000 ") || !keep(file) {
			continue
		}
		files.WriteString(file)
		files.WriteByte(0)
	}
	if files.Len() == 0 {
		return nil
	}
	prefix, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	_, err = git(ctx, r.root, env, &files, "checkout-index", "--force", "-z", "--stdin", "--prefix="+prefix+string(filepath.Separator))
	return err
}

func git(ctx context.Context, dir string, env []string, stdin *bytes.Buffer, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	inOwnGroup(cmd)
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

func splitNUL(out []byte) []string {
	text := strings.TrimSuffix(string(out), "\x00")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\x00")
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
