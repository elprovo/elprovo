package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type view struct {
	w   *world
	web string
	api string
}

type userJSON struct {
	Login      string `json:"login"`
	ID         int64  `json:"id"`
	AvatarURL  string `json:"avatar_url"`
	GravatarID string `json:"gravatar_id"`
	URL        string `json:"url"`
	HTMLURL    string `json:"html_url"`
	ReposURL   string `json:"repos_url"`
	Type       string `json:"type"`
	SiteAdmin  bool   `json:"site_admin"`
}

type fullUserJSON struct {
	userJSON
	Name        *string   `json:"name"`
	Company     *string   `json:"company"`
	Blog        string    `json:"blog"`
	Location    *string   `json:"location"`
	Email       *string   `json:"email"`
	Bio         *string   `json:"bio"`
	PublicRepos int       `json:"public_repos"`
	Followers   int       `json:"followers"`
	Following   int       `json:"following"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type orgJSON struct {
	Login       string `json:"login"`
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ReposURL    string `json:"repos_url"`
	AvatarURL   string `json:"avatar_url"`
	Description string `json:"description"`
}

type repoJSON struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	FullName      string         `json:"full_name"`
	Owner         userJSON       `json:"owner"`
	Private       bool           `json:"private"`
	HTMLURL       string         `json:"html_url"`
	Description   *string        `json:"description"`
	Fork          bool           `json:"fork"`
	URL           string         `json:"url"`
	CloneURL      string         `json:"clone_url"`
	DefaultBranch string         `json:"default_branch"`
	Visibility    string         `json:"visibility"`
	Archived      bool           `json:"archived"`
	Disabled      bool           `json:"disabled"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	PushedAt      time.Time      `json:"pushed_at"`
	Permissions   *repoPermsJSON `json:"permissions,omitempty"`
}

type repoPermsJSON struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

type repoRefJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

type labelJSON struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Default     bool   `json:"default"`
	Description string `json:"description"`
}

type branchJSON struct {
	Label string   `json:"label"`
	Ref   string   `json:"ref"`
	SHA   string   `json:"sha"`
	User  userJSON `json:"user"`
	Repo  repoJSON `json:"repo"`
}

type pullJSON struct {
	URL               string      `json:"url"`
	ID                int64       `json:"id"`
	HTMLURL           string      `json:"html_url"`
	DiffURL           string      `json:"diff_url"`
	PatchURL          string      `json:"patch_url"`
	IssueURL          string      `json:"issue_url"`
	CommentsURL       string      `json:"comments_url"`
	StatusesURL       string      `json:"statuses_url"`
	Number            int         `json:"number"`
	State             string      `json:"state"`
	Locked            bool        `json:"locked"`
	Title             string      `json:"title"`
	User              userJSON    `json:"user"`
	Body              *string     `json:"body"`
	Labels            []labelJSON `json:"labels"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
	ClosedAt          *time.Time  `json:"closed_at"`
	MergedAt          *time.Time  `json:"merged_at"`
	MergeCommitSHA    *string     `json:"merge_commit_sha"`
	Draft             bool        `json:"draft"`
	Head              branchJSON  `json:"head"`
	Base              branchJSON  `json:"base"`
	AuthorAssociation string      `json:"author_association"`
	Merged            bool        `json:"merged"`
	Mergeable         *bool       `json:"mergeable"`
	MergedBy          *userJSON   `json:"merged_by"`
	Comments          int         `json:"comments"`
}

type issuePullRefJSON struct {
	URL      string     `json:"url"`
	HTMLURL  string     `json:"html_url"`
	DiffURL  string     `json:"diff_url"`
	PatchURL string     `json:"patch_url"`
	MergedAt *time.Time `json:"merged_at"`
}

type issueJSON struct {
	URL               string           `json:"url"`
	RepositoryURL     string           `json:"repository_url"`
	CommentsURL       string           `json:"comments_url"`
	HTMLURL           string           `json:"html_url"`
	ID                int64            `json:"id"`
	Number            int              `json:"number"`
	Title             string           `json:"title"`
	User              userJSON         `json:"user"`
	Labels            []labelJSON      `json:"labels"`
	State             string           `json:"state"`
	Locked            bool             `json:"locked"`
	Comments          int              `json:"comments"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	ClosedAt          *time.Time       `json:"closed_at"`
	AuthorAssociation string           `json:"author_association"`
	Body              *string          `json:"body"`
	PullRequest       issuePullRefJSON `json:"pull_request"`
}

