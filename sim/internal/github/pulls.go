package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/stagely-dev/stagely/sim/internal/github/gitrepo"
)

const defaultLabelColor = "ededed"

type pullDraft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

func (s *Sim) createPull(ctx context.Context, owner, name string, userID int64, d pullDraft) (pullJSON, error) {
	var defaultBranch string
	var err error
	s.read(func(v view) {
		rp := v.w.repo(owner, name)
		switch {
		case rp == nil:
			err = errNotFound()
		case strings.TrimSpace(d.Title) == "":
			err = errValidation("PullRequest", "title", "missing_field")
		default:
			owner, name, defaultBranch = v.w.account(rp.OwnerID).Login, rp.Name, rp.DefaultBranch
		}
	})
	if err != nil {
		return pullJSON{}, err
	}
	d.Base = cmp.Or(d.Base, defaultBranch)
	headSHA, baseSHA, err := s.comparableBranches(ctx, s.gitRepo(owner, name), d.Head, d.Base)
	if err != nil {
		return pullJSON{}, err
	}
	var out pullJSON
	err = s.update(func(t *tx) error {
		rp := t.w.repo(owner, name)
		if rp == nil {
			return errNotFound()
		}
		for _, p := range rp.Pulls {
			if p.State == stateOpen && p.HeadRef == d.Head && p.BaseRef == d.Base {
				return fail(http.StatusUnprocessableEntity, "A pull request already exists for %s:%s.", owner, d.Head)
			}
		}
		out = t.pull(rp, t.openPull(rp, userID, d, headSHA, baseSHA))
		return nil
	})
	return out, err
}

func (s *Sim) comparableBranches(ctx context.Context, g *gitrepo.Repo, head, base string) (string, string, error) {
	headSHA, err := g.ResolveCommit(ctx, "refs/heads/"+head)
	if isNotFound(err) || head == "" {
		return "", "", errValidation("PullRequest", "head", "invalid")
	}
	if err != nil {
		return "", "", err
	}
	baseSHA, err := g.ResolveCommit(ctx, "refs/heads/"+base)
	if isNotFound(err) {
		return "", "", errValidation("PullRequest", "base", "invalid")
	}
	if err != nil {
		return "", "", err
	}
	ahead, err := g.CountCommits(ctx, baseSHA, headSHA)
	if err != nil {
		return "", "", err
	}
	if head == base || ahead == 0 {
		return "", "", fail(http.StatusUnprocessableEntity, "No commits between %s and %s", base, head)
	}
	return headSHA, baseSHA, nil
}

func (t *tx) openPull(r *repo, userID int64, d pullDraft, headSHA, baseSHA string) *pull {
	at := now()
	p := &pull{
		ID: t.w.newID(), IssueID: t.w.newID(), Number: r.NextNumber, Title: d.Title, Body: d.Body, State: stateOpen,
		UserID: userID, HeadRef: d.Head, HeadSHA: headSHA, BaseRef: d.Base, BaseSHA: baseSHA,
		CreatedAt: at, UpdatedAt: at,
	}
	r.NextNumber++
	r.Pulls = append(r.Pulls, p)
	t.emitPullRequest(r, p, "opened", userID, nil)
	return p
}

func checkPullState(p *pull, state string) error {
	switch {
	case state != stateOpen && state != stateClosed:
		return errValidation("PullRequest", "state", "invalid")
	case state == p.State:
		return fail(http.StatusUnprocessableEntity, "Pull request #%d is already %s", p.Number, state)
	case p.Merged:
		return fail(http.StatusUnprocessableEntity, "Pull request #%d is merged and cannot be reopened", p.Number)
	}
	return nil
}

func (t *tx) setPullState(r *repo, p *pull, actorID int64, state string) error {
	if err := checkPullState(p, state); err != nil {
		return err
	}
	at := now()
	p.State, p.UpdatedAt = state, at
	action := "reopened"
	p.ClosedAt = nil
	if state == stateClosed {
		action = "closed"
		p.ClosedAt = &at
	}
	t.emitPullRequest(r, p, action, actorID, nil)
	return nil
}

func (t *tx) editPull(r *repo, p *pull, actorID int64, title, body *string) error {
	changes := &editChanges{}
	if title != nil && *title != p.Title {
		if strings.TrimSpace(*title) == "" {
			return errValidation("PullRequest", "title", "missing_field")
		}
		changes.Title = &changeFrom{From: p.Title}
		p.Title = *title
	}
	if body != nil && *body != p.Body {
		changes.Body = &changeFrom{From: p.Body}
		p.Body = *body
	}
	if changes.Title == nil && changes.Body == nil {
		return nil
	}
	p.UpdatedAt = now()
	t.emitPullRequest(r, p, "edited", actorID, func(e *pullRequestEvent) { e.Changes = changes })
	return nil
}

func (t *tx) addLabels(r *repo, p *pull, actorID int64, names []string) error {
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return errValidation("Label", "name", "missing_field")
		}
	}
	for _, n := range names {
		l := r.label(n)
		if l == nil {
			l = &label{ID: t.w.newID(), Name: strings.TrimSpace(n), Color: defaultLabelColor}
			r.Labels = append(r.Labels, l)
		}
		if slices.ContainsFunc(p.Labels, func(have string) bool { return strings.EqualFold(have, l.Name) }) {
			continue
		}
		p.Labels = append(p.Labels, l.Name)
		p.UpdatedAt = now()
		lj := t.label(r, l)
		t.emitPullRequest(r, p, "labeled", actorID, func(e *pullRequestEvent) { e.Label = &lj })
	}
	return nil
}

