package aws

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/registrytoken"
)

// pullPolicy is the session policy of a stagelet machine's registry token:
// pull from the repositories under acme/ in us-east-1.
func pullPolicy(account string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"},` +
		`{"Effect":"Allow","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer","ecr:BatchCheckLayerAvailability"],` +
		`"Resource":"arn:aws:ecr:us-east-1:` + account + `:repository/acme/*"}]}`
}

// registryAccess asks the registry's token service for the scope, signing
// in with the user and password as Docker does, and returns the status and
// the access the token grants.
func (e *testEnv) registryAccess(user, password, service, scope string) (int, []registrytoken.Access) {
	e.t.Helper()
	q := url.Values{"service": {service}, "scope": {scope}}
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.server.URL+"/v2/token?"+q.Encode(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.SetBasicAuth(user, password)
	status, body := e.do(req)
	if status != http.StatusOK {
		return status, nil
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || strings.Count(resp.Token, ".") != 2 {
		e.t.Fatalf("token service answered %s: %v", body, err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(resp.Token, ".")[1])
	var claims struct {
		Access []registrytoken.Access `json:"access"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		e.t.Fatalf("token claims %s: %v", payload, err)
	}
	return status, claims.Access
}

// actions returns the actions the access grants on the repository.
func actions(access []registrytoken.Access, repository string) []string {
	for _, a := range access {
		if a.Type == "repository" && a.Name == repository {
			return a.Actions
		}
	}
	return nil
}

func TestRegistryTokenOfAPullOnlySessionPullsButCannotPush(t *testing.T) {
	e := newEnv(t)
	account := e.callerAccount()
	creds, _ := e.federationToken("stagely-registry-pull", pullPolicy(account))
	_, out := e.ecrAs(creds, "GetAuthorizationToken", map[string]any{})
	user, password := e.authorizationToken(out)
	registry := account + ".dkr.ecr.us-east-1.amazonaws.com"

	status, access := e.registryAccess(user, password, registry, "repository:acme/octo/app/web:pull,push")

	if got := actions(access, "acme/octo/app/web"); status != http.StatusOK || !slices.Equal(got, []string{"pull"}) {
		t.Fatalf("token service = %d, actions %v; want pull only", status, got)
	}
	if _, access := e.registryAccess(user, password, registry, "repository:other/web:pull"); len(access) != 0 {
		t.Fatalf("access outside acme/ = %+v, want none", access)
	}
}

func TestRegistryTokenOfTheIAMUserPushesAndPulls(t *testing.T) {
	e := newEnv(t)
	account := e.callerAccount()
	user, password := e.authorizationToken(e.mustECR("GetAuthorizationToken"))

	_, access := e.registryAccess(user, password, account+".dkr.ecr.us-east-1.amazonaws.com", "repository:acme/web:pull,push")

	if got := actions(access, "acme/web"); !slices.Equal(got, []string{"pull", "push"}) {
		t.Fatalf("actions = %v, want pull and push", got)
	}
}

func TestRegistryTokenWorksOnlyAtTheRegistryOfItsRegion(t *testing.T) {
	e := newEnv(t)
	account := e.callerAccount()
	user, password := e.authorizationToken(e.mustECR("GetAuthorizationToken"))

	status, access := e.registryAccess(user, password, account+".dkr.ecr.eu-central-1.amazonaws.com", "repository:acme/web:pull")

	if status != http.StatusOK || len(access) != 0 {
		t.Fatalf("token service for another region's registry = %d %+v, want a token without access", status, access)
	}
}

func TestRegistryTokenServiceRefusesUnknownAndExpiredPasswords(t *testing.T) {
	e := newEnv(t)
	registry := e.callerAccount() + ".dkr.ecr.us-east-1.amazonaws.com"
	user, password := e.authorizationToken(e.mustECR("GetAuthorizationToken"))

	if status, _ := e.registryAccess(user, "not-a-token", registry, "repository:acme/web:pull"); status != http.StatusUnauthorized {
		t.Errorf("unknown password = %d, want 401", status)
	}
	if status, _ := e.registryAccess("admin", password, registry, "repository:acme/web:pull"); status != http.StatusUnauthorized {
		t.Errorf("a user other than AWS = %d, want 401", status)
	}
	e.clock.advance(12 * time.Hour)
	if status, _ := e.registryAccess(user, password, registry, "repository:acme/web:pull"); status != http.StatusUnauthorized {
		t.Errorf("password after 12 hours = %d, want 401", status)
	}
}
