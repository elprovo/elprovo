package github

import (
	"net/http"
	"slices"
	"strconv"
)

func (s *Sim) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_sim/users", s.adminCreateUser)
	mux.HandleFunc("POST /_sim/users/{login}/tokens", s.adminCreateToken)
	mux.HandleFunc("POST /_sim/orgs", s.adminCreateOrg)
	mux.HandleFunc("PUT /_sim/orgs/{org}/members/{login}", s.adminSetMember)
	mux.HandleFunc("POST /_sim/repos", s.adminCreateRepo)
	mux.HandleFunc("PUT /_sim/repos/{owner}/{repo}/collaborators/{login}", s.adminSetCollaborator)
	mux.HandleFunc("POST /_sim/apps", s.adminCreateApp)
	mux.HandleFunc("GET /_sim/apps/{slug}", s.adminGetApp)
	mux.HandleFunc("POST /_sim/apps/{slug}/installations", s.adminInstall)
	mux.HandleFunc("GET /_sim/deliveries", s.adminListDeliveries)
	mux.HandleFunc("POST /_sim/deliveries/{id}/redeliver", s.adminRedeliver)
	mux.HandleFunc("POST /_sim/repos/{owner}/{repo}/pulls", s.adminOpenPull)
	mux.HandleFunc("PATCH /_sim/repos/{owner}/{repo}/pulls/{number}", s.adminUpdatePull)
	mux.HandleFunc("PUT /_sim/repos/{owner}/{repo}/pulls/{number}/merge", s.adminMergePull)
	mux.HandleFunc("POST /_sim/repos/{owner}/{repo}/issues/{number}/comments", s.adminComment)
	mux.HandleFunc("POST /_sim/repos/{owner}/{repo}/issues/{number}/labels", s.adminAddLabels)
	mux.HandleFunc("DELETE /_sim/repos/{owner}/{repo}/issues/{number}/labels/{name}", s.adminRemoveLabel)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.writeError(w, r, errNotFound()) })
	return mux
}

func (s *Sim) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

type accountInput struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Sim) createAccountFromRequest(w http.ResponseWriter, r *http.Request, kind string) {
	var in accountInput
	var out fullUserJSON
	err := decodeJSON(r, &in)
	if err == nil {
		err = s.update(func(t *tx) error {
			a, err := t.createAccount(in.Login, in.Name, in.Email, kind)
			if err == nil {
				out = t.fullUser(a, true)
			}
			return err
		})
	}
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	s.createAccountFromRequest(w, r, typeUser)
}

func (s *Sim) adminCreateOrg(w http.ResponseWriter, r *http.Request) {
	s.createAccountFromRequest(w, r, typeOrg)
}