type commentJSON struct {
	ID                    int64     `json:"id"`
	URL                   string    `json:"url"`
	HTMLURL               string    `json:"html_url"`
	IssueURL              string    `json:"issue_url"`
	Body                  string    `json:"body"`
	User                  userJSON  `json:"user"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	AuthorAssociation     string    `json:"author_association"`
	PerformedViaGitHubApp *appJSON  `json:"performed_via_github_app"`
}

type appJSON struct {
	ID                 int64             `json:"id"`
	Slug               string            `json:"slug"`
	ClientID           string            `json:"client_id"`
	Owner              userJSON          `json:"owner"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	ExternalURL        string            `json:"external_url"`
	HTMLURL            string            `json:"html_url"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Permissions        map[string]string `json:"permissions"`
	Events             []string          `json:"events"`
	InstallationsCount *int              `json:"installations_count,omitempty"`
}

type installationJSON struct {
	ID                     int64             `json:"id"`
	Account                userJSON          `json:"account"`
	RepositorySelection    string            `json:"repository_selection"`
	AccessTokensURL        string            `json:"access_tokens_url"`
	RepositoriesURL        string            `json:"repositories_url"`
	HTMLURL                string            `json:"html_url"`
	AppID                  int64             `json:"app_id"`
	AppSlug                string            `json:"app_slug"`
	TargetID               int64             `json:"target_id"`
	TargetType             string            `json:"target_type"`
	Permissions            map[string]string `json:"permissions"`
	Events                 []string          `json:"events"`
	CreatedAt              time.Time         `json:"created_at"`
	UpdatedAt              time.Time         `json:"updated_at"`
	SingleFileName         *string           `json:"single_file_name"`
	HasMultipleSingleFiles bool              `json:"has_multiple_single_files"`
	SingleFilePaths        []string          `json:"single_file_paths"`
	SuspendedBy            *userJSON         `json:"suspended_by"`
	SuspendedAt            *time.Time        `json:"suspended_at"`
}

type statusJSON struct {
	URL         string    `json:"url"`
	AvatarURL   string    `json:"avatar_url"`
	ID          int64     `json:"id"`
	State       string    `json:"state"`
	Description *string   `json:"description"`
	TargetURL   *string   `json:"target_url"`
	Context     string    `json:"context"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Creator     userJSON  `json:"creator"`
}

type deploymentJSON struct {
	URL                   string          `json:"url"`
	ID                    int64           `json:"id"`
	SHA                   string          `json:"sha"`
	Ref                   string          `json:"ref"`
	Task                  string          `json:"task"`
	Payload               json.RawMessage `json:"payload"`
	OriginalEnvironment   string          `json:"original_environment"`
	Environment           string          `json:"environment"`
	Description           *string         `json:"description"`
	Creator               userJSON        `json:"creator"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	StatusesURL           string          `json:"statuses_url"`
	RepositoryURL         string          `json:"repository_url"`
	TransientEnvironment  bool            `json:"transient_environment"`
	ProductionEnvironment bool            `json:"production_environment"`
	PerformedViaGitHubApp *appJSON        `json:"performed_via_github_app"`
}

type deploymentStatusJSON struct {
	URL                   string    `json:"url"`
	ID                    int64     `json:"id"`
	State                 string    `json:"state"`
	Creator               userJSON  `json:"creator"`
	Description           string    `json:"description"`
	Environment           string    `json:"environment"`
	TargetURL             string    `json:"target_url"`
	LogURL                string    `json:"log_url"`
	EnvironmentURL        string    `json:"environment_url"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	DeploymentURL         string    `json:"deployment_url"`
	RepositoryURL         string    `json:"repository_url"`
	PerformedViaGitHubApp *appJSON  `json:"performed_via_github_app"`
}

