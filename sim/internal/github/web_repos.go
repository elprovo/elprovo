package github

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type repoSummary struct {
	FullName string
	Private  bool
}

func (s *Sim) home(w http.ResponseWriter, r *http.Request) {
	var userID int64
	if u := s.currentUser(r); u != nil {
		userID = u.ID
	}
	var repos []repoSummary
	s.read(func(v view) {
		for _, rp := range v.w.Repos {
			if v.w.userRole(userID, rp) != "" {
				repos = append(repos, repoSummary{FullName: v.fullName(rp), Private: rp.Private})
			}
		}
	})
	slices.SortFunc(repos, func(a, b repoSummary) int { return strings.Compare(a.FullName, b.FullName) })
	s.render(w, r, http.StatusOK, "home", "Repositories", struct{ Repos []repoSummary }{repos})
}

type webRepo struct {
	owner, name, fullName, defaultBranch, cloneURL string
	canWrite                                       bool
}

func (s *Sim) webRepo(r *http.Request) (webRepo, error) {
	var userID int64
	if u := s.currentUser(r); u != nil {
		userID = u.ID
	}
	var out webRepo
	var err error
	s.read(func(v view) {
		rp := v.w.repo(r.PathValue("owner"), r.PathValue("repo"))
		if rp == nil || v.w.userRole(userID, rp) == "" {
			err = fail(http.StatusNotFound, "This repository does not exist or you cannot see it.")
			return
		}
		j := v.repo(rp)
		out = webRepo{
			owner: j.Owner.Login, name: rp.Name, fullName: j.FullName, defaultBranch: rp.DefaultBranch, cloneURL: j.CloneURL,
			canWrite: userID != 0 && permLevel(v.w.userRole(userID, rp)) >= permLevel(permWrite),
		}
	})
	return out, err
}

type repoPageData struct {
	FullName      string
	CloneURL      string
	DefaultBranch string
	Branches      []string
	Pulls         []pullJSON
	CanWrite      bool
}

func (s *Sim) repoPage(w http.ResponseWriter, r *http.Request) {
	wr, err := s.webRepo(r)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	branches, err := s.gitRepo(wr.owner, wr.name).Branches(r.Context())
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	data := repoPageData{FullName: wr.fullName, CloneURL: wr.cloneURL, DefaultBranch: wr.defaultBranch, Branches: branches, CanWrite: wr.canWrite}
	s.read(func(v view) {
		rp := v.w.repo(wr.owner, wr.name)
		for _, p := range slices.Backward(rp.Pulls) {
			data.Pulls = append(data.Pulls, v.pull(rp, p))
		}
	})
	s.render(w, r, http.StatusOK, "repo", wr.fullName, data)
}

func (s *Sim) writableRepo(w http.ResponseWriter, r *http.Request) (webRepo, *pageUser, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return webRepo{}, nil, false
	}
	wr, err := s.webRepo(r)
	if err == nil && !wr.canWrite {
		err = fail(http.StatusForbidden, "You need write access to %s.", wr.fullName)
	}
	if err != nil {
		s.renderError(w, r, err)
		return webRepo{}, nil, false
	}
	return wr, u, true
}

func (s *Sim) createPullForm(w http.ResponseWriter, r *http.Request) {
	wr, u, ok := s.writableRepo(w, r)
	if !ok {
		return
	}
	d := pullDraft{Title: r.PostFormValue("title"), Body: r.PostFormValue("body"), Head: r.PostFormValue("head"), Base: r.PostFormValue("base")}
	p, err := s.createPull(r.Context(), wr.owner, wr.name, u.ID, d)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/pull/%d", wr.fullName, p.Number), http.StatusSeeOther)
}

type pullPageData struct {
	FullName string
	Pull     pullJSON
	Comments []commentJSON
	CanWrite bool
}

func (s *Sim) pullPage(w http.ResponseWriter, r *http.Request) {
	wr, err := s.webRepo(r)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	number, err := pathInt(r, "number")
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	data := pullPageData{FullName: wr.fullName, CanWrite: wr.canWrite}
	s.read(func(v view) {
		rp := v.w.repo(wr.owner, wr.name)
		p := rp.pull(int(number))
		if p == nil {
			err = errNotFound()
			return
		}
		data.Pull = v.pull(rp, p)
		for _, c := range rp.Comments {
			if c.IssueNumber == p.Number {
				data.Comments = append(data.Comments, v.comment(rp, c))
			}
		}
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "pull", data.Pull.Title, data)
}

func (s *Sim) changePull(w http.ResponseWriter, r *http.Request, wr webRepo, change func(t *tx, rp *repo, p *pull) error) {
	number, err := pathInt(r, "number")
	if err == nil {
		err = s.update(func(t *tx) error {
			rp := t.w.repo(wr.owner, wr.name)
			if rp == nil || rp.pull(int(number)) == nil {
				return errNotFound()
			}
			return change(t, rp, rp.pull(int(number)))
		})
	}
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/pull/%d", wr.fullName, number), http.StatusSeeOther)
}

func (s *Sim) commentForm(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	wr, err := s.webRepo(r)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	s.changePull(w, r, wr, func(t *tx, rp *repo, p *pull) error {
		_, err := t.addComment(rp, p, u.ID, 0, r.PostFormValue("body"))
		return err
	})
}

func (s *Sim) addLabelForm(w http.ResponseWriter, r *http.Request) {
	if wr, u, ok := s.writableRepo(w, r); ok {
		s.changePull(w, r, wr, func(t *tx, rp *repo, p *pull) error {
			return t.addLabels(rp, p, u.ID, []string{r.PostFormValue("name")})
		})
	}
}

func (s *Sim) removeLabelForm(w http.ResponseWriter, r *http.Request) {
	if wr, u, ok := s.writableRepo(w, r); ok {
		s.changePull(w, r, wr, func(t *tx, rp *repo, p *pull) error {
			return t.removeLabel(rp, p, u.ID, r.PostFormValue("name"))
		})
	}
}

func (s *Sim) pullStateForm(w http.ResponseWriter, r *http.Request) {
	if wr, u, ok := s.writableRepo(w, r); ok {
		s.changePull(w, r, wr, func(t *tx, rp *repo, p *pull) error {
			return t.setPullState(rp, p, u.ID, r.PostFormValue("state"))
		})
	}
}

func (s *Sim) mergeForm(w http.ResponseWriter, r *http.Request) {
	wr, u, ok := s.writableRepo(w, r)
	if !ok {
		return
	}
	number, err := pathInt(r, "number")
	if err == nil {
		err = s.mergePull(r.Context(), wr.owner, wr.name, int(number), u.ID)
	}
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/pull/%d", wr.fullName, number), http.StatusSeeOther)
}
