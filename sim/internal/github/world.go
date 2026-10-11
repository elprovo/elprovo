package github

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

type world struct {
	NextID         int64           `json:"next_id"`
	Accounts       []*account      `json:"accounts"`
	Repos          []*repo         `json:"repos"`
	Apps           []*app          `json:"apps"`
	Installations  []*installation `json:"installations"`
	Authorizations []authorization `json:"authorizations"`
	Tokens         []*token        `json:"tokens"`
	Codes          []*authCode     `json:"codes"`
	Sessions       []session       `json:"sessions"`
	Deliveries     []*delivery     `json:"deliveries"`
}

const (
	typeUser = "User"
	typeOrg  = "Organization"
	typeBot  = "Bot"
)

type account struct {
	ID        int64        `json:"id"`
	Login     string       `json:"login"`
	Name      string       `json:"name"`
	Email     string       `json:"email"`
	Type      string       `json:"type"`
	Members   []membership `json:"members,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

const (
	roleAdmin  = "admin"
	roleMember = "member"
)

type membership struct {
	UserID int64  `json:"user_id"`
	Role   string `json:"role"`
}

type repo struct {
	ID            int64          `json:"id"`
	OwnerID       int64          `json:"owner_id"`
	Name          string         `json:"name"`
	Private       bool           `json:"private"`
	DefaultBranch string         `json:"default_branch"`
	Collaborators []collaborator `json:"collaborators,omitempty"`
	Labels        []*label       `json:"labels,omitempty"`
	Pulls         []*pull        `json:"pulls,omitempty"`
	Comments      []*comment     `json:"comments,omitempty"`
	Statuses      []*status      `json:"statuses,omitempty"`
	Deployments   []*deployment  `json:"deployments,omitempty"`
	NextNumber    int            `json:"next_number"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	PushedAt      time.Time      `json:"pushed_at"`
}

type collaborator struct {
	UserID     int64  `json:"user_id"`
	Permission string `json:"permission"`
}

type label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

const (
	stateOpen   = "open"
	stateClosed = "closed"
)

