package github

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
)

var (
	loginPattern    = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	reservedLogins  = []string{"apps", "avatars", "codeload", "login", "logout", "session", "settings", "signup"}
)

func validLogin(login string) bool {
	return len(login) <= 39 && loginPattern.MatchString(login) && !slices.Contains(reservedLogins, strings.ToLower(login))
}

func validRepoName(name string) bool {
	return len(name) <= 100 && repoNamePattern.MatchString(name) && name != "." && name != ".." &&
		!strings.HasSuffix(strings.ToLower(name), ".git")
}

func (t *tx) createAccount(login, name, email, kind string) (*account, error) {
	if !validLogin(login) {
		return nil, errValidation(kind, "login", "invalid")
	}
	if t.w.accountByLogin(login) != nil {
		return nil, errValidation(kind, "login", "already_exists")
	}
	a := &account{ID: t.w.newID(), Login: login, Name: name, Email: email, Type: kind, CreatedAt: now()}
	t.w.Accounts = append(t.w.Accounts, a)
	return a, nil
}

func (t *tx) setMember(orgLogin, userLogin, role string) error {
	org := t.w.accountByLogin(orgLogin)
	if org == nil || org.Type != typeOrg {
		return fail(http.StatusNotFound, "organization %s does not exist", orgLogin)
	}
	u := t.w.user(userLogin)
	if u == nil {
		return fail(http.StatusNotFound, "user %s does not exist", userLogin)
	}
	if role != roleAdmin && role != roleMember {
		return errValidation("Membership", "role", "invalid")
	}
	org.Members = slices.DeleteFunc(org.Members, func(m membership) bool { return m.UserID == u.ID })
	org.Members = append(org.Members, membership{UserID: u.ID, Role: role})
	return nil
}

func (t *tx) setCollaborator(owner, name, login, permission string) error {
	r := t.w.repo(owner, name)
	if r == nil {
		return errNotFound()
	}
	u := t.w.user(login)
	if u == nil {
		return fail(http.StatusNotFound, "user %s does not exist", login)
	}
	if permission != permRead && permission != permWrite && permission != permAdmin {
		return errValidation("Collaborator", "permission", "invalid")
	}
	r.Collaborators = slices.DeleteFunc(r.Collaborators, func(c collaborator) bool { return c.UserID == u.ID })
	r.Collaborators = append(r.Collaborators, collaborator{UserID: u.ID, Permission: permission})
	return nil
}
