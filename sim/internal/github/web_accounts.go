package github

import (
	"net/http"
	"slices"
	"strings"
)

// accountsOf returns the organizations the user belongs to, and the owners
// under which the user may create repositories: the user's own account and
// the organizations the user administers.
func accountsOf(v view, userID int64) (orgs, owners []string) {
	for _, a := range v.w.Accounts {
		switch {
		case a.ID == userID:
			owners = append(owners, a.Login)
		case a.Type == typeOrg && a.role(userID) != "":
			orgs = append(orgs, a.Login)
			if a.role(userID) == roleAdmin {
				owners = append(owners, a.Login)
			}
		}
	}
	slices.Sort(orgs)
	return orgs, owners
}

type newRepoPageData struct {
	Owners []string
}

func (s *Sim) newRepoPage(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	var data newRepoPageData
	s.read(func(v view) { _, data.Owners = accountsOf(v, u.ID) })
	s.render(w, r, http.StatusOK, "new_repo", "New repository", data)
}

func (s *Sim) newRepoSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	owner := r.PostFormValue("owner")
	var owners []string
	s.read(func(v view) { _, owners = accountsOf(v, u.ID) })
	if !slices.Contains(owners, owner) {
		s.renderError(w, r, fail(http.StatusForbidden, "You can create repositories only for yourself or an organization you administer."))
		return
	}
	created, err := s.createRepo(r.Context(), newRepo{
		Owner: owner, Name: strings.TrimSpace(r.PostFormValue("name")),
		Private: r.PostFormValue("private") == "on", AutoInit: r.PostFormValue("auto_init") == "on",
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+created.FullName, http.StatusSeeOther)
}

type person struct {
	Login string
	Role  string
}

type peoplePageData struct {
	Org     string
	People  []person
	IsAdmin bool
}

func (s *Sim) peoplePage(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	var data peoplePageData
	var err error
	s.read(func(v view) {
		org := v.w.accountByLogin(r.PathValue("org"))
		if org == nil || org.Type != typeOrg || org.role(u.ID) == "" {
			err = fail(http.StatusNotFound, "This organization does not exist or you are not a member.")
			return
		}
		data = peoplePageData{Org: org.Login, IsAdmin: org.role(u.ID) == roleAdmin}
		for _, m := range org.Members {
			if a := v.w.account(m.UserID); a != nil {
				data.People = append(data.People, person{Login: a.Login, Role: m.Role})
			}
		}
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	slices.SortFunc(data.People, func(a, b person) int { return strings.Compare(a.Login, b.Login) })
	s.render(w, r, http.StatusOK, "people", data.Org+" people", data)
}

// peopleSubmit adds a member, changes a member's role or removes a member.
// Only the organization's admins may.
func (s *Sim) peopleSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	orgLogin, login := r.PathValue("org"), strings.TrimSpace(r.PostFormValue("login"))
	err := s.update(func(t *tx) error {
		org := t.w.accountByLogin(orgLogin)
		if org == nil || org.Type != typeOrg || org.role(u.ID) == "" {
			return fail(http.StatusNotFound, "This organization does not exist or you are not a member.")
		}
		if org.role(u.ID) != roleAdmin {
			return fail(http.StatusForbidden, "Only admins of %s can change its members.", orgLogin)
		}
		if r.PostFormValue("action") == "remove" {
			return t.removeMember(orgLogin, login)
		}
		if target := t.w.user(login); target != nil && r.PostFormValue("role") != roleAdmin && org.lastAdmin(target.ID) {
			return fail(http.StatusUnprocessableEntity, "%s is the last admin of %s", login, orgLogin)
		}
		return t.setMember(orgLogin, login, r.PostFormValue("role"))
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, "/orgs/"+orgLogin+"/people", http.StatusSeeOther)
}
