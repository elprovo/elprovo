package github

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func contentsFixture(t *testing.T) (*testSim, string) {
	t.Helper()
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{
		".stagely.yml":        "services: [web]\n",
		"compose/base.yml":    "services:\n  web:\n    image: nginx\n",
		"compose/extra.yml":   "services: {}\n",
		"docs/guide/intro.md": "hello",
	})
	a := ts.createApp("octo", nil)
	return ts, ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)
}

func TestContentsReturnsFileAsBase64JSON(t *testing.T) {
	ts, tok := contentsFixture(t)

	r := ts.call(http.MethodGet, "/repos/octo/web/contents/compose/base.yml?ref=main", tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET contents = %d %s", r.status, r.body)
	}
	var got contentJSON
	r.decode(t, &got)
	if got.Type != "file" || got.Encoding != "base64" || got.Path != "compose/base.yml" || got.Name != "base.yml" || got.Content == nil {
		t.Fatalf("content = %+v, want file compose/base.yml in base64", got)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(*got.Content, "\n", ""))
	if err != nil || string(raw) != "services:\n  web:\n    image: nginx\n" {
		t.Fatalf("decoded content = %q (%v), want the file bytes", raw, err)
	}
}

func TestContentsReturnsRawBytesForRawMediaTypes(t *testing.T) {
	ts, tok := contentsFixture(t)

	for _, accept := range []string{"application/vnd.github.raw+json", "application/vnd.github.raw"} {
		r := do(t, http.DefaultClient, http.MethodGet, ts.api.URL+"/repos/octo/web/contents/.stagely.yml", nil,
			http.Header{"Authorization": {"Bearer " + tok}, "Accept": {accept}})
		if r.status != http.StatusOK || string(r.body) != "services: [web]\n" {
			t.Fatalf("Accept %s: %d %q, want the raw file", accept, r.status, r.body)
		}
	}
}

func TestContentsListsADirectory(t *testing.T) {
	ts, tok := contentsFixture(t)

	var got []contentJSON
	ts.call(http.MethodGet, "/repos/octo/web/contents/compose", tok, nil).decode(t, &got)
	var names []string
	for _, e := range got {
		names = append(names, e.Type+":"+e.Path)
		if e.Content != nil {
			t.Fatalf("directory entry %s carries content", e.Path)
		}
	}
	if want := []string{"file:compose/base.yml", "file:compose/extra.yml"}; !slices.Equal(names, want) {
		t.Fatalf("directory = %v, want %v", names, want)
	}

	var root []contentJSON
	ts.call(http.MethodGet, "/repos/octo/web/contents", tok, nil).decode(t, &root)
	if len(root) != 3 || root[0].Name != ".stagely.yml" || root[1].Type != "dir" {
		t.Fatalf("root listing = %+v, want .stagely.yml, compose and docs", root)
	}
}

func TestContentsAnswers404ForMissingPathsAndRefs(t *testing.T) {
	ts, tok := contentsFixture(t)

	for _, path := range []string{
		"/repos/octo/web/contents/missing.yml",
		"/repos/octo/web/contents/.stagely.yml/inside-a-file",
		"/repos/octo/web/contents/.stagely.yml?ref=no-such-branch",
	} {
		r := ts.call(http.MethodGet, path, tok, nil)
		var body errorBody
		r.decode(t, &body)
		if r.status != http.StatusNotFound || body.Status != "404" {
			t.Fatalf("GET %s = %d %s, want 404", path, r.status, r.body)
		}
	}
}

func TestTarballRedirectsToASignedArchiveWithTheRepoFolder(t *testing.T) {
	ts, tok := contentsFixture(t)
	var commit commitJSON
	ts.call(http.MethodGet, "/repos/octo/web/commits/main", tok, nil).decode(t, &commit)

	r := ts.call(http.MethodGet, "/repos/octo/web/tarball/"+commit.SHA, tok, nil)
	location := r.header.Get("Location")
	if r.status != http.StatusFound || !strings.HasPrefix(location, ts.web.URL+"/codeload/octo/web/legacy.tar.gz/") {
		t.Fatalf("tarball = %d Location %q, want 302 to the web host", r.status, location)
	}

	archive := do(t, http.DefaultClient, http.MethodGet, location, nil, nil)
	if archive.status != http.StatusOK {
		t.Fatalf("download archive = %d %s", archive.status, archive.body)
	}
	wantPrefix := "octo-web-" + commit.SHA[:7] + "/"
	names := tarNames(t, archive.body)
	if !slices.Contains(names, wantPrefix+"compose/base.yml") || !slices.Contains(names, wantPrefix+".stagely.yml") {
		t.Fatalf("archive entries = %v, want files under %s", names, wantPrefix)
	}

	tampered := strings.Replace(location, "token=", "token=0", 1)
	if r := do(t, http.DefaultClient, http.MethodGet, tampered, nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("download with a tampered token = %d, want 404", r.status)
	}
}

