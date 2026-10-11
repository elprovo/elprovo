package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/stagely-dev/stagely/sim/internal/github/gitrepo"
)

type newRepo struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	SourceDir     string `json:"source_dir"`
	// AutoInit commits a README when SourceDir is empty, as GitHub's
	// auto_init does; otherwise the repository starts empty.
	AutoInit bool `json:"auto_init"`
}

func (s *Sim) createRepo(ctx context.Context, in newRepo) (repoJSON, error) {
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()

	owner, sig, err := s.checkNewRepo(in)
	if err != nil {
		return repoJSON{}, err
	}
	dir := s.repoDir(owner, in.Name)
	if err := s.initRepo(ctx, dir, in, sig); err != nil {
		_ = os.RemoveAll(dir)
		return repoJSON{}, err
	}
	var out repoJSON
	err = s.update(func(t *tx) error {
		a := t.w.accountByLogin(owner)
		at := now()
		r := &repo{
			ID: t.w.newID(), OwnerID: a.ID, Name: in.Name, Private: in.Private, DefaultBranch: in.DefaultBranch,
			NextNumber: 1, CreatedAt: at, UpdatedAt: at, PushedAt: at,
		}
		t.w.Repos = append(t.w.Repos, r)
		out = t.repo(r)
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(dir)
	}
	return out, err
}

func (s *Sim) checkNewRepo(in newRepo) (string, gitrepo.Signature, error) {
	var owner string
	var sig gitrepo.Signature
	var err error
	s.read(func(v view) {
		a := v.w.accountByLogin(in.Owner)
		switch {
		case a == nil || a.Type == typeBot:
			err = fail(http.StatusNotFound, "owner %s does not exist", in.Owner)
		case !validRepoName(in.Name):
			err = errValidation("Repository", "name", "invalid")
		case v.w.repo(in.Owner, in.Name) != nil:
			err = &apiError{status: http.StatusUnprocessableEntity, message: "Repository creation failed.", errors: []fieldError{
				{Resource: "Repository", Field: "name", Code: "custom", Message: "name already exists on this account"},
			}}
		default:
			owner = a.Login
			sig = signature(a)
		}
	})
	return owner, sig, err
}

func (s *Sim) initRepo(ctx context.Context, dir string, in newRepo, sig gitrepo.Signature) error {
	g, err := gitrepo.Init(ctx, dir, in.DefaultBranch)
	if err != nil {
		return err
	}
	if in.SourceDir == "" && in.AutoInit {
		return seedReadme(ctx, g, in, sig)
	}
	if in.SourceDir == "" {
		return nil
	}
	if info, err := os.Stat(in.SourceDir); err != nil || !info.IsDir() {
		return fail(http.StatusUnprocessableEntity, "source_dir %s is not a directory", in.SourceDir)
	}
	if _, err := g.SeedFromDir(ctx, in.SourceDir, in.DefaultBranch, "Initial commit", sig); err != nil {
		return fmt.Errorf("seed repository: %w", err)
	}
	return nil
}

func seedReadme(ctx context.Context, g *gitrepo.Repo, in newRepo, sig gitrepo.Signature) error {
	src, err := os.MkdirTemp("", "readme-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# "+in.Name+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := g.SeedFromDir(ctx, src, in.DefaultBranch, "Initial commit", sig); err != nil {
		return fmt.Errorf("seed repository: %w", err)
	}
	return nil
}

func signature(a *account) gitrepo.Signature {
	name, email := a.Name, a.Email
	if name == "" {
		name = a.Login
	}
	if email == "" {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", a.ID, a.Login)
	}
	return gitrepo.Signature{Name: name, Email: email, When: now()}
}

func (s *Sim) gitRepo(owner, name string) *gitrepo.Repo { return gitrepo.Open(s.repoDir(owner, name)) }

func isNotFound(err error) bool { return errors.Is(err, gitrepo.ErrNotFound) }
