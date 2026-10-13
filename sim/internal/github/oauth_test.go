package github

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const callbackURL = "https://elprovo.test/auth/github/callback"

func oauthFixture(t *testing.T) (*testSim, appCredentials) {
	t.Helper()
	ts := newTestSim(t)
	ts.createUser("octo")
	return ts, ts.createApp("octo", map[string]any{"callback_urls": []string{callbackURL, "https://other.test/cb"}})
}

func authorizeURL(ts *testSim, a appCredentials, redirect, state string) string {
	q := url.Values{"client_id": {a.ClientID}, "state": {state}}
	if redirect != "" {
		q.Set("redirect_uri", redirect)
	}
	return ts.web.URL + "/login/oauth/authorize?" + q.Encode()
}

func redirectQuery(t *testing.T, r response, wantPrefix string) url.Values {
	t.Helper()
	loc := r.header.Get("Location")
	if r.status != http.StatusFound || !strings.HasPrefix(loc, wantPrefix) {
		t.Fatalf("response = %d Location %q, want 302 to %s", r.status, loc, wantPrefix)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u.Query()
}

func exchange(t *testing.T, ts *testSim, form url.Values) map[string]any {
	t.Helper()
	r := do(t, http.DefaultClient, http.MethodPost, ts.web.URL+"/login/oauth/access_token", strings.NewReader(form.Encode()),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}})
	if r.status != http.StatusOK {
		t.Fatalf("access_token = %d %s, want 200", r.status, r.body)
	}
	var out map[string]any
	r.decode(t, &out)
	return out
}

func authorizedCode(t *testing.T, ts *testSim, a appCredentials, client *http.Client) string {
	t.Helper()
	r := do(t, client, http.MethodGet, authorizeURL(ts, a, callbackURL, "s1"), nil, nil)
	if r.status == http.StatusOK {
		r = postForm(t, client, ts.web.URL+"/login/oauth/authorize",
			url.Values{"client_id": {a.ClientID}, "redirect_uri": {callbackURL}, "state": {"s1"}, "decision": {"authorize"}})
	}
	return redirectQuery(t, r, callbackURL).Get("code")
}

func TestWebFlowSignsInAuthorizesAndIssuesUserTokens(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)

	r := do(t, client, http.MethodGet, authorizeURL(ts, a, callbackURL, "xyz"), nil, nil)
	loginQuery := redirectQuery(t, r, "/login?")
	ts.signIn(client, "octo")

	consent := do(t, client, http.MethodGet, ts.web.URL+loginQuery.Get("return_to"), nil, nil)
	if consent.status != http.StatusOK || !strings.Contains(string(consent.body), "Authorize Elprovo Dev") {
		t.Fatalf("consent page = %d, want the Authorize Elprovo Dev page", consent.status)
	}
	r = postForm(t, client, ts.web.URL+"/login/oauth/authorize",
		url.Values{"client_id": {a.ClientID}, "redirect_uri": {callbackURL}, "state": {"xyz"}, "decision": {"authorize"}})
	q := redirectQuery(t, r, callbackURL+"?")
	if q.Get("state") != "xyz" || q.Get("code") == "" {
		t.Fatalf("callback query = %v, want the code and state xyz", q)
	}

	tokens := exchange(t, ts, url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {q.Get("code")}, "redirect_uri": {callbackURL}})
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if !strings.HasPrefix(access, "ghu_") || !strings.HasPrefix(refresh, "ghr_") || tokens["expires_in"] != 28800.0 ||
		tokens["refresh_token_expires_in"] != 15897600.0 || tokens["token_type"] != "bearer" || tokens["scope"] != "" {
		t.Fatalf("token response = %v, want ghu_/ghr_ tokens with GitHub's lifetimes", tokens)
	}
	var me fullUserJSON
	ts.call(http.MethodGet, "/user", access, nil).decode(t, &me)
	if me.Login != "octo" || me.Email == nil || *me.Email != "octo@example.com" {
		t.Fatalf("GET /user = %+v, want octo with email", me)
	}
	avatar, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(me.AvatarURL, "data:image/svg+xml;base64,"))
	if !strings.HasPrefix(me.AvatarURL, "data:image/svg+xml;base64,") || err != nil || !strings.Contains(string(avatar), ">O</text>") {
		t.Fatalf("avatar_url = %q, want an inline SVG with the initial O, which a strict Content-Security-Policy allows", me.AvatarURL)
	}

	again := do(t, client, http.MethodGet, authorizeURL(ts, a, callbackURL, "second"), nil, nil)
	if code := redirectQuery(t, again, callbackURL).Get("code"); code == "" {
		t.Fatal("a second authorization asked for consent again instead of returning a code")
	}
}

