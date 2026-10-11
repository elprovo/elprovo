package github

import (
	"fmt"
	"slices"
	"time"
)

const (
	eventPullRequest  = "pull_request"
	eventIssueComment = "issue_comment"
	eventPush         = "push"
)

var supportedEvents = []string{eventPullRequest, eventIssueComment, eventPush}

type installationRef struct {
	ID int64 `json:"id"`
}

type changeFrom struct {
	From string `json:"from"`
}

type editChanges struct {
	Title *changeFrom `json:"title,omitempty"`
	Body  *changeFrom `json:"body,omitempty"`
}

type pullRequestEvent struct {
	Action       string          `json:"action"`
	Number       int             `json:"number"`
	PullRequest  pullJSON        `json:"pull_request"`
	Label        *labelJSON      `json:"label,omitempty"`
	Changes      *editChanges    `json:"changes,omitempty"`
	Before       string          `json:"before,omitempty"`
	After        string          `json:"after,omitempty"`
	Repository   repoJSON        `json:"repository"`
	Organization *orgJSON        `json:"organization,omitempty"`
	Installation installationRef `json:"installation"`
	Sender       userJSON        `json:"sender"`
}

type issueCommentEvent struct {
	Action       string          `json:"action"`
	Issue        issueJSON       `json:"issue"`
	Comment      commentJSON     `json:"comment"`
	Changes      *editChanges    `json:"changes,omitempty"`
	Repository   repoJSON        `json:"repository"`
	Organization *orgJSON        `json:"organization,omitempty"`
	Installation installationRef `json:"installation"`
	Sender       userJSON        `json:"sender"`
}

type installationEvent struct {
	Action       string           `json:"action"`
	Installation installationJSON `json:"installation"`
	Repositories []repoRefJSON    `json:"repositories,omitempty"`
	Requester    *userJSON        `json:"requester"`
	Sender       userJSON         `json:"sender"`
}

type installationReposEvent struct {
	Action              string           `json:"action"`
	Installation        installationJSON `json:"installation"`
	RepositorySelection string           `json:"repository_selection"`
	RepositoriesAdded   []repoRefJSON    `json:"repositories_added"`
	RepositoriesRemoved []repoRefJSON    `json:"repositories_removed"`
	Requester           *userJSON        `json:"requester"`
	Sender              userJSON         `json:"sender"`
}

type pingHookConfig struct {
	ContentType string `json:"content_type"`
	InsecureSSL string `json:"insecure_ssl"`
	URL         string `json:"url"`
}

type pingHook struct {
	Type          string         `json:"type"`
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Active        bool           `json:"active"`
	Events        []string       `json:"events"`
	Config        pingHookConfig `json:"config"`
	UpdatedAt     time.Time      `json:"updated_at"`
	CreatedAt     time.Time      `json:"created_at"`
	AppID         int64          `json:"app_id"`
	DeliveriesURL string         `json:"deliveries_url"`
}

type pingEvent struct {
	Zen    string   `json:"zen"`
	HookID int64    `json:"hook_id"`
	Hook   pingHook `json:"hook"`
}

func (t *tx) emitRepoEvent(r *repo, event, action string, payload func(installationRef) any) {
	for _, inst := range t.w.Installations {
		if !inst.covers(r) || !slices.Contains(inst.Events, event) {
			continue
		}
		if a := t.w.app(inst.AppID); a != nil {
			t.recordDelivery(a, inst.ID, event, action, payload(installationRef{ID: inst.ID}))
		}
	}
}

func (t *tx) emitPullRequest(r *repo, p *pull, action string, senderID int64, fill func(*pullRequestEvent)) {
	t.emitRepoEvent(r, eventPullRequest, action, func(ref installationRef) any {
		e := pullRequestEvent{
			Action:       action,
			Number:       p.Number,
			PullRequest:  t.pull(r, p),
			Repository:   t.repo(r),
			Organization: t.org(r.OwnerID),
			Installation: ref,
			Sender:       t.user(senderID),
		}
		if fill != nil {
			fill(&e)
		}
		return e
	})
}

func (t *tx) emitIssueComment(r *repo, p *pull, c *comment, action string, senderID int64, changes *editChanges) {
	t.emitRepoEvent(r, eventIssueComment, action, func(ref installationRef) any {
		return issueCommentEvent{
			Action:       action,
			Issue:        t.issue(r, p),
			Comment:      t.comment(r, c),
			Changes:      changes,
			Repository:   t.repo(r),
			Organization: t.org(r.OwnerID),
			Installation: ref,
			Sender:       t.user(senderID),
		}
	})
}

func (t *tx) emitInstallation(inst *installation, action string, senderID int64) {
	var repos []repoRefJSON
	for _, r := range t.w.installationRepos(inst) {
		repos = append(repos, t.repoRef(r))
	}
	t.recordDelivery(t.w.app(inst.AppID), inst.ID, "installation", action, installationEvent{
		Action:       action,
		Installation: t.installation(inst),
		Repositories: repos,
		Sender:       t.user(senderID),
	})
}

func (t *tx) emitInstallationRepos(inst *installation, added, removed []*repo, senderID int64) {
	refs := func(rs []*repo) []repoRefJSON {
		out := []repoRefJSON{}
		for _, r := range rs {
			out = append(out, t.repoRef(r))
		}
		return out
	}
	emit := func(action string, added, removed []*repo) {
		t.recordDelivery(t.w.app(inst.AppID), inst.ID, "installation_repositories", action, installationReposEvent{
			Action:              action,
			Installation:        t.installation(inst),
			RepositorySelection: inst.RepositorySelection,
			RepositoriesAdded:   refs(added),
			RepositoriesRemoved: refs(removed),
			Sender:              t.user(senderID),
		})
	}
	if len(added) > 0 {
		emit("added", added, nil)
	}
	if len(removed) > 0 {
		emit("removed", nil, removed)
	}
}

func (t *tx) emitPing(a *app) {
	t.recordDelivery(a, 0, "ping", "", pingEvent{
		Zen:    "Keep it logically awesome.",
		HookID: a.HookID,
		Hook: pingHook{
			Type:          "App",
			ID:            a.HookID,
			Name:          "web",
			Active:        true,
			Events:        orEmpty(a.Events),
			Config:        pingHookConfig{ContentType: "json", InsecureSSL: "0", URL: a.WebhookURL},
			UpdatedAt:     a.UpdatedAt,
			CreatedAt:     a.CreatedAt,
			AppID:         a.ID,
			DeliveriesURL: fmt.Sprintf("%s/app/hook/deliveries", t.api),
		},
	})
}
