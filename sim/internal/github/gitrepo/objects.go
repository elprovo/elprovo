package gitrepo

import (
	"bytes"
	"context"
	"path"
	"strconv"
	"strings"
	"time"
)

// Entry is one item of a tree.
type Entry struct {
	Mode string // 100644, 100755, 120000 (symlink), 040000 (tree) or 160000 (submodule)
	Type string // blob, tree or commit
	SHA  string
	Size int64 // zero for trees and submodules
	Path string
}

// Name returns the last element of the entry's path.
func (e Entry) Name() string { return path.Base(e.Path) }

// Stat returns the entry at p in commit. An empty p is the root tree.
func (r *Repo) Stat(ctx context.Context, commit, p string) (Entry, error) {
	if p == "" {
		tree, err := r.git(ctx, nil, "rev-parse", commit+"^{tree}")
		if err != nil {
			return Entry{}, err
		}
		return Entry{Mode: "040000", Type: "tree", SHA: line(tree)}, nil
	}
	dir, name := path.Split(p)
	entries, err := r.ListTree(ctx, commit, strings.TrimSuffix(dir, "/"))
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Name() == name {
			return e, nil
		}
	}
	return Entry{}, ErrNotFound
}

// ListTree returns the entries of the directory dir in commit.
func (r *Repo) ListTree(ctx context.Context, commit, dir string) ([]Entry, error) {
	spec := commit + ":" + dir
	if typ, err := r.objectType(ctx, spec); err != nil || typ != "tree" {
		return nil, ErrNotFound
	}
	out, err := r.git(ctx, nil, "ls-tree", "--long", "-z", spec)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, rec := range bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		if e, ok := parseTreeRecord(string(rec), dir); ok {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// ReadBlob returns the content of the blob sha.
func (r *Repo) ReadBlob(ctx context.Context, sha string) ([]byte, error) {
	return r.git(ctx, nil, "cat-file", "blob", sha)
}

func (r *Repo) objectType(ctx context.Context, spec string) (string, error) {
	out, err := r.git(ctx, nil, "cat-file", "-t", spec)
	if err != nil {
		return "", ErrNotFound
	}
	return line(out), nil
}

func parseTreeRecord(rec, dir string) (Entry, bool) {
	meta, name, ok := strings.Cut(rec, "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 4 {
		return Entry{}, false
	}
	size, _ := strconv.ParseInt(fields[3], 10, 64)
	return Entry{Mode: fields[0], Type: fields[1], SHA: fields[2], Size: size, Path: path.Join(dir, name)}, true
}

// Person is the author or committer of a commit.
type Person struct {
	Name  string
	Email string
	When  time.Time
}

// Commit describes one commit.
type Commit struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    Person
	Committer Person
	Message   string
}

const commitFormat = "%H%x00%T%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%B"

// ReadCommit returns the commit sha.
func (r *Repo) ReadCommit(ctx context.Context, sha string) (Commit, error) {
	out, err := r.git(ctx, nil, "log", "-1", "--format="+commitFormat, "--end-of-options", sha)
	if err != nil {
		return Commit{}, err
	}
	f := strings.SplitN(string(out), "\x00", 10)
	if len(f) != 10 {
		return Commit{}, ErrNotFound
	}
	return Commit{
		SHA:       f[0],
		Tree:      f[1],
		Parents:   strings.Fields(f[2]),
		Author:    Person{Name: f[3], Email: f[4], When: parseTime(f[5])},
		Committer: Person{Name: f[6], Email: f[7], When: parseTime(f[8])},
		Message:   strings.TrimRight(f[9], "\n"),
	}, nil
}

// NewCommits returns up to limit commits, oldest first, that are reachable
// from after but from none of exclude.
func (r *Repo) NewCommits(ctx context.Context, after string, exclude []string, limit int) ([]Commit, error) {
	args := []string{"rev-list", "--reverse", "--max-count=" + strconv.Itoa(limit), after}
	if len(exclude) > 0 {
		args = append(append(args, "--not"), exclude...)
	}
	out, err := r.git(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, sha := range strings.Fields(string(out)) {
		c, err := r.ReadCommit(ctx, sha)
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// Changes lists the paths a commit added, removed and modified compared
// with its first parent.
type Changes struct {
	Added, Removed, Modified []string
}

// ChangedFiles returns the changes that commit sha made.
func (r *Repo) ChangedFiles(ctx context.Context, sha string) (Changes, error) {
	out, err := r.git(ctx, nil, "diff-tree", "-r", "--root", "--diff-merges=first-parent", "--no-commit-id", "--no-renames", "--name-status", "-z", sha)
	if err != nil {
		return Changes{}, err
	}
	var c Changes
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "A":
			c.Added = append(c.Added, fields[i+1])
		case "D":
			c.Removed = append(c.Removed, fields[i+1])
		default:
			c.Modified = append(c.Modified, fields[i+1])
		}
	}
	return c, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t.UTC()
}