// avatarURL is the account's avatar as a data URL. GitHub serves avatars
// over HTTPS; the simulation inlines them instead, so pages with a strict
// Content-Security-Policy show them as they show GitHub's.
func (v view) avatarURL(id int64) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(avatarSVG(v.initial(id)))
}

// initial is the first letter of the account's login, or "?".
func (v view) initial(id int64) string {
	if a := v.w.account(id); a != nil {
		return strings.ToUpper(a.Login[:1])
	}
	return "?"
}

func (v view) user(id int64) userJSON {
	a := v.w.account(id)
	if a == nil {
		return userJSON{Login: "ghost", Type: typeUser}
	}
	login := url.PathEscape(a.Login)
	html := v.web + "/" + login
	if a.Type == typeBot {
		html = v.web + "/apps/" + strings.TrimSuffix(login, "%5Bbot%5D")
	}
	return userJSON{
		Login:     a.Login,
		ID:        a.ID,
		AvatarURL: v.avatarURL(a.ID),
		URL:       v.api + "/users/" + login,
		HTMLURL:   html,
		ReposURL:  v.api + "/users/" + login + "/repos",
		Type:      a.Type,
	}
}

func (v view) fullUser(a *account, private bool) fullUserJSON {
	public := 0
	for _, r := range v.w.Repos {
		if r.OwnerID == a.ID && !r.Private {
			public++
		}
	}
	u := fullUserJSON{
		userJSON:    v.user(a.ID),
		Name:        optional(a.Name),
		PublicRepos: public,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.CreatedAt,
	}
	if private {
		u.Email = optional(a.Email)
	}
	return u
}

func (v view) org(id int64) *orgJSON {
	a := v.w.account(id)
	if a == nil || a.Type != typeOrg {
		return nil
	}
	login := url.PathEscape(a.Login)
	return &orgJSON{
		Login:     a.Login,
		ID:        a.ID,
		URL:       v.api + "/orgs/" + login,
		ReposURL:  v.api + "/orgs/" + login + "/repos",
		AvatarURL: v.avatarURL(a.ID),
	}
}

func (v view) fullName(r *repo) string {
	owner := v.w.account(r.OwnerID)
	if owner == nil {
		return r.Name
	}
	return owner.Login + "/" + r.Name
}

func (v view) repoAPI(r *repo) string  { return v.api + "/repos/" + v.fullName(r) }
func (v view) repoHTML(r *repo) string { return v.web + "/" + v.fullName(r) }

func (v view) repo(r *repo) repoJSON {
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	return repoJSON{
		ID:            r.ID,
		Name:          r.Name,
		FullName:      v.fullName(r),
		Owner:         v.user(r.OwnerID),
		Private:       r.Private,
		HTMLURL:       v.repoHTML(r),
		URL:           v.repoAPI(r),
		CloneURL:      v.repoHTML(r) + ".git",
		DefaultBranch: r.DefaultBranch,
		Visibility:    visibility,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		PushedAt:      r.PushedAt,
	}
}

func (v view) repoForUser(r *repo, userID int64) repoJSON {
	j := v.repo(r)
	role := v.w.userRole(userID, r)
	j.Permissions = &repoPermsJSON{
		Admin:    role == permAdmin,
		Maintain: role == permAdmin,
		Push:     permLevel(role) >= permLevel(permWrite),
		Triage:   permLevel(role) >= permLevel(permWrite),
		Pull:     permLevel(role) >= permLevel(permRead),
	}
	return j
}

func (v view) repoRef(r *repo) repoRefJSON {
	return repoRefJSON{ID: r.ID, Name: r.Name, FullName: v.fullName(r), Private: r.Private}
}

func (v view) label(r *repo, l *label) labelJSON {
	return labelJSON{
		ID:          l.ID,
		URL:         v.repoAPI(r) + "/labels/" + url.PathEscape(l.Name),
		Name:        l.Name,
		Color:       l.Color,
		Description: l.Description,
	}
}

func (v view) labels(r *repo, names []string) []labelJSON {
	out := []labelJSON{}
	for _, n := range names {
		if l := r.label(n); l != nil {
			out = append(out, v.label(r, l))
		}
	}
	return out
}

