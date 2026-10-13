package github

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elprovo/elprovo/sim/internal/github/gitrepo"
)

const maxJSONBlobSize = 1 << 20

func (v view) repoFor(c caller, r *http.Request, level string, perms ...string) (*repo, error) {
	rp := v.w.repo(r.PathValue("owner"), r.PathValue("repo"))
	if rp == nil {
		return nil, errNotFound()
	}
	granted := v.w.repoPerms(c, rp)
	if granted == nil {
		return nil, errNotFound()
	}
	for _, name := range perms {
		if allows(granted, name, level) {
			return rp, nil
		}
	}
	return nil, errDenied(c)
}

type repoHandle struct {
	owner, name, defaultBranch string
	git                        *gitrepo.Repo
}

func (s *Sim) contentsHandle(c caller, r *http.Request) (repoHandle, error) {
	var h repoHandle
	var err error
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permRead, "contents"); err != nil {
			return
		}
		owner := v.w.account(rp.OwnerID).Login
		h = repoHandle{owner: owner, name: rp.Name, defaultBranch: rp.DefaultBranch, git: s.gitRepo(owner, rp.Name)}
	})
	return h, err
}

func (s *Sim) getRepo(w http.ResponseWriter, r *http.Request, c caller) {
	var out repoJSON
	var err error
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permRead, "metadata"); err != nil {
			return
		}
		out = v.repo(rp)
		if c.kind == callerUser || c.kind == callerPersonal {
			out = v.repoForUser(rp, c.userID)
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) listPulls(w http.ResponseWriter, r *http.Request, c caller) {
	state := r.URL.Query().Get("state")
	var all []pullJSON
	var err error
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permRead, "pull_requests"); err != nil {
			return
		}
		for _, p := range slices.Backward(rp.Pulls) {
			if state == "all" || p.State == cmp.Or(state, stateOpen) {
				all = append(all, v.pull(rp, p))
			}
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	lo, hi := s.paginate(w, r, len(all))
	writeJSON(w, http.StatusOK, orEmpty(all[lo:hi]))
}

func (s *Sim) getPull(w http.ResponseWriter, r *http.Request, c caller) {
	number, err := pathInt(r, "number")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out pullJSON
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permRead, "pull_requests"); err != nil {
			return
		}
		p := rp.pull(int(number))
		if p == nil {
			err = errNotFound()
			return
		}
		out = v.pull(rp, p)
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type contentLinks struct {
	Self string `json:"self"`
	Git  string `json:"git"`
	HTML string `json:"html"`
}

type contentJSON struct {
	Type        string       `json:"type"`
	Encoding    string       `json:"encoding,omitempty"`
	Size        int64        `json:"size"`
	Name        string       `json:"name"`
	Path        string       `json:"path"`
	Content     *string      `json:"content,omitempty"`
	Target      string       `json:"target,omitempty"`
	SHA         string       `json:"sha"`
	URL         string       `json:"url"`
	GitURL      string       `json:"git_url"`
	HTMLURL     string       `json:"html_url"`
	DownloadURL *string      `json:"download_url"`
	Links       contentLinks `json:"_links"`
}