func TestAccessTokenIsFormEncodedWithoutAcceptJSON(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")
	code := authorizedCode(t, ts, a, client)

	r := postForm(t, http.DefaultClient, ts.web.URL+"/login/oauth/access_token",
		url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {code}})
	values, err := url.ParseQuery(string(r.body))
	if err != nil || !strings.HasPrefix(values.Get("access_token"), "ghu_") || values.Get("token_type") != "bearer" || values.Get("expires_in") != "28800" {
		t.Fatalf("form response %q, want an urlencoded ghu_ token", r.body)
	}
}

func TestAccessTokenAcceptsJSONBodiesAndBasicClientAuth(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")

	for _, tc := range []struct {
		name    string
		request func(code string) (io.Reader, http.Header)
	}{
		{"JSON body", func(code string) (io.Reader, http.Header) {
			return jsonBody(t, map[string]string{"client_id": a.ClientID, "client_secret": a.ClientSecret, "code": code}),
				http.Header{"Content-Type": {"application/json"}}
		}},
		{"client credentials in Basic auth", func(code string) (io.Reader, http.Header) {
			auth := base64.StdEncoding.EncodeToString([]byte(a.ClientID + ":" + a.ClientSecret))
			return strings.NewReader(url.Values{"code": {code}}.Encode()),
				http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Authorization": {"Basic " + auth}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, header := tc.request(authorizedCode(t, ts, a, client))
			header.Set("Accept", "application/json")
			var got oauthResult
			do(t, http.DefaultClient, http.MethodPost, ts.web.URL+"/login/oauth/access_token", body, header).decode(t, &got)
			if !strings.HasPrefix(got.AccessToken, "ghu_") {
				t.Fatalf("token response = %+v, want a ghu_ token", got)
			}
		})
	}
}

func TestAuthorizeRedirectsMismatchedRedirectURIWithAnError(t *testing.T) {
	ts, a := oauthFixture(t)

	r := do(t, browser(t), http.MethodGet, authorizeURL(ts, a, "https://evil.test/cb", "s9"), nil, nil)
	q := redirectQuery(t, r, callbackURL+"?")
	if q.Get("error") != "redirect_uri_mismatch" || q.Get("state") != "s9" || q.Get("code") != "" {
		t.Fatalf("mismatch redirect query = %v, want error=redirect_uri_mismatch and state s9", q)
	}
}

func TestAuthorizeUsesTheFirstCallbackWhenRedirectURIIsOmitted(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")

	postForm(t, client, ts.web.URL+"/login/oauth/authorize",
		url.Values{"client_id": {a.ClientID}, "state": {"s"}, "decision": {"authorize"}})
	r := do(t, client, http.MethodGet, authorizeURL(ts, a, "", "s"), nil, nil)
	redirectQuery(t, r, callbackURL+"?")
}

func TestDeniedAuthorizationRedirectsWithAccessDenied(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")

	r := postForm(t, client, ts.web.URL+"/login/oauth/authorize",
		url.Values{"client_id": {a.ClientID}, "redirect_uri": {callbackURL}, "state": {"s2"}, "decision": {"cancel"}})
	q := redirectQuery(t, r, callbackURL+"?")
	if q.Get("error") != "access_denied" || q.Get("state") != "s2" {
		t.Fatalf("cancel redirect query = %v, want error=access_denied", q)
	}
}

