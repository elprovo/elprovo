package gcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

// ownerToken returns an access token of the project's own service account.
func (e *testEnv) ownerToken() string {
	e.t.Helper()
	k := e.keyFile()
	_, body := e.exchange(assertion(e.t, k, standardClaims(k, time.Now())))
	return body["access_token"].(string)
}

func (e *testEnv) callAs(token string, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	return e.do(method, path, token, reader, "application/json")
}

// registryAccount makes a service account that the project's own account may
// impersonate, and gives it role on the repository images.
func (e *testEnv) registryAccount(id, role string) string {
	e.t.Helper()
	var account accountJSON
	e.mustCall(http.StatusOK, http.MethodPost, "/v1/projects/"+testProject+"/serviceAccounts", map[string]any{"accountId": id}, &account)
	owner := "serviceAccount:" + e.keyFile().ClientEmail
	var current policyJSON
	e.mustCall(http.StatusOK, http.MethodPost, "/v1/projects/"+testProject+"/serviceAccounts/"+account.Email+":getIamPolicy", map[string]any{}, &current)
	e.mustCall(http.StatusOK, http.MethodPost, "/v1/projects/"+testProject+"/serviceAccounts/"+account.Email+":setIamPolicy", map[string]any{
		"policy": map[string]any{"etag": current.Etag, "bindings": []binding{{Role: tokenCreatorRole, Members: []string{owner}}}},
	}, nil)
	var repoPolicy policyJSON
	e.mustCall(http.StatusOK, http.MethodGet, repositoriesPath+"/images:getIamPolicy", nil, &repoPolicy)
	repoPolicy.Bindings = append(repoPolicy.Bindings, binding{Role: role, Members: []string{"serviceAccount:" + account.Email}})
	e.mustCall(http.StatusOK, http.MethodPost, repositoriesPath+"/images:setIamPolicy", map[string]any{"policy": repoPolicy}, nil)
	return account.Email
}

func (e *testEnv) impersonate(token, email string, lifetime string) (int, map[string]string) {
	e.t.Helper()
	status, data := e.callAs(token, http.MethodPost, "/v1/projects/-/serviceAccounts/"+email+":generateAccessToken",
		map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}, "lifetime": lifetime})
	var out map[string]string
	_ = json.Unmarshal(data, &out)
	return status, out
}

func TestRegistryAccountsGetOnlyTheirRoleOnTheRepository(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusOK, http.MethodPost, repositoriesPath+"?repositoryId=images", map[string]any{"format": "DOCKER"}, nil)
	pusher := e.registryAccount("images-push", "roles/artifactregistry.writer")
	puller := e.registryAccount("images-pull", "roles/artifactregistry.reader")
	owner := e.ownerToken()
	scope := "repository:" + testProject + "/images/app:pull,push"
	tests := []struct {
		email string
		want  []string
	}{
		{pusher, []string{"pull", "push"}},
		{puller, []string{"pull"}},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			status, token := e.impersonate(owner, tt.email, "3600s")
			if status != http.StatusOK || token["accessToken"] == "" || token["expireTime"] == "" {
				t.Fatalf("generateAccessToken = %d %v", status, token)
			}

			_, body := e.registryToken("oauth2accesstoken", token["accessToken"], scope)

			_, claims := e.verifyRegistryToken(body["token"].(string))
			if access := accessOf(claims); len(access) != 1 || !slices.Equal(access[0].Actions, tt.want) || claims["sub"] != tt.email {
				t.Fatalf("access = %+v for %v, want %v", access, claims["sub"], tt.want)
			}
			if status, _ := e.callAs(token["accessToken"], http.MethodGet, computePath+"/zones/"+testZone+"/instances", nil); status != http.StatusForbidden {
				t.Fatalf("the registry account's token listed instances: %d, want 403", status)
			}
		})
	}
}

func TestImpersonationNeedsTheTokenCreatorRole(t *testing.T) {
	e := newEnv(t)
	var account accountJSON
	e.mustCall(http.StatusOK, http.MethodPost, "/v1/projects/"+testProject+"/serviceAccounts", map[string]any{"accountId": "nobody-may"}, &account)

	status, _ := e.impersonate(e.ownerToken(), account.Email, "3600s")

	if status != http.StatusForbidden {
		t.Fatalf("generateAccessToken without the role = %d, want 403", status)
	}
}

func TestImpersonatedTokensLastAnHourAtMost(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusOK, http.MethodPost, repositoriesPath+"?repositoryId=images", map[string]any{"format": "DOCKER"}, nil)
	pusher := e.registryAccount("images-push", "roles/artifactregistry.writer")

	status, _ := e.impersonate(e.ownerToken(), pusher, "7200s")

	if status != http.StatusBadRequest {
		t.Fatalf("generateAccessToken for two hours = %d, want 400", status)
	}
}

func TestSetIamPolicyRejectsAStaleEtag(t *testing.T) {
	e := newEnv(t)
	var account accountJSON
	e.mustCall(http.StatusOK, http.MethodPost, "/v1/projects/"+testProject+"/serviceAccounts", map[string]any{"accountId": "images-push"}, &account)
	path := "/v1/projects/" + testProject + "/serviceAccounts/" + account.Email
	var first policyJSON
	e.mustCall(http.StatusOK, http.MethodPost, path+":getIamPolicy", map[string]any{}, &first)
	e.mustCall(http.StatusOK, http.MethodPost, path+":setIamPolicy", map[string]any{"policy": map[string]any{"etag": first.Etag}}, nil)

	got := e.fail(http.StatusConflict, http.MethodPost, path+":setIamPolicy", map[string]any{"policy": map[string]any{"etag": first.Etag}})

	if got.Error.Errors[0].Reason != "aborted" {
		t.Fatalf("error = %+v", got)
	}
}