func (t *tx) removeLabel(r *repo, p *pull, actorID int64, name string) error {
	i := slices.IndexFunc(p.Labels, func(have string) bool { return strings.EqualFold(have, name) })
	if i < 0 {
		return fail(http.StatusNotFound, "Label does not exist")
	}
	p.Labels = slices.Delete(p.Labels, i, i+1)
	p.UpdatedAt = now()
	if l := r.label(name); l != nil {
		lj := t.label(r, l)
		t.emitPullRequest(r, p, "unlabeled", actorID, func(e *pullRequestEvent) { e.Label = &lj })
	}
	return nil
}

func (t *tx) addComment(r *repo, p *pull, userID, appID int64, body string) (*comment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errValidation("IssueComment", "body", "missing_field")
	}
	at := now()
	c := &comment{ID: t.w.newID(), IssueNumber: p.Number, UserID: userID, AppID: appID, Body: body, CreatedAt: at, UpdatedAt: at}
	r.Comments = append(r.Comments, c)
	t.emitIssueComment(r, p, c, "created", userID, nil)
	return c, nil
}

func (t *tx) editComment(r *repo, c *comment, actorID int64, body string) error {
	if strings.TrimSpace(body) == "" {
		return errValidation("IssueComment", "body", "missing_field")
	}
	changes := &editChanges{Body: &changeFrom{From: c.Body}}
	c.Body, c.UpdatedAt = body, now()
	t.emitIssueComment(r, r.pull(c.IssueNumber), c, "edited", actorID, changes)
	return nil
}

func (t *tx) deleteComment(r *repo, c *comment, actorID int64) {
	r.Comments = slices.DeleteFunc(r.Comments, func(have *comment) bool { return have.ID == c.ID })
	t.emitIssueComment(r, r.pull(c.IssueNumber), c, "deleted", actorID, nil)
}

func (s *Sim) mergePull(ctx context.Context, owner, name string, number int, userID int64) error {
	var repoID int64
	s.read(func(v view) {
		if rp := v.w.repo(owner, name); rp != nil {
			repoID = rp.ID
		}
	})
	if repoID == 0 {
		return errNotFound()
	}
	defer s.lockRepo(repoID)()

	p, owner, sig, err := s.mergeablePull(repoID, number, userID)
	if err != nil {
		return err
	}
	update, err := s.mergeIntoBase(ctx, s.gitRepo(owner, p.repoName), owner, p.pull, sig)
	if err != nil {
		return err
	}
	return s.update(func(t *tx) error {
		rp := t.w.repoByID(repoID)
		if rp == nil || rp.pull(number).State != stateOpen {
			return fail(http.StatusConflict, "Pull request #%d changed while it was merged", number)
		}
		t.markMerged(rp, rp.pull(number), userID, update.after)
		t.recordPush(rp, userID, []refUpdate{update})
		return nil
	})
}

type pullInRepo struct {
	pull
	repoName string
}

func (s *Sim) mergeablePull(repoID int64, number int, userID int64) (pullInRepo, string, gitrepo.Signature, error) {
	var p pullInRepo
	var owner string
	var sig gitrepo.Signature
	err := errNotFound()
	s.read(func(v view) {
		rp := v.w.repoByID(repoID)
		if rp == nil || rp.pull(number) == nil {
			return
		}
		p, owner, sig = pullInRepo{pull: *rp.pull(number), repoName: rp.Name}, v.w.account(rp.OwnerID).Login, signature(v.w.account(userID))
		err = nil
		if p.State != stateOpen {
			err = fail(http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		}
	})
	return p, owner, sig, err
}

func (s *Sim) mergeIntoBase(ctx context.Context, g *gitrepo.Repo, owner string, p pull, sig gitrepo.Signature) (refUpdate, error) {
	ref := "refs/heads/" + p.BaseRef
	base, err := g.ResolveCommit(ctx, ref)
	if err != nil {
		return refUpdate{}, fmt.Errorf("resolve base branch: %w", err)
	}
	message := fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s", p.Number, owner, p.HeadRef, p.Title)
	merged, err := g.Merge(ctx, base, p.HeadSHA, message, sig)
	if errors.Is(err, gitrepo.ErrConflict) {
		return refUpdate{}, fail(http.StatusMethodNotAllowed, "Pull Request is not mergeable")
	}
	if err != nil {
		return refUpdate{}, err
	}
	if err := g.UpdateRef(ctx, ref, merged, base); err != nil {
		return refUpdate{}, fmt.Errorf("move base branch: %w", err)
	}
	return describeRefUpdate(ctx, g, ref, base, merged, nil)
}

func (t *tx) markMerged(r *repo, p *pull, actorID int64, mergeSHA string) {
	at := now()
	p.State, p.Merged, p.MergedByID, p.MergeCommitSHA = stateClosed, true, actorID, mergeSHA
	p.MergedAt, p.ClosedAt, p.UpdatedAt = &at, &at, at
	t.emitPullRequest(r, p, "closed", actorID, nil)
}