func TestAccessTokenErrorsFollowGitHub(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")
	used := authorizedCode(t, ts, a, client)
	exchange(t, ts, url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {used}})

	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"wrong secret", url.Values{"client_id": {a.ClientID}, "client_secret": {"nope"}, "code": {"x"}}, "incorrect_client_credentials"},
		{"unknown code", url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {"nope"}}, "bad_verification_code"},
		{"reused code", url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {used}}, "bad_verification_code"},
		{"other redirect_uri", url.Values{
			"client_id": {a.ClientID}, "client_secret": {a.ClientSecret},
			"code": {authorizedCode(t, ts, a, client)}, "redirect_uri": {"https://other.test/cb"},
		}, "redirect_uri_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := exchange(t, ts, tc.form)
			if got["error"] != tc.want || got["error_description"] == "" || got["access_token"] != nil {
				t.Fatalf("response = %v, want error %s", got, tc.want)
			}
		})
	}
}

func TestRefreshTokenRotatesBothTokens(t *testing.T) {
	ts, a := oauthFixture(t)
	client := browser(t)
	ts.signIn(client, "octo")
	first := exchange(t, ts, url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {authorizedCode(t, ts, a, client)}})
	refreshForm := url.Values{
		"client_id": {a.ClientID}, "client_secret": {a.ClientSecret},
		"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)},
	}

	second := exchange(t, ts, refreshForm)
	newAccess, _ := second["access_token"].(string)
	if !strings.HasPrefix(newAccess, "ghu_") || newAccess == first["access_token"] || second["refresh_token"] == first["refresh_token"] {
		t.Fatalf("refresh response = %v, want new ghu_ and ghr_ tokens", second)
	}
	if r := ts.call(http.MethodGet, "/user", first["access_token"].(string), nil); r.status != http.StatusUnauthorized {
		t.Fatalf("old access token after refresh = %d, want 401", r.status)
	}
	if r := ts.call(http.MethodGet, "/user", newAccess, nil); r.status != http.StatusOK {
		t.Fatalf("new access token = %d, want 200", r.status)
	}
	if again := exchange(t, ts, refreshForm); again["error"] != "bad_refresh_token" {
		t.Fatalf("reused refresh token = %v, want bad_refresh_token", again)
	}
}

func TestUserTokenListsTheUsersInstallationsOfTheApp(t *testing.T) {
	ts, a := oauthFixture(t)
	ts.createUser("mona")
	ts.createOrg("acme", "octo")
	ts.createRepo("acme", "web", nil)
	ts.createRepo("mona", "private", nil)
	acme := ts.install(a.Slug, "acme")
	ts.install(a.Slug, "mona")
	client := browser(t)
	ts.signIn(client, "octo")
	tokens := exchange(t, ts, url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {authorizedCode(t, ts, a, client)}})
	tok := tokens["access_token"].(string)

	var list userInstallationsJSON
	ts.call(http.MethodGet, "/user/installations", tok, nil).decode(t, &list)
	if list.TotalCount != 1 || list.Installations[0].ID != acme.ID || list.Installations[0].Account.Login != "acme" {
		t.Fatalf("user installations = %+v, want only the acme installation", list)
	}
	var repos installationReposJSON
	ts.call(http.MethodGet, "/user/installations/"+strconv.FormatInt(acme.ID, 10)+"/repositories", tok, nil).decode(t, &repos)
	if repos.TotalCount != 1 || repos.Repositories[0].FullName != "acme/web" || repos.Repositories[0].Permissions == nil || !repos.Repositories[0].Permissions.Admin {
		t.Fatalf("installation repositories = %+v, want acme/web with admin permission", repos)
	}
	if r := ts.call(http.MethodGet, "/repos/acme/web", tok, nil); r.status != http.StatusOK {
		t.Fatalf("user token on an installed repository = %d, want 200", r.status)
	}
}