func tarNames(t *testing.T, gz []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(gz)))
	if err != nil {
		t.Fatalf("archive is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names = append(names, h.Name)
	}
}

func TestCommitByRefDescribesTheCommit(t *testing.T) {
	ts, tok := contentsFixture(t)

	var got commitJSON
	r := ts.call(http.MethodGet, "/repos/octo/web/commits/main", tok, nil)
	r.decode(t, &got)
	if r.status != http.StatusOK || len(got.SHA) != 40 || got.Commit.Message != "Initial commit" || got.Author == nil || got.Author.Login != "octo" {
		t.Fatalf("commit = %d %+v, want octo's initial commit", r.status, got)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/web/commits/nope", tok, nil); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown ref = %d, want 422", r.status)
	}
}

func TestListPullsFiltersByState(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.createRepo("octo", "web", map[string]string{"README.md": "x"})
	ts.pushBranch("octo", "web", "one", map[string]string{"one.txt": "1"})
	ts.pushBranch("octo", "web", "two", map[string]string{"two.txt": "2"})
	ts.openPull("octo", "web", "octo", "one")
	second := ts.openPull("octo", "web", "octo", "two")
	ts.admin(http.MethodPatch, "/_sim/repos/octo/web/pulls/2", map[string]string{"user": "octo", "state": "closed"}, http.StatusOK)
	a := ts.createApp("octo", nil)
	tok := ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)

	for query, want := range map[string][]int{"": {1}, "?state=closed": {second.Number}, "?state=all": {2, 1}} {
		var got []pullJSON
		ts.call(http.MethodGet, "/repos/octo/web/pulls"+query, tok, nil).decode(t, &got)
		var numbers []int
		for _, p := range got {
			numbers = append(numbers, p.Number)
		}
		if !slices.Equal(numbers, want) {
			t.Fatalf("pulls%s = %v, want %v", query, numbers, want)
		}
	}

	var one pullJSON
	ts.call(http.MethodGet, "/repos/octo/web/pulls/1", tok, nil).decode(t, &one)
	if one.Head.Ref != "one" || one.Base.Ref != "main" || one.State != "open" || len(one.Head.SHA) != 40 {
		t.Fatalf("pull 1 = %+v, want open one -> main", one)
	}
}

func TestCollaboratorPermissionReportsEachRole(t *testing.T) {
	ts := newTestSim(t)
	for _, login := range []string{"octo", "mona", "hubot"} {
		ts.createUser(login)
	}
	ts.createRepo("octo", "web", nil)
	ts.admin(http.MethodPut, "/_sim/repos/octo/web/collaborators/mona", map[string]string{"permission": "write"}, http.StatusNoContent)
	a := ts.createApp("octo", nil)
	tok := ts.installationToken(a, ts.install(a.Slug, "octo").ID, nil)

	for login, want := range map[string]string{"octo": "admin", "mona": "write", "hubot": "none"} {
		var got collaboratorPermissionJSON
		ts.call(http.MethodGet, "/repos/octo/web/collaborators/"+login+"/permission", tok, nil).decode(t, &got)
		if got.Permission != want || got.User.Login != login {
			t.Fatalf("%s permission = %q, want %q", login, got.Permission, want)
		}
	}
	if r := ts.call(http.MethodGet, "/repos/octo/web/collaborators/ghost/permission", tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", r.status)
	}
}

func TestPublicEndpointsWorkWithoutAuthentication(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	ts.admin(http.MethodPost, "/_sim/repos", map[string]any{"owner": "octo", "name": "site"}, http.StatusCreated)

	var u fullUserJSON
	if r := ts.call(http.MethodGet, "/users/octo", "", nil); r.status != http.StatusOK {
		t.Fatalf("GET /users/octo = %d", r.status)
	} else {
		r.decode(t, &u)
	}
	if u.Login != "octo" || u.Type != "User" || u.PublicRepos != 1 {
		t.Fatalf("user = %+v, want octo with one public repository", u)
	}
	if r := ts.call(http.MethodGet, "/repos/octo/site", "", nil); r.status != http.StatusOK {
		t.Fatalf("anonymous GET public repo = %d, want 200", r.status)
	}
	if r := ts.call(http.MethodGet, "/user", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /user = %d, want 401", r.status)
	}
	if r := ts.call(http.MethodGet, "/users/"+url.PathEscape("nobody"), "", nil); r.status != http.StatusNotFound {
		t.Fatalf("GET unknown user = %d, want 404", r.status)
	}
}
