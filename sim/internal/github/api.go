package github

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const anonymousRateLimit = 60

type apiHandler func(w http.ResponseWriter, r *http.Request, c caller)

func (s *Sim) apiHandler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h apiHandler) { mux.Handle(pattern, s.authenticated(h)) }

	route("GET /app", s.getApp)
	route("GET /app/installations", s.listAppInstallations)
	route("GET /app/installations/{id}", s.getAppInstallation)
	route("DELETE /app/installations/{id}", s.deleteAppInstallation)
	route("PUT /app/installations/{id}/suspended", s.suspendInstallation)
	route("DELETE /app/installations/{id}/suspended", s.unsuspendInstallation)
	route("POST /app/installations/{id}/access_tokens", s.createInstallationToken)
	route("GET /installation/repositories", s.listInstallationRepos)

	route("GET /user", s.getAuthenticatedUser)
	route("GET /user/installations", s.listUserInstallations)
	route("GET /user/installations/{id}/repositories", s.listUserInstallationRepos)
	route("GET /users/{login}", s.getUser)

	route("GET /repos/{owner}/{repo}", s.getRepo)
	route("GET /repos/{owner}/{repo}/pulls", s.listPulls)
	route("GET /repos/{owner}/{repo}/pulls/{number}", s.getPull)
	route("GET /repos/{owner}/{repo}/contents", s.getContents)
	route("GET /repos/{owner}/{repo}/contents/{path...}", s.getContents)
	route("GET /repos/{owner}/{repo}/tarball", s.getTarball)
	route("GET /repos/{owner}/{repo}/tarball/{ref...}", s.getTarball)
	route("GET /repos/{owner}/{repo}/commits/{ref...}", s.getCommit)
	route("GET /repos/{owner}/{repo}/collaborators/{username}/permission", s.getCollaboratorPermission)

	route("GET /repos/{owner}/{repo}/issues/{number}/comments", s.listComments)
	route("POST /repos/{owner}/{repo}/issues/{number}/comments", s.createComment)
	route("PATCH /repos/{owner}/{repo}/issues/comments/{id}", s.updateComment)
	route("DELETE /repos/{owner}/{repo}/issues/{number}/{child}", s.deleteIssueChild)
	route("GET /repos/{owner}/{repo}/issues/{number}/labels", s.listIssueLabels)
	route("POST /repos/{owner}/{repo}/issues/{number}/labels", s.addIssueLabels)
	route("DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}", s.removeIssueLabel)

	route("POST /repos/{owner}/{repo}/statuses/{sha}", s.createStatus)
	route("POST /repos/{owner}/{repo}/deployments", s.createDeployment)
	route("POST /repos/{owner}/{repo}/deployments/{id}/statuses", s.createDeploymentStatus)

	route("/", func(w http.ResponseWriter, r *http.Request, _ caller) { s.writeError(w, r, errNotFound()) })
	return mux
}

func (s *Sim) authenticated(h apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.callerFor(r)
		if err != nil {
			s.writeError(w, r, fail(http.StatusUnauthorized, "%s", err.Error()))
			return
		}
		if !s.limits.take(w, c, s.cfg.RateLimit) {
			s.writeError(w, r, fail(http.StatusForbidden, "API rate limit exceeded for %s.", c.rateKey))
			return
		}
		h(w, r, c)
	})
}

type badCredentials struct{}

func (badCredentials) Error() string { return "Bad credentials" }

func (s *Sim) callerFor(r *http.Request) (caller, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		return caller{kind: callerAnonymous, rateKey: host}, nil
	}
	scheme, cred, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "bearer") && !strings.EqualFold(scheme, "token") {
		return caller{}, badCredentials{}
	}
	var c caller
	var err error
	s.read(func(v view) { c, err = v.w.callerForCredential(strings.TrimSpace(cred)) })
	return c, err
}

func (w *world) callerForCredential(cred string) (caller, error) {
	if looksLikeJWT(cred) {
		a, err := w.verifyAppJWT(cred, time.Now())
		if err != nil {
			return caller{}, err
		}
		return caller{kind: callerApp, appID: a.ID, rateKey: fmt.Sprintf("app ID %d", a.ID)}, nil
	}
	t := w.liveToken(cred)
	if t == nil {
		return caller{}, badCredentials{}
	}
	switch t.Kind {
	case kindInstallation:
		return caller{
			kind: callerInstallation, appID: t.AppID, installationID: t.InstallationID,
			repoIDs: t.RepoIDs, permissions: t.Permissions,
			rateKey: fmt.Sprintf("installation ID %d", t.InstallationID),
		}, nil
	case kindUser:
		return caller{kind: callerUser, appID: t.AppID, userID: t.UserID, rateKey: fmt.Sprintf("user ID %d", t.UserID)}, nil
	case kindPersonal:
		return caller{kind: callerPersonal, userID: t.UserID, rateKey: fmt.Sprintf("user ID %d", t.UserID)}, nil
	}
	return caller{}, badCredentials{}
}

type rateWindow struct {
	used  int
	reset time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

func newRateLimiter() *rateLimiter { return &rateLimiter{windows: map[string]*rateWindow{}} }

func (l *rateLimiter) take(w http.ResponseWriter, c caller, authenticated int) bool {
	limit := authenticated
	if c.kind == callerAnonymous {
		limit = anonymousRateLimit
	}
	l.mu.Lock()
	t := time.Now()
	win := l.windows[c.rateKey]
	if win == nil || !t.Before(win.reset) {
		l.prune(t)
		win = &rateWindow{reset: t.Add(time.Hour)}
		l.windows[c.rateKey] = win
	}
	allowed := win.used < limit
	if allowed {
		win.used++
	}
	used, reset := win.used, win.reset
	l.mu.Unlock()

	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(limit-used))
	h.Set("X-RateLimit-Used", strconv.Itoa(used))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set("X-RateLimit-Resource", "core")
	return allowed
}

func (l *rateLimiter) prune(t time.Time) {
	for key, win := range l.windows {
		if !t.Before(win.reset) {
			delete(l.windows, key)
		}
	}
}

func (s *Sim) paginate(w http.ResponseWriter, r *http.Request, total int) (lo, hi int) {
	q := r.URL.Query()
	page := positiveInt(q.Get("page"), 1)
	perPage := min(positiveInt(q.Get("per_page"), 30), 100)
	last := max(1, (total+perPage-1)/perPage)
	page = min(page, last+1)
	lo = min((page-1)*perPage, total)
	hi = min(lo+perPage, total)

	var links []string
	link := func(p int, rel string) {
		q.Set("page", strconv.Itoa(p))
		links = append(links, fmt.Sprintf(`<%s%s?%s>; rel="%s"`, s.cfg.APIURL, r.URL.Path, q.Encode(), rel))
	}
	if page > 1 {
		link(min(page-1, last), "prev")
	}
	if page < last {
		link(page+1, "next")
		link(last, "last")
	}
	if page > 1 {
		link(1, "first")
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	return lo, hi
}

func positiveInt(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func pathInt(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n < 1 {
		return 0, errNotFound()
	}
	return n, nil
}