func (s *Sim) adminCreateToken(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Token string `json:"token"`
	}
	err := s.update(func(t *tx) error {
		u := t.w.user(r.PathValue("login"))
		if u == nil {
			return errNotFound()
		}
		out.Token = t.w.issueToken(token{Kind: kindPersonal, UserID: u.ID}, 0).Value
		return nil
	})
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminSetMember(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	err := decodeJSON(r, &in)
	if err == nil {
		err = s.update(func(t *tx) error { return t.setMember(r.PathValue("org"), r.PathValue("login"), in.Role) })
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Sim) adminCreateRepo(w http.ResponseWriter, r *http.Request) {
	var in newRepo
	err := decodeJSON(r, &in)
	var out repoJSON
	if err == nil {
		out, err = s.createRepo(r.Context(), in)
	}
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminSetCollaborator(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Permission string `json:"permission"`
	}
	err := decodeJSON(r, &in)
	if err == nil {
		err = s.update(func(t *tx) error {
			return t.setCollaborator(r.PathValue("owner"), r.PathValue("repo"), r.PathValue("login"), in.Permission)
		})
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Sim) adminCreateApp(w http.ResponseWriter, r *http.Request) {
	var in newApp
	err := decodeJSON(r, &in)
	var out appCredentials
	if err == nil {
		out, err = s.createApp(in)
	}
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminGetApp(w http.ResponseWriter, r *http.Request) {
	var out appCredentials
	err := errNotFound()
	s.read(func(v view) {
		if a := v.w.appBySlug(r.PathValue("slug")); a != nil {
			out, err = s.credentials(v, a), nil
		}
	})
	s.respond(w, r, http.StatusOK, out, err)
}

type installInput struct {
	Account             string   `json:"account"`
	RepositorySelection string   `json:"repository_selection"`
	Repositories        []string `json:"repositories"`
}

func (s *Sim) adminInstall(w http.ResponseWriter, r *http.Request) {
	var in installInput
	err := decodeJSON(r, &in)
	var out installationJSON
	status := http.StatusOK
	if err == nil {
		err = s.update(func(t *tx) error {
			a := t.w.appBySlug(r.PathValue("slug"))
			acct := t.w.accountByLogin(in.Account)
			if a == nil || acct == nil || acct.Type == typeBot {
				return errNotFound()
			}
			var ids []int64
			for _, name := range in.Repositories {
				rp := t.w.repo(acct.Login, name)
				if rp == nil {
					return errValidation("Installation", "repositories", "invalid")
				}
				ids = append(ids, rp.ID)
			}
			sender := acct.ID
			if acct.Type == typeOrg {
				sender = a.OwnerID
			}
			inst, created, err := t.installApp(a, acct, in.RepositorySelection, ids, sender)
			if err != nil {
				return err
			}
			if created {
				status = http.StatusCreated
			}
			out = t.installation(inst)
			return nil
		})
	}
	s.respond(w, r, status, out, err)
}

func (s *Sim) adminListDeliveries(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("app")
	out := []delivery{}
	s.read(func(v view) {
		for _, d := range slices.Backward(v.w.Deliveries) {
			if a := v.w.app(d.AppID); slug == "" || (a != nil && a.Slug == slug) {
				out = append(out, *d)
			}
		}
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) adminRedeliver(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, r, errNotFound())
		return
	}
	var out struct {
		ID   int64  `json:"id"`
		GUID string `json:"guid"`
	}
	err = s.update(func(t *tx) error {
		d := t.w.delivery(id)
		if d == nil {
			return errNotFound()
		}
		redo := &delivery{
			ID: t.w.newID(), GUID: d.GUID, AppID: d.AppID, InstallationID: d.InstallationID,
			Event: d.Event, Action: d.Action, Payload: d.Payload, Redelivery: true, URL: d.URL,
		}
		t.w.appendDelivery(redo)
		t.deliveries = append(t.deliveries, redo.ID)
		out.ID, out.GUID = redo.ID, redo.GUID
		return nil
	})
	s.respond(w, r, http.StatusAccepted, out, err)
}

type actorInput struct {
	User   string   `json:"user"`
	Title  *string  `json:"title"`
	Body   *string  `json:"body"`
	Head   string   `json:"head"`
	Base   string   `json:"base"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

func (s *Sim) adminActor(r *http.Request) (actorInput, int64, error) {
	var in actorInput
	if r.Method != http.MethodDelete {
		if err := decodeJSON(r, &in); err != nil {
			return in, 0, err
		}
	} else {
		in.User = r.URL.Query().Get("user")
	}
	var userID int64
	s.read(func(v view) {
		if u := v.w.user(in.User); u != nil {
			userID = u.ID
		}
	})
	if userID == 0 {
		return in, 0, errValidation("User", "user", "invalid")
	}
	return in, userID, nil
}

func (s *Sim) adminOpenPull(w http.ResponseWriter, r *http.Request) {
	in, userID, err := s.adminActor(r)
	var out pullJSON
	if err == nil {
		d := pullDraft{Head: in.Head, Base: in.Base}
		if in.Title != nil {
			d.Title = *in.Title
		}
		if in.Body != nil {
			d.Body = *in.Body
		}
		out, err = s.createPull(r.Context(), r.PathValue("owner"), r.PathValue("repo"), userID, d)
	}
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminPull(r *http.Request, change func(t *tx, rp *repo, p *pull, in actorInput, userID int64) error) (any, error) {
	in, userID, err := s.adminActor(r)
	if err != nil {
		return nil, err
	}
	number, err := pathInt(r, "number")
	if err != nil {
		return nil, err
	}
	var out any
	err = s.update(func(t *tx) error {
		rp := t.w.repo(r.PathValue("owner"), r.PathValue("repo"))
		if rp == nil || rp.pull(int(number)) == nil {
			return errNotFound()
		}
		p := rp.pull(int(number))
		if err := change(t, rp, p, in, userID); err != nil {
			return err
		}
		out = t.pull(rp, p)
		return nil
	})
	return out, err
}

func (s *Sim) adminUpdatePull(w http.ResponseWriter, r *http.Request) {
	out, err := s.adminPull(r, func(t *tx, rp *repo, p *pull, in actorInput, userID int64) error {
		changeState := in.State != "" && in.State != p.State
		if changeState {
			if err := checkPullState(p, in.State); err != nil {
				return err
			}
		}
		if err := t.editPull(rp, p, userID, in.Title, in.Body); err != nil {
			return err
		}
		if !changeState {
			return nil
		}
		return t.setPullState(rp, p, userID, in.State)
	})
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Sim) adminMergePull(w http.ResponseWriter, r *http.Request) {
	_, userID, err := s.adminActor(r)
	var number int64
	if err == nil {
		number, err = pathInt(r, "number")
	}
	if err == nil {
		err = s.mergePull(r.Context(), r.PathValue("owner"), r.PathValue("repo"), int(number), userID)
	}
	var out pullJSON
	if err == nil {
		s.read(func(v view) {
			rp := v.w.repo(r.PathValue("owner"), r.PathValue("repo"))
			out = v.pull(rp, rp.pull(int(number)))
		})
	}
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Sim) adminComment(w http.ResponseWriter, r *http.Request) {
	var out commentJSON
	_, err := s.adminPull(r, func(t *tx, rp *repo, p *pull, in actorInput, userID int64) error {
		c, err := t.addComment(rp, p, userID, 0, derefString(in.Body))
		if err == nil {
			out = t.comment(rp, c)
		}
		return err
	})
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Sim) adminAddLabels(w http.ResponseWriter, r *http.Request) {
	var out []labelJSON
	_, err := s.adminPull(r, func(t *tx, rp *repo, p *pull, in actorInput, userID int64) error {
		err := t.addLabels(rp, p, userID, in.Labels)
		out = t.labels(rp, p.Labels)
		return err
	})
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Sim) adminRemoveLabel(w http.ResponseWriter, r *http.Request) {
	var out []labelJSON
	_, err := s.adminPull(r, func(t *tx, rp *repo, p *pull, _ actorInput, userID int64) error {
		err := t.removeLabel(rp, p, userID, r.PathValue("name"))
		out = t.labels(rp, p.Labels)
		return err
	})
	s.respond(w, r, http.StatusOK, out, err)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