func (v view) commentCount(r *repo, number int) int {
	n := 0
	for _, c := range r.Comments {
		if c.IssueNumber == number {
			n++
		}
	}
	return n
}

func (v view) pull(r *repo, p *pull) pullJSON {
	api := fmt.Sprintf("%s/pulls/%d", v.repoAPI(r), p.Number)
	html := fmt.Sprintf("%s/pull/%d", v.repoHTML(r), p.Number)
	owner := v.w.account(r.OwnerID)
	branch := func(ref, sha string) branchJSON {
		return branchJSON{Label: owner.Login + ":" + ref, Ref: ref, SHA: sha, User: v.user(r.OwnerID), Repo: v.repo(r)}
	}
	j := pullJSON{
		URL:               api,
		ID:                p.ID,
		HTMLURL:           html,
		DiffURL:           html + ".diff",
		PatchURL:          html + ".patch",
		IssueURL:          fmt.Sprintf("%s/issues/%d", v.repoAPI(r), p.Number),
		CommentsURL:       fmt.Sprintf("%s/issues/%d/comments", v.repoAPI(r), p.Number),
		StatusesURL:       v.repoAPI(r) + "/statuses/" + p.HeadSHA,
		Number:            p.Number,
		State:             p.State,
		Title:             p.Title,
		User:              v.user(p.UserID),
		Body:              optional(p.Body),
		Labels:            v.labels(r, p.Labels),
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
		ClosedAt:          p.ClosedAt,
		MergedAt:          p.MergedAt,
		MergeCommitSHA:    optional(p.MergeCommitSHA),
		Head:              branch(p.HeadRef, p.HeadSHA),
		Base:              branch(p.BaseRef, p.BaseSHA),
		AuthorAssociation: v.authorAssociation(p.UserID, r),
		Merged:            p.Merged,
		Comments:          v.commentCount(r, p.Number),
	}
	if p.State == stateOpen {
		mergeable := true
		j.Mergeable = &mergeable
	}
	if p.Merged {
		by := v.user(p.MergedByID)
		j.MergedBy = &by
	}
	return j
}

func (v view) issue(r *repo, p *pull) issueJSON {
	api := fmt.Sprintf("%s/issues/%d", v.repoAPI(r), p.Number)
	html := fmt.Sprintf("%s/pull/%d", v.repoHTML(r), p.Number)
	return issueJSON{
		URL:               api,
		RepositoryURL:     v.repoAPI(r),
		CommentsURL:       api + "/comments",
		HTMLURL:           html,
		ID:                p.IssueID,
		Number:            p.Number,
		Title:             p.Title,
		User:              v.user(p.UserID),
		Labels:            v.labels(r, p.Labels),
		State:             p.State,
		Comments:          v.commentCount(r, p.Number),
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
		ClosedAt:          p.ClosedAt,
		AuthorAssociation: v.authorAssociation(p.UserID, r),
		Body:              optional(p.Body),
		PullRequest: issuePullRefJSON{
			URL:      fmt.Sprintf("%s/pulls/%d", v.repoAPI(r), p.Number),
			HTMLURL:  html,
			DiffURL:  html + ".diff",
			PatchURL: html + ".patch",
			MergedAt: p.MergedAt,
		},
	}
}

func (v view) comment(r *repo, c *comment) commentJSON {
	return commentJSON{
		ID:                    c.ID,
		URL:                   fmt.Sprintf("%s/issues/comments/%d", v.repoAPI(r), c.ID),
		HTMLURL:               fmt.Sprintf("%s/pull/%d#issuecomment-%d", v.repoHTML(r), c.IssueNumber, c.ID),
		IssueURL:              fmt.Sprintf("%s/issues/%d", v.repoAPI(r), c.IssueNumber),
		Body:                  c.Body,
		User:                  v.user(c.UserID),
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
		AuthorAssociation:     v.authorAssociation(c.UserID, r),
		PerformedViaGitHubApp: v.appRef(c.AppID),
	}
}

func (v view) authorAssociation(userID int64, r *repo) string {
	if r.OwnerID == userID {
		return "OWNER"
	}
	if owner := v.w.account(r.OwnerID); owner != nil && owner.role(userID) != "" {
		return "MEMBER"
	}
	for _, c := range r.Collaborators {
		if c.UserID == userID {
			return "COLLABORATOR"
		}
	}
	return "NONE"
}

