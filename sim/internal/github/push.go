package github

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/github/gitrepo"
)

const maxPushCommits = 2048

type pushedCommit struct {
	commit  gitrepo.Commit
	changes gitrepo.Changes
}

type refUpdate struct {
	ref, before, after string
	forced             bool
	commits            []pushedCommit
	known              *pushedCommit
}

func (u refUpdate) created() bool { return u.before == gitrepo.ZeroSHA }
func (u refUpdate) deleted() bool { return u.after == gitrepo.ZeroSHA }

func collectRefUpdates(ctx context.Context, g *gitrepo.Repo, before, after map[string]string) ([]refUpdate, error) {
	names := slices.Sorted(maps.Keys(after))
	for name := range before {
		if _, ok := after[name]; !ok {
			names = append(names, name)
		}
	}
	var updates []refUpdate
	for _, name := range names {
		if before[name] == after[name] {
			continue
		}
		var others []string
		for other, sha := range before {
			if other != name {
				others = append(others, sha)
			}
		}
		u, err := describeRefUpdate(ctx, g, name, orZero(before[name]), orZero(after[name]), others)
		if err != nil {
			return nil, err
		}
		updates = append(updates, u)
	}
	return updates, nil
}

func orZero(sha string) string {
	if sha == "" {
		return gitrepo.ZeroSHA
	}
	return sha
}

func describeRefUpdate(ctx context.Context, g *gitrepo.Repo, ref, before, after string, others []string) (refUpdate, error) {
	u := refUpdate{ref: ref, before: before, after: after}
	if u.deleted() {
		return u, nil
	}
	exclude := others
	if !u.created() {
		exclude = []string{before}
		ff, err := g.IsAncestor(ctx, before, after)
		if err != nil {
			return u, err
		}
		u.forced = !ff
	}
	commits, err := g.NewCommits(ctx, after, exclude, maxPushCommits)
	if err != nil {
		return u, err
	}
	for _, c := range commits {
		pc, err := describeCommit(ctx, g, c)
		if err != nil {
			return u, err
		}
		u.commits = append(u.commits, pc)
	}
	if len(u.commits) > 0 {
		return u, nil
	}
	c, err := g.ReadCommit(ctx, after)
	if err != nil {
		return u, err
	}
	known, err := describeCommit(ctx, g, c)
	u.known = &known
	return u, err
}

func describeCommit(ctx context.Context, g *gitrepo.Repo, c gitrepo.Commit) (pushedCommit, error) {
	changes, err := g.ChangedFiles(ctx, c.SHA)
	return pushedCommit{commit: c, changes: changes}, err
}

type pushPersonJSON struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username,omitempty"`
}

type pushCommitJSON struct {
	ID        string         `json:"id"`
	TreeID    string         `json:"tree_id"`
	Distinct  bool           `json:"distinct"`
	Message   string         `json:"message"`
	Timestamp time.Time      `json:"timestamp"`
	URL       string         `json:"url"`
	Author    pushPersonJSON `json:"author"`
	Committer pushPersonJSON `json:"committer"`
	Added     []string       `json:"added"`
	Removed   []string       `json:"removed"`
	Modified  []string       `json:"modified"`
}

type pushRepoJSON struct {
	repoJSON
	CreatedAt    int64  `json:"created_at"`
	PushedAt     int64  `json:"pushed_at"`
	MasterBranch string `json:"master_branch"`
}

type pushEvent struct {
	Ref          string           `json:"ref"`
	Before       string           `json:"before"`
	After        string           `json:"after"`
	Repository   pushRepoJSON     `json:"repository"`
	Pusher       pushPersonJSON   `json:"pusher"`
	Organization *orgJSON         `json:"organization,omitempty"`
	Installation installationRef  `json:"installation"`
	Sender       userJSON         `json:"sender"`
	Created      bool             `json:"created"`
	Deleted      bool             `json:"deleted"`
	Forced       bool             `json:"forced"`
	BaseRef      *string          `json:"base_ref"`
	Compare      string           `json:"compare"`
	Commits      []pushCommitJSON `json:"commits"`
	HeadCommit   *pushCommitJSON  `json:"head_commit"`
}

func (t *tx) recordPush(r *repo, pusherID int64, updates []refUpdate) {
	at := now()
	r.PushedAt, r.UpdatedAt = at, at
	for _, u := range updates {
		t.emitPush(r, pusherID, u)
		branch, ok := strings.CutPrefix(u.ref, "refs/heads/")
		if !ok || u.deleted() {
			continue
		}
		for _, p := range r.Pulls {
			if p.State != stateOpen || p.HeadRef != branch || p.HeadSHA == u.after {
				continue
			}
			before := p.HeadSHA
			p.HeadSHA, p.UpdatedAt = u.after, at
			t.emitPullRequest(r, p, "synchronize", pusherID, func(e *pullRequestEvent) {
				e.Before, e.After = before, u.after
			})
		}
	}
}

func (t *tx) emitPush(r *repo, pusherID int64, u refUpdate) {
	repoJ := t.repo(r)
	compare := repoJ.HTMLURL + "/compare/" + u.before[:12] + "..." + u.after[:12]
	if u.created() {
		compare = repoJ.HTMLURL + "/commit/" + u.after[:12]
	}
	commits := []pushCommitJSON{}
	for _, c := range u.commits {
		commits = append(commits, t.pushCommit(repoJ.HTMLURL, c))
	}
	var head *pushCommitJSON
	switch {
	case len(commits) > 0:
		head = &commits[len(commits)-1]
	case u.known != nil:
		known := t.pushCommit(repoJ.HTMLURL, *u.known)
		known.Distinct = false
		head = &known
	}
	pusher := t.w.account(pusherID)
	t.emitRepoEvent(r, eventPush, "", func(ref installationRef) any {
		return pushEvent{
			Ref: u.ref, Before: u.before, After: u.after,
			Repository: pushRepoJSON{
				repoJSON: repoJ, CreatedAt: r.CreatedAt.Unix(), PushedAt: r.PushedAt.Unix(), MasterBranch: r.DefaultBranch,
			},
			Pusher:       pushPersonJSON{Name: pusher.Login, Email: signature(pusher).Email},
			Organization: t.org(r.OwnerID),
			Installation: ref,
			Sender:       t.user(pusherID),
			Created:      u.created(),
			Deleted:      u.deleted(),
			Forced:       u.forced,
			Compare:      compare,
			Commits:      commits,
			HeadCommit:   head,
		}
	})
}

func (t *tx) pushCommit(repoHTML string, c pushedCommit) pushCommitJSON {
	person := func(p gitrepo.Person) pushPersonJSON {
		out := pushPersonJSON{Name: p.Name, Email: p.Email}
		if u := t.userByEmail(p.Email); u != nil {
			out.Username = u.Login
		}
		return out
	}
	return pushCommitJSON{
		ID:        c.commit.SHA,
		TreeID:    c.commit.Tree,
		Distinct:  true,
		Message:   c.commit.Message,
		Timestamp: c.commit.Author.When,
		URL:       repoHTML + "/commit/" + c.commit.SHA,
		Author:    person(c.commit.Author),
		Committer: person(c.commit.Committer),
		Added:     orEmpty(c.changes.Added),
		Removed:   orEmpty(c.changes.Removed),
		Modified:  orEmpty(c.changes.Modified),
	}
}
