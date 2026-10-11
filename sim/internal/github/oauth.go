package github

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const oauthErrorDocs = "https://docs.github.com/apps/managing-oauth-apps/troubleshooting-oauth-app-access-token-request-errors/"

type oauthApp struct {
	id           int64
	name         string
	clientID     string
	callbackURLs []string
}

func (s *Sim) oauthApp(clientID string) (oauthApp, bool) {
	var a oauthApp
	found := false
	s.read(func(v view) {
		if app := v.w.appByClientID(clientID); app != nil && clientID != "" {
			a, found = oauthApp{id: app.ID, name: app.Name, clientID: app.ClientID, callbackURLs: app.CallbackURLs}, true
		}
	})
	return a, found
}

func (a oauthApp) redirectURI(requested string) (string, bool) {
	if requested == "" && len(a.callbackURLs) > 0 {
		return a.callbackURLs[0], true
	}
	return requested, slices.Contains(a.callbackURLs, requested)
}

func redirectWith(w http.ResponseWriter, r *http.Request, target string, params url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid redirect URL", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range params {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

type authorizeData struct {
	AppName     string
	ClientID    string
	RedirectURI string
	State       string
}

func (s *Sim) checkAuthorizeRequest(w http.ResponseWriter, r *http.Request, clientID, requested, state string) (oauthApp, string, bool) {
	a, ok := s.oauthApp(clientID)
	if !ok {
		s.renderError(w, r, fail(http.StatusNotFound, "No GitHub App has the client ID %q.", clientID))
		return a, "", false
	}
	redirect, ok := a.redirectURI(requested)
	if ok {
		return a, redirect, true
	}
	if len(a.callbackURLs) == 0 {
		s.renderError(w, r, fail(http.StatusBadRequest, "The redirect_uri is not associated with this application."))
		return a, "", false
	}
	redirectWith(w, r, a.callbackURLs[0], url.Values{
		"error":             {"redirect_uri_mismatch"},
		"error_description": {"The redirect_uri MUST match the registered callback URL for this application."},
		"error_uri":         {"https://docs.github.com/apps/managing-oauth-apps/troubleshooting-authorization-request-errors/#redirect-uri-mismatch"},
		"state":             {state},
	})
	return a, "", false
}

func (s *Sim) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, redirect, ok := s.checkAuthorizeRequest(w, r, q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"))
	if !ok {
		return
	}
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	authorized := false
	s.read(func(v view) { authorized = v.w.authorized(u.ID, a.id) })
	if authorized {
		s.redirectWithCode(w, r, a, u.ID, redirect, q.Get("state"))
		return
	}
	s.render(w, r, http.StatusOK, "authorize", "Authorize "+a.name, authorizeData{
		AppName: a.name, ClientID: a.clientID, RedirectURI: redirect, State: q.Get("state"),
	})
}

func (s *Sim) authorizeDecision(w http.ResponseWriter, r *http.Request) {
	state := r.PostFormValue("state")
	a, redirect, ok := s.checkAuthorizeRequest(w, r, r.PostFormValue("client_id"), r.PostFormValue("redirect_uri"), state)
	if !ok {
		return
	}
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if r.PostFormValue("decision") != "authorize" {
		redirectWith(w, r, redirect, url.Values{
			"error":             {"access_denied"},
			"error_description": {"The user has denied your application access."},
			"error_uri":         {"https://docs.github.com/apps/managing-oauth-apps/troubleshooting-authorization-request-errors/#access-denied"},
			"state":             {state},
		})
		return
	}
	err := s.update(func(t *tx) error {
		if !t.w.authorized(u.ID, a.id) {
			t.w.Authorizations = append(t.w.Authorizations, authorization{UserID: u.ID, AppID: a.id, CreatedAt: now()})
		}
		return nil
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	s.redirectWithCode(w, r, a, u.ID, redirect, state)
}

func (s *Sim) redirectWithCode(w http.ResponseWriter, r *http.Request, a oauthApp, userID int64, redirect, state string) {
	code := randomHex(10)
	err := s.update(func(t *tx) error {
		t.w.pruneTokens()
		t.w.Codes = append(t.w.Codes, &authCode{Code: code, AppID: a.id, UserID: userID, RedirectURI: redirect, ExpiresAt: now().Add(authCodeTTL)})
		return nil
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	redirectWith(w, r, redirect, url.Values{"code": {code}, "state": {state}})
}

type oauthResult struct {
	AccessToken           string `json:"access_token,omitempty"`
	ExpiresIn             int    `json:"expires_in,omitempty"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in,omitempty"`
	Scope                 string `json:"scope"`
	TokenType             string `json:"token_type,omitempty"`
}

type oauthFailure struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorURI         string `json:"error_uri"`
}

func (f oauthFailure) form() url.Values {
	return url.Values{"error": {f.Error}, "error_description": {f.ErrorDescription}, "error_uri": {f.ErrorURI}}
}

func (o oauthResult) form() url.Values {
	return url.Values{
		"access_token":             {o.AccessToken},
		"expires_in":               {strconv.Itoa(o.ExpiresIn)},
		"refresh_token":            {o.RefreshToken},
		"refresh_token_expires_in": {strconv.Itoa(o.RefreshTokenExpiresIn)},
		"scope":                    {o.Scope},
		"token_type":               {o.TokenType},
	}
}

var (
	errBadClient = oauthFailure{
		Error: "incorrect_client_credentials", ErrorDescription: "The client_id and/or client_secret passed are incorrect.",
		ErrorURI: oauthErrorDocs + "#incorrect-client-credentials",
	}
	errBadCode = oauthFailure{
		Error: "bad_verification_code", ErrorDescription: "The code passed is incorrect or expired.",
		ErrorURI: oauthErrorDocs + "#bad-verification-code",
	}
	errRedirectMismatch = oauthFailure{
		Error: "redirect_uri_mismatch", ErrorDescription: "The redirect_uri MUST match the registered callback URL for this application.",
		ErrorURI: "https://docs.github.com/apps/managing-oauth-apps/troubleshooting-authorization-request-errors/#redirect-uri-mismatch2",
	}
	errBadRefresh = oauthFailure{
		Error: "bad_refresh_token", ErrorDescription: "The refresh token passed is incorrect or expired.",
		ErrorURI: oauthErrorDocs + "#bad-refresh-token",
	}
)

func (s *Sim) accessToken(w http.ResponseWriter, r *http.Request) {
	params, err := tokenParams(r)
	if err != nil {
		http.Error(w, "Problems parsing the request", http.StatusBadRequest)
		return
	}
	var result oauthResult
	var failure *oauthFailure
	err = s.update(func(t *tx) error {
		result, failure = t.exchange(params)
		return nil
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	if failure != nil {
		writeOAuth(w, r, *failure, failure.form())
		return
	}
	writeOAuth(w, r, result, result.form())
}

func tokenParams(r *http.Request) (url.Values, error) {
	params, err := tokenBody(r)
	if err != nil {
		return nil, err
	}
	if id, secret, ok := r.BasicAuth(); ok && params.Get("client_id") == "" {
		params.Set("client_id", id)
		params.Set("client_secret", secret)
	}
	return params, nil
}

func tokenBody(r *http.Request) (url.Values, error) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		err := r.ParseForm()
		return r.Form, err
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&body); err != nil {
		return nil, err
	}
	params := r.URL.Query()
	for k, v := range body {
		if s, ok := v.(string); ok {
			params.Set(k, s)
		}
	}
	return params, nil
}

func writeOAuth(w http.ResponseWriter, r *http.Request, v any, form url.Values) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, v)
		return
	}
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	_, _ = w.Write([]byte(form.Encode()))
}

func (t *tx) exchange(p url.Values) (oauthResult, *oauthFailure) {
	a := t.w.appByClientID(p.Get("client_id"))
	if a == nil || p.Get("client_id") == "" || a.ClientSecret != p.Get("client_secret") {
		return oauthResult{}, &errBadClient
	}
	t.w.pruneTokens()
	if p.Get("grant_type") == "refresh_token" {
		return t.refreshUserToken(a, p.Get("refresh_token"))
	}
	i := slices.IndexFunc(t.w.Codes, func(c *authCode) bool { return c.Code == p.Get("code") && c.AppID == a.ID })
	if i < 0 || p.Get("code") == "" {
		return oauthResult{}, &errBadCode
	}
	code := t.w.Codes[i]
	if redirect := p.Get("redirect_uri"); redirect != "" && redirect != code.RedirectURI {
		return oauthResult{}, &errRedirectMismatch
	}
	t.w.Codes = slices.Delete(t.w.Codes, i, i+1)
	return t.issueUserTokens(a.ID, code.UserID), nil
}

func (t *tx) refreshUserToken(a *app, value string) (oauthResult, *oauthFailure) {
	old := t.w.liveToken(value)
	if old == nil || old.Kind != kindRefresh || old.AppID != a.ID {
		return oauthResult{}, &errBadRefresh
	}
	t.w.revokeTokens(func(tok *token) bool { return tok.Value == old.Value || tok.Value == old.Pair })
	return t.issueUserTokens(a.ID, old.UserID), nil
}

func (t *tx) issueUserTokens(appID, userID int64) oauthResult {
	access := t.w.issueToken(token{Kind: kindUser, AppID: appID, UserID: userID}, userTokenTTL)
	refresh := t.w.issueToken(token{Kind: kindRefresh, AppID: appID, UserID: userID, Pair: access.Value}, refreshTokenTTL)
	access.Pair = refresh.Value
	return oauthResult{
		AccessToken:           access.Value,
		ExpiresIn:             int(userTokenTTL.Seconds()),
		RefreshToken:          refresh.Value,
		RefreshTokenExpiresIn: int(refreshTokenTTL.Seconds()),
		TokenType:             "bearer",
	}
}