func (v view) app(a *app) appJSON {
	return appJSON{
		ID:          a.ID,
		Slug:        a.Slug,
		ClientID:    a.ClientID,
		Owner:       v.user(a.OwnerID),
		Name:        a.Name,
		HTMLURL:     v.web + "/apps/" + a.Slug,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
		Permissions: a.Permissions,
		Events:      a.Events,
	}
}

func (v view) appRef(id int64) *appJSON {
	a := v.w.app(id)
	if a == nil {
		return nil
	}
	j := v.app(a)
	return &j
}

func (v view) installation(i *installation) installationJSON {
	a := v.w.app(i.AppID)
	acct := v.w.account(i.AccountID)
	html := fmt.Sprintf("%s/settings/installations/%d", v.web, i.ID)
	j := installationJSON{
		ID:                  i.ID,
		Account:             v.user(i.AccountID),
		RepositorySelection: i.RepositorySelection,
		AccessTokensURL:     fmt.Sprintf("%s/app/installations/%d/access_tokens", v.api, i.ID),
		RepositoriesURL:     v.api + "/installation/repositories",
		HTMLURL:             html,
		AppID:               i.AppID,
		AppSlug:             a.Slug,
		TargetID:            i.AccountID,
		TargetType:          acct.Type,
		Permissions:         i.Permissions,
		Events:              orEmpty(i.Events),
		CreatedAt:           i.CreatedAt,
		UpdatedAt:           i.UpdatedAt,
		SingleFilePaths:     []string{},
		SuspendedAt:         i.SuspendedAt,
	}
	if i.SuspendedAt != nil {
		by := v.user(i.SuspendedByID)
		j.SuspendedBy = &by
	}
	return j
}

func (v view) status(r *repo, s *status) statusJSON {
	return statusJSON{
		URL:         v.repoAPI(r) + "/statuses/" + s.SHA,
		AvatarURL:   v.avatarURL(s.CreatorID),
		ID:          s.ID,
		State:       s.State,
		Description: optional(s.Description),
		TargetURL:   optional(s.TargetURL),
		Context:     s.Context,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.CreatedAt,
		Creator:     v.user(s.CreatorID),
	}
}

func (v view) deployment(r *repo, d *deployment) deploymentJSON {
	api := fmt.Sprintf("%s/deployments/%d", v.repoAPI(r), d.ID)
	return deploymentJSON{
		URL:                   api,
		ID:                    d.ID,
		SHA:                   d.SHA,
		Ref:                   d.Ref,
		Task:                  d.Task,
		Payload:               d.Payload,
		OriginalEnvironment:   d.Environment,
		Environment:           d.Environment,
		Description:           optional(d.Description),
		Creator:               v.user(d.CreatorID),
		CreatedAt:             d.CreatedAt,
		UpdatedAt:             d.CreatedAt,
		StatusesURL:           api + "/statuses",
		RepositoryURL:         v.repoAPI(r),
		TransientEnvironment:  d.Transient,
		ProductionEnvironment: d.Production,
		PerformedViaGitHubApp: v.appRef(d.AppID),
	}
}

func (v view) deploymentStatus(r *repo, d *deployment, s *deploymentStatus) deploymentStatusJSON {
	api := fmt.Sprintf("%s/deployments/%d", v.repoAPI(r), d.ID)
	return deploymentStatusJSON{
		URL:                   fmt.Sprintf("%s/statuses/%d", api, s.ID),
		ID:                    s.ID,
		State:                 s.State,
		Creator:               v.user(s.CreatorID),
		Description:           s.Description,
		Environment:           s.Environment,
		TargetURL:             s.TargetURL,
		LogURL:                s.LogURL,
		EnvironmentURL:        s.EnvironmentURL,
		CreatedAt:             s.CreatedAt,
		UpdatedAt:             s.CreatedAt,
		DeploymentURL:         api,
		RepositoryURL:         v.repoAPI(r),
		PerformedViaGitHubApp: v.appRef(s.AppID),
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