func (s *Sim) getContents(w http.ResponseWriter, r *http.Request, c caller) {
	h, err := s.contentsHandle(c, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p, ok := cleanContentPath(r.PathValue("path"))
	if !ok {
		s.writeError(w, r, errNotFound())
		return
	}
	ref := cmp.Or(r.URL.Query().Get("ref"), h.defaultBranch)
	commit, err := h.git.ResolveCommit(r.Context(), ref)
	if isNotFound(err) {
		err = fail(http.StatusNotFound, "No commit found for the ref %s", ref)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	entry, err := h.git.Stat(r.Context(), commit, p)
	if isNotFound(err) {
		err = errNotFound()
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if entry.Type == "tree" {
		s.writeDirectory(w, r, h, commit, ref, p)
		return
	}
	s.writeFile(w, r, h, ref, entry)
}

func (s *Sim) writeDirectory(w http.ResponseWriter, r *http.Request, h repoHandle, commit, ref, dir string) {
	entries, err := h.git.ListTree(r.Context(), commit, dir)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := []contentJSON{}
	for _, e := range entries {
		out = append(out, s.contentEntry(h, ref, e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) writeFile(w http.ResponseWriter, r *http.Request, h repoHandle, ref string, e gitrepo.Entry) {
	data, err := h.git.ReadBlob(r.Context(), e.SHA)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if wantsRaw(r) && e.Type == "blob" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return
	}
	out := s.contentEntry(h, ref, e)
	switch {
	case e.Mode == "120000":
		out.Target = string(data)
	case e.Type == "blob" && len(data) > maxJSONBlobSize:
		s.writeError(w, r, &apiError{
			status:  http.StatusForbidden,
			message: "This API returns blobs up to 1 MB in size. The requested blob is too large to fetch via the API, but you can use the Git Data API to request blobs up to 100 MB in size.",
			errors:  []fieldError{{Resource: "Blob", Field: "data", Code: "too_large"}},
		})
		return
	case e.Type == "blob":
		content := wrapBase64(data)
		out.Encoding, out.Content = "base64", &content
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) contentEntry(h repoHandle, ref string, e gitrepo.Entry) contentJSON {
	full := h.owner + "/" + h.name
	escaped := escapePath(e.Path)
	typ, gitKind, htmlKind := "file", "blobs", "blob"
	switch {
	case e.Type == "tree":
		typ, gitKind, htmlKind = "dir", "trees", "tree"
	case e.Type == "commit":
		typ, gitKind = "submodule", "trees"
	case e.Mode == "120000":
		typ = "symlink"
	}
	self := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", s.cfg.APIURL, full, escaped, url.QueryEscape(ref))
	gitURL := fmt.Sprintf("%s/repos/%s/git/%s/%s", s.cfg.APIURL, full, gitKind, e.SHA)
	htmlURL := fmt.Sprintf("%s/%s/%s/%s/%s", s.cfg.WebURL, full, htmlKind, url.PathEscape(ref), escaped)
	return contentJSON{
		Type: typ, Size: e.Size, Name: e.Name(), Path: e.Path, SHA: e.SHA,
		URL: self, GitURL: gitURL, HTMLURL: htmlURL,
		Links: contentLinks{Self: self, Git: gitURL, HTML: htmlURL},
	}
}

func cleanContentPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

func wantsRaw(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/vnd.github.raw") || strings.Contains(accept, "application/vnd.github.v3.raw")
}

func wrapBase64(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(enc) > 60 {
		b.WriteString(enc[:60])
		b.WriteByte('\n')
		enc = enc[60:]
	}
	b.WriteString(enc)
	b.WriteByte('\n')
	return b.String()
}

type gitPersonJSON struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

type treeRefJSON struct {
	SHA string `json:"sha"`
	URL string `json:"url"`
}

type commitDetailJSON struct {
	Author       gitPersonJSON `json:"author"`
	Committer    gitPersonJSON `json:"committer"`
	Message      string        `json:"message"`
	Tree         treeRefJSON   `json:"tree"`
	URL          string        `json:"url"`
	CommentCount int           `json:"comment_count"`
}

type commitRefJSON struct {
	SHA     string `json:"sha"`
	URL     string `json:"url"`
	HTMLURL string `json:"html_url"`
}

type commitJSON struct {
	SHA         string           `json:"sha"`
	URL         string           `json:"url"`
	HTMLURL     string           `json:"html_url"`
	CommentsURL string           `json:"comments_url"`
	Commit      commitDetailJSON `json:"commit"`
	Author      *userJSON        `json:"author"`
	Committer   *userJSON        `json:"committer"`
	Parents     []commitRefJSON  `json:"parents"`
}

func (s *Sim) getCommit(w http.ResponseWriter, r *http.Request, c caller) {
	h, err := s.contentsHandle(c, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ref := r.PathValue("ref")
	sha, err := h.git.ResolveCommit(r.Context(), ref)
	if isNotFound(err) {
		err = fail(http.StatusUnprocessableEntity, "No commit found for SHA: %s", ref)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	commit, err := h.git.ReadCommit(r.Context(), sha)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out commitJSON
	s.read(func(v view) { out = v.commit(h.owner+"/"+h.name, commit) })
	writeJSON(w, http.StatusOK, out)
}

func (v view) commit(fullName string, c gitrepo.Commit) commitJSON {
	api := v.api + "/repos/" + fullName
	html := v.web + "/" + fullName
	out := commitJSON{
		SHA:         c.SHA,
		URL:         api + "/commits/" + c.SHA,
		HTMLURL:     html + "/commit/" + c.SHA,
		CommentsURL: api + "/commits/" + c.SHA + "/comments",
		Commit: commitDetailJSON{
			Author:    gitPersonJSON{Name: c.Author.Name, Email: c.Author.Email, Date: c.Author.When},
			Committer: gitPersonJSON{Name: c.Committer.Name, Email: c.Committer.Email, Date: c.Committer.When},
			Message:   c.Message,
			Tree:      treeRefJSON{SHA: c.Tree, URL: api + "/git/trees/" + c.Tree},
			URL:       api + "/git/commits/" + c.SHA,
		},
		Author:    v.userByEmail(c.Author.Email),
		Committer: v.userByEmail(c.Committer.Email),
		Parents:   []commitRefJSON{},
	}
	for _, p := range c.Parents {
		out.Parents = append(out.Parents, commitRefJSON{SHA: p, URL: api + "/commits/" + p, HTMLURL: html + "/commit/" + p})
	}
	return out
}

func (v view) userByEmail(email string) *userJSON {
	for _, a := range v.w.Accounts {
		if a.Type == typeUser && strings.EqualFold(signature(a).Email, email) {
			u := v.user(a.ID)
			return &u
		}
	}
	return nil
}

type collaboratorJSON struct {
	userJSON
	Permissions repoPermsJSON `json:"permissions"`
	RoleName    string        `json:"role_name"`
}

type collaboratorPermissionJSON struct {
	Permission string           `json:"permission"`
	RoleName   string           `json:"role_name"`
	User       collaboratorJSON `json:"user"`
}

func (s *Sim) getCollaboratorPermission(w http.ResponseWriter, r *http.Request, c caller) {
	var out collaboratorPermissionJSON
	var err error
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permRead, "metadata"); err != nil {
			return
		}
		u := v.w.user(r.PathValue("username"))
		if u == nil {
			err = errNotFound()
			return
		}
		role := cmp.Or(v.w.userRole(u.ID, rp), "none")
		perms := v.repoForUser(rp, u.ID).Permissions
		out = collaboratorPermissionJSON{
			Permission: role,
			RoleName:   role,
			User:       collaboratorJSON{userJSON: v.user(u.ID), Permissions: *perms, RoleName: role},
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
