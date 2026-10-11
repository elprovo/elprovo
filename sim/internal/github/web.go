package github

import (
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	sessionCookie = "user_session"
	maxSessions   = 1000
)

type pageUser struct {
	ID    int64
	Login string
}

type page struct {
	Title string
	User  *pageUser
	Data  any
}

type messageData struct {
	Text  string
	Error bool
}

func parsePages() (*template.Template, error) {
	return template.ParseFS(templateFS, "templates/*.html")
}

func (s *Sim) webHandler() http.Handler {
	admin := s.adminHandler()
	ui := s.uiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_sim/") {
			admin.ServeHTTP(w, r)
			return
		}
		if owner, name, rest, ok := gitRequestPath(r.URL.Path); ok {
			s.serveGit(w, r, owner, name, rest)
			return
		}
		ui.ServeHTTP(w, r)
	})
}

func (s *Sim) uiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /session", s.signIn)
	mux.HandleFunc("POST /signup", s.signUp)
	mux.HandleFunc("POST /logout", s.signOut)
	mux.HandleFunc("GET /avatars/u/{id}", s.avatar)
	mux.HandleFunc("GET /codeload/{owner}/{repo}/legacy.tar.gz/{sha}", s.serveArchive)

	mux.HandleFunc("GET /login/oauth/authorize", s.authorizePage)
	mux.HandleFunc("POST /login/oauth/authorize", s.authorizeDecision)
	mux.HandleFunc("POST /login/oauth/access_token", s.accessToken)
	mux.HandleFunc("GET /apps/{slug}/installations/new", s.installPage)
	mux.HandleFunc("POST /apps/{slug}/installations/new", s.installSubmit)

	mux.HandleFunc("GET /new", s.newRepoPage)
	mux.HandleFunc("POST /new", s.newRepoSubmit)
	mux.HandleFunc("GET /orgs/{org}/people", s.peoplePage)
	mux.HandleFunc("POST /orgs/{org}/people", s.peopleSubmit)
	mux.HandleFunc("GET /{owner}/{repo}", s.repoPage)
	mux.HandleFunc("POST /{owner}/{repo}/pulls", s.createPullForm)
	mux.HandleFunc("GET /{owner}/{repo}/pull/{number}", s.pullPage)
	mux.HandleFunc("POST /{owner}/{repo}/pull/{number}/comments", s.commentForm)
	mux.HandleFunc("POST /{owner}/{repo}/pull/{number}/labels", s.addLabelForm)
	mux.HandleFunc("POST /{owner}/{repo}/pull/{number}/labels/delete", s.removeLabelForm)
	mux.HandleFunc("POST /{owner}/{repo}/pull/{number}/state", s.pullStateForm)
	mux.HandleFunc("POST /{owner}/{repo}/pull/{number}/merge", s.mergeForm)
	return mux
}

func (s *Sim) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.pages.ExecuteTemplate(w, name, page{Title: title, User: s.currentUser(r), Data: data}); err != nil {
		s.log.Error("render page", "page", name, "error", err)
	}
}

func (s *Sim) renderError(w http.ResponseWriter, r *http.Request, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		e = &apiError{status: http.StatusInternalServerError, message: "Something went wrong."}
	}
	s.render(w, r, e.status, "message", http.StatusText(e.status), messageData{Text: e.message, Error: true})
}

func (s *Sim) currentUser(r *http.Request) *pageUser {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var u *pageUser
	s.read(func(v view) {
		i := slices.IndexFunc(v.w.Sessions, func(se session) bool { return se.ID == cookie.Value })
		if i < 0 {
			return
		}
		if a := v.w.account(v.w.Sessions[i].UserID); a != nil {
			u = &pageUser{ID: a.ID, Login: a.Login}
		}
	})
	return u
}

func (s *Sim) requireUser(w http.ResponseWriter, r *http.Request) *pageUser {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	}
	return u
}

func (s *Sim) startSession(w http.ResponseWriter, userID int64) error {
	id := rand.Text()
	err := s.update(func(t *tx) error {
		t.w.Sessions = append(t.w.Sessions, session{ID: id, UserID: userID})
		if over := len(t.w.Sessions) - maxSessions; over > 0 {
			t.w.Sessions = t.w.Sessions[over:]
		}
		return nil
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return nil
}

func safeReturnTo(s string) string {
	u, err := url.Parse(s)
	unsafe := strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' })
	if err != nil || unsafe || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return "/"
	}
	return s
}

type loginData struct {
	Users    []account
	ReturnTo string
}

func (s *Sim) loginPage(w http.ResponseWriter, r *http.Request) {
	data := loginData{ReturnTo: safeReturnTo(r.URL.Query().Get("return_to"))}
	s.read(func(v view) {
		for _, a := range v.w.Accounts {
			if a.Type == typeUser {
				data.Users = append(data.Users, *a)
			}
		}
	})
	s.render(w, r, http.StatusOK, "login", "Sign in", data)
}

func (s *Sim) signIn(w http.ResponseWriter, r *http.Request) {
	var userID int64
	s.read(func(v view) {
		if u := v.w.user(r.PostFormValue("login")); u != nil {
			userID = u.ID
		}
	})
	if userID == 0 {
		s.renderError(w, r, fail(http.StatusNotFound, "User %s does not exist.", r.PostFormValue("login")))
		return
	}
	s.finishSignIn(w, r, userID)
}

func (s *Sim) signUp(w http.ResponseWriter, r *http.Request) {
	var userID int64
	err := s.update(func(t *tx) error {
		a, err := t.createAccount(r.PostFormValue("login"), r.PostFormValue("name"), r.PostFormValue("email"), typeUser)
		if err == nil {
			userID = a.ID
		}
		return err
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	s.finishSignIn(w, r, userID)
}

func (s *Sim) finishSignIn(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := s.startSession(w, userID); err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, safeReturnTo(r.PostFormValue("return_to")), http.StatusSeeOther)
}

func (s *Sim) signOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		err := s.update(func(t *tx) error {
			t.w.Sessions = slices.DeleteFunc(t.w.Sessions, func(se session) bool { return se.ID == cookie.Value })
			return nil
		})
		if err != nil {
			s.renderError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Sim) avatar(w http.ResponseWriter, r *http.Request) {
	initial := "?"
	if id, err := pathInt(r, "id"); err == nil {
		s.read(func(v view) { initial = v.initial(id) })
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(avatarSVG(initial))
}

func avatarSVG(initial string) []byte {
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="#57606a"/>`+
		`<text x="32" y="42" font-size="32" text-anchor="middle" fill="#fff" font-family="sans-serif">%s</text></svg>`, template.HTMLEscapeString(initial))
}