type pull struct {
	ID             int64      `json:"id"`
	IssueID        int64      `json:"issue_id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	UserID         int64      `json:"user_id"`
	HeadRef        string     `json:"head_ref"`
	HeadSHA        string     `json:"head_sha"`
	BaseRef        string     `json:"base_ref"`
	BaseSHA        string     `json:"base_sha"`
	Labels         []string   `json:"labels,omitempty"`
	Merged         bool       `json:"merged"`
	MergedByID     int64      `json:"merged_by_id,omitempty"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type comment struct {
	ID          int64     `json:"id"`
	IssueNumber int       `json:"issue_number"`
	UserID      int64     `json:"user_id"`
	AppID       int64     `json:"app_id,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type status struct {
	ID          int64     `json:"id"`
	SHA         string    `json:"sha"`
	State       string    `json:"state"`
	TargetURL   string    `json:"target_url"`
	Description string    `json:"description"`
	Context     string    `json:"context"`
	CreatorID   int64     `json:"creator_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type deployment struct {
	ID          int64               `json:"id"`
	SHA         string              `json:"sha"`
	Ref         string              `json:"ref"`
	Task        string              `json:"task"`
	Payload     json.RawMessage     `json:"payload"`
	Environment string              `json:"environment"`
	Description string              `json:"description"`
	Transient   bool                `json:"transient"`
	Production  bool                `json:"production"`
	CreatorID   int64               `json:"creator_id"`
	AppID       int64               `json:"app_id,omitempty"`
	Statuses    []*deploymentStatus `json:"statuses,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
}

type deploymentStatus struct {
	ID             int64     `json:"id"`
	State          string    `json:"state"`
	TargetURL      string    `json:"target_url"`
	LogURL         string    `json:"log_url"`
	Description    string    `json:"description"`
	Environment    string    `json:"environment"`
	EnvironmentURL string    `json:"environment_url"`
	CreatorID      int64     `json:"creator_id"`
	AppID          int64     `json:"app_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type app struct {
	ID            int64             `json:"id"`
	Slug          string            `json:"slug"`
	Name          string            `json:"name"`
	OwnerID       int64             `json:"owner_id"`
	BotID         int64             `json:"bot_id"`
	ClientID      string            `json:"client_id"`
	ClientSecret  string            `json:"client_secret"`
	WebhookURL    string            `json:"webhook_url"`
	WebhookSecret string            `json:"webhook_secret"`
	HookID        int64             `json:"hook_id"`
	PrivateKeyPEM string            `json:"private_key_pem"`
	CallbackURLs  []string          `json:"callback_urls"`
	SetupURL      string            `json:"setup_url"`
	Permissions   map[string]string `json:"permissions"`
	Events        []string          `json:"events"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

const (
	selectionAll      = "all"
	selectionSelected = "selected"
)

type installation struct {
	ID                  int64             `json:"id"`
	AppID               int64             `json:"app_id"`
	AccountID           int64             `json:"account_id"`
	RepositorySelection string            `json:"repository_selection"`
	RepoIDs             []int64           `json:"repo_ids,omitempty"`
	Permissions         map[string]string `json:"permissions"`
	Events              []string          `json:"events"`
	SuspendedAt         *time.Time        `json:"suspended_at,omitempty"`
	SuspendedByID       int64             `json:"suspended_by_id,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type session struct {
	ID     string `json:"id"`
	UserID int64  `json:"user_id"`
}

type authorization struct {
	UserID    int64     `json:"user_id"`
	AppID     int64     `json:"app_id"`
	CreatedAt time.Time `json:"created_at"`
}

type authCode struct {
	Code        string    `json:"code"`
	AppID       int64     `json:"app_id"`
	UserID      int64     `json:"user_id"`
	RedirectURI string    `json:"redirect_uri"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (w *world) newID() int64 {
	w.NextID++
	return w.NextID
}

func (w *world) account(id int64) *account {
	for _, a := range w.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (w *world) accountByLogin(login string) *account {
	for _, a := range w.Accounts {
		if strings.EqualFold(a.Login, login) {
			return a
		}
	}
	return nil
}

func (w *world) user(login string) *account {
	if a := w.accountByLogin(login); a != nil && a.Type == typeUser {
		return a
	}
	return nil
}

func (w *world) repo(owner, name string) *repo {
	a := w.accountByLogin(owner)
	if a == nil {
		return nil
	}
	for _, r := range w.Repos {
		if r.OwnerID == a.ID && strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

func (w *world) repoByID(id int64) *repo {
	for _, r := range w.Repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (w *world) app(id int64) *app {
	for _, a := range w.Apps {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (w *world) appBySlug(slug string) *app {
	for _, a := range w.Apps {
		if strings.EqualFold(a.Slug, slug) {
			return a
		}
	}
	return nil
}

func (w *world) appByClientID(clientID string) *app {
	for _, a := range w.Apps {
		if a.ClientID == clientID {
			return a
		}
	}
	return nil
}

func (w *world) installation(id int64) *installation {
	for _, i := range w.Installations {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func (w *world) installationFor(appID, accountID int64) *installation {
	for _, i := range w.Installations {
		if i.AppID == appID && i.AccountID == accountID {
			return i
		}
	}
	return nil
}

func (w *world) authorized(userID, appID int64) bool {
	return slices.ContainsFunc(w.Authorizations, func(a authorization) bool {
		return a.UserID == userID && a.AppID == appID
	})
}

func (r *repo) pull(number int) *pull {
	for _, p := range r.Pulls {
		if p.Number == number {
			return p
		}
	}
	return nil
}

func (r *repo) comment(id int64) *comment {
	for _, c := range r.Comments {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (r *repo) label(name string) *label {
	for _, l := range r.Labels {
		if strings.EqualFold(l.Name, name) {
			return l
		}
	}
	return nil
}

func (r *repo) deployment(id int64) *deployment {
	for _, d := range r.Deployments {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (a *account) role(userID int64) string {
	for _, m := range a.Members {
		if m.UserID == userID {
			return m.Role
		}
	}
	return ""
}

func (i *installation) covers(r *repo) bool {
	if i.SuspendedAt != nil || i.AccountID != r.OwnerID {
		return false
	}
	return i.RepositorySelection == selectionAll || slices.Contains(i.RepoIDs, r.ID)
}

func (w *world) installationRepos(i *installation) []*repo {
	var out []*repo
	for _, r := range w.Repos {
		if i.covers(r) {
			out = append(out, r)
		}
	}
	return out
}
