package github

import (
	"context"
	"net/http"
	"strings"

	"github.com/stagely-dev/stagely/sim/internal/github/gitrepo"
)

func gitRequestPath(path string) (owner, name, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), parts[2], true
}

func isPush(r *http.Request, rest string) bool {
	return rest == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack"
}

type gitAccess struct {
	owner, name string
	repoID      int64
	actorID     int64
	actorLogin  string
	allowed     bool
	missing     bool
	canCreate   bool
}

func (s *Sim) serveGit(w http.ResponseWriter, r *http.Request, owner, name, rest string) {
	c, err := s.gitCaller(r)
	if err != nil {
		challenge(w)
		return
	}
	push := isPush(r, rest)
	access := s.gitAccess(c, owner, name, push)
	if access.missing && push && access.canCreate {
		if _, err := s.createRepo(r.Context(), newRepo{Owner: owner, Name: name, Private: true}); err != nil {
			s.writeError(w, r, err)
			return
		}
		access = s.gitAccess(c, owner, name, push)
	}
	switch {
	case access.missing && c.kind == callerAnonymous:
		challenge(w)
		return
	case access.missing:
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	case !access.allowed && c.kind == callerAnonymous:
		challenge(w)
		return
	case !access.allowed:
		http.Error(w, "Permission to "+access.owner+"/"+access.name+".git denied.", http.StatusForbidden)
		return
	}
	r.URL.Path = "/" + access.owner + "/" + access.name + ".git/" + rest
	switch {
	case push && r.Method == http.MethodPost:
		s.servePush(w, r, access)
	case push:
		gitrepo.ServeHTTP(w, r, s.reposDir(), access.actorLogin)
	default:
		gitrepo.ServeHTTP(w, r, s.reposDir(), "")
	}
}

func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
	http.Error(w, "Authentication failed", http.StatusUnauthorized)
}

func (s *Sim) gitCaller(r *http.Request) (caller, error) {
	login, password, ok := r.BasicAuth()
	if !ok {
		return caller{kind: callerAnonymous}, nil
	}
	var c caller
	var err error
	s.read(func(v view) {
		if looksLikeToken(password) {
			c, err = v.w.callerForCredential(password)
			return
		}
		u := v.w.user(login)
		if u == nil {
			err = badCredentials{}
			return
		}
		c = caller{kind: callerPersonal, userID: u.ID}
	})
	return c, err
}

func (s *Sim) gitAccess(c caller, owner, name string, push bool) gitAccess {
	level := permRead
	if push {
		level = permWrite
	}
	var a gitAccess
	s.read(func(v view) {
		rp := v.w.repo(owner, name)
		if rp == nil {
			acct := v.w.accountByLogin(owner)
			a.missing = true
			a.canCreate = c.kind == callerPersonal && acct != nil && validRepoName(name) &&
				(acct.ID == c.userID || acct.role(c.userID) == roleAdmin)
			return
		}
		perms := v.w.repoPerms(c, rp)
		if perms == nil {
			a.missing = true
			return
		}
		a.owner, a.name, a.repoID = v.w.account(rp.OwnerID).Login, rp.Name, rp.ID
		a.allowed = allows(perms, "contents", level)
		a.actorID = c.userID
		if c.kind == callerInstallation {
			a.actorID = v.w.app(c.appID).BotID
		}
		if actor := v.w.account(a.actorID); actor != nil {
			a.actorLogin = actor.Login
		}
	})
	return a
}

func (s *Sim) servePush(w http.ResponseWriter, r *http.Request, a gitAccess) {
	defer s.lockRepo(a.repoID)()
	g := s.gitRepo(a.owner, a.name)
	before, err := g.Refs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	gitrepo.ServeHTTP(w, r, s.reposDir(), a.actorLogin)

	ctx := context.WithoutCancel(r.Context())
	after, err := g.Refs(ctx)
	if err != nil {
		s.log.Error("read refs after push", "repository", a.owner+"/"+a.name, "error", err)
		return
	}
	updates, err := collectRefUpdates(ctx, g, before, after)
	if err != nil {
		s.log.Error("describe push", "repository", a.owner+"/"+a.name, "error", err)
		return
	}
	if len(updates) == 0 {
		return
	}
	err = s.update(func(t *tx) error {
		if rp := t.w.repoByID(a.repoID); rp != nil {
			t.recordPush(rp, a.actorID, updates)
		}
		return nil
	})
	if err != nil {
		s.log.Error("record push", "repository", a.owner+"/"+a.name, "error", err)
	}
}
