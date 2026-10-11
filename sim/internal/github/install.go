package github

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

type installRepo struct {
	ID   int64
	Name string
}

type installAccount struct {
	Login string
	Repos []installRepo
}

type installData struct {
	AppName  string
	Slug     string
	State    string
	Accounts []installAccount
}

func (s *Sim) installPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.installData(r)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	s.read(func(v view) {
		for _, acct := range v.w.adminAccounts(u.ID) {
			ia := installAccount{Login: acct.Login}
			for _, rp := range v.w.Repos {
				if rp.OwnerID == acct.ID {
					ia.Repos = append(ia.Repos, installRepo{ID: rp.ID, Name: rp.Name})
				}
			}
			data.Accounts = append(data.Accounts, ia)
		}
	})
	s.render(w, r, http.StatusOK, "install", "Install "+data.AppName, data)
}

func (s *Sim) installData(r *http.Request) (installData, error) {
	var data installData
	var err error
	s.read(func(v view) {
		a := v.w.appBySlug(r.PathValue("slug"))
		if a == nil {
			err = fail(http.StatusNotFound, "No GitHub App is called %s.", r.PathValue("slug"))
			return
		}
		data = installData{AppName: a.Name, Slug: a.Slug, State: r.FormValue("state")}
	})
	return data, err
}

func (s *Sim) installSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, fail(http.StatusBadRequest, "The form could not be read."))
		return
	}
	var repoIDs []int64
	for _, raw := range r.PostForm["repository_ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			s.renderError(w, r, errValidation("Installation", "repositories", "invalid"))
			return
		}
		repoIDs = append(repoIDs, id)
	}
	var instID int64
	var created bool
	var setupURL, appName, accountLogin string
	err := s.update(func(t *tx) error {
		a := t.w.appBySlug(r.PathValue("slug"))
		if a == nil {
			return errNotFound()
		}
		acct := t.w.accountByLogin(r.PostFormValue("account"))
		if acct == nil || !slices.Contains(t.w.adminAccounts(u.ID), acct) {
			return fail(http.StatusForbidden, "You cannot install apps on %s.", r.PostFormValue("account"))
		}
		inst, isNew, err := t.installApp(a, acct, r.PostFormValue("repository_selection"), repoIDs, u.ID)
		if err != nil {
			return err
		}
		instID, created, setupURL, appName, accountLogin = inst.ID, isNew, a.SetupURL, a.Name, acct.Login
		return nil
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	if setupURL == "" {
		s.render(w, r, http.StatusOK, "message", "Installed", messageData{Text: appName + " is installed on " + accountLogin + "."})
		return
	}
	action := "update"
	if created {
		action = "install"
	}
	redirectWith(w, r, setupURL, url.Values{
		"installation_id": {strconv.FormatInt(instID, 10)},
		"setup_action":    {action},
		"state":           {r.PostFormValue("state")},
	})
}
