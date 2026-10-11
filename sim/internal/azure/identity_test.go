package azure

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestTokenEndpointIssuesAServicePrincipalToken(t *testing.T) {
	e := newEnv(t)

	token := e.issue("https://management.example/.default")

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q is not a JWT", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Audience != "https://management.example" || claims.TenantID != defaultTenant || claims.AppID != defaultClientID ||
		claims.ObjectID == "" || claims.IDType != "app" || claims.Expires <= claims.IssuedAt {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestTokenEndpointRefusesAWrongSecret(t *testing.T) {
	e := newEnv(t)

	status, body := e.tokenRequest(url.Values{"grant_type": {"client_credentials"}, "client_id": {defaultClientID}, "client_secret": {"wrong"}, "scope": {"https://x/.default"}})

	if status != http.StatusUnauthorized || body["error"] != "invalid_client" || !strings.HasPrefix(body["error_description"].(string), "AADSTS7000215") {
		t.Fatalf("token endpoint = %d %v, want 401 invalid_client", status, body)
	}
}

func TestTokenEndpointNeedsADefaultScope(t *testing.T) {
	e := newEnv(t)

	status, body := e.tokenRequest(url.Values{"grant_type": {"client_credentials"}, "client_id": {defaultClientID}, "client_secret": {testSecret}, "scope": {"https://x/read"}})

	if status != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("token endpoint = %d %v, want 400 invalid_scope", status, body)
	}
}

func TestResourceManagerChecksTheBearerToken(t *testing.T) {
	e := newEnv(t)
	path := "/subscriptions/" + defaultSubscription + "/resourceGroups" + apiVersion
	cases := []struct {
		name, token, code string
	}{
		{"missing", "", "AuthenticationFailed"},
		{"forged", "a.b.c", "InvalidAuthenticationToken"},
		{"issued for Microsoft Graph", e.issue(graphResource + "/.default"), "InvalidAuthenticationTokenAudience"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, headers, data := e.do(http.MethodGet, path, c.token, nil)
			var got armErrorBody
			_ = json.Unmarshal(data, &got)
			if status != http.StatusUnauthorized || got.Error.Code != c.code || headers.Get("WWW-Authenticate") == "" {
				t.Fatalf("GET = %d %s, want 401 %s with WWW-Authenticate", status, got.Error.Code, c.code)
			}
		})
	}
	if status, _, data := e.do(http.MethodGet, path, e.token, nil); status != http.StatusOK {
		t.Fatalf("GET with a token = %d: %s", status, data)
	}
}

func TestResourceManagerRequiresAnAPIVersion(t *testing.T) {
	e := newEnv(t)

	e.mustFail(http.StatusBadRequest, "MissingApiVersionParameter", http.MethodGet, "/subscriptions/"+defaultSubscription+"/resourceGroups", nil)
}

func TestAnotherSubscriptionIsNotFound(t *testing.T) {
	e := newEnv(t)

	e.mustFail(http.StatusNotFound, "SubscriptionNotFound", http.MethodGet, "/subscriptions/0000a2e0-0000-4000-8000-0000000000ff/resourceGroups"+apiVersion, nil)
}

func TestCloudMetadataPointsEveryEndpointAtTheSimulation(t *testing.T) {
	e := newEnv(t)

	status, _, data := e.do(http.MethodGet, "/metadata/endpoints?api-version=2022-09-01", "", nil)

	var doc struct {
		Name            string `json:"name"`
		ResourceManager string `json:"resourceManager"`
		Graph           string `json:"microsoftGraphResourceId"`
		Authentication  struct {
			LoginEndpoint string `json:"loginEndpoint"`
		} `json:"authentication"`
		Suffixes struct {
			ACR string `json:"acrLoginServer"`
		} `json:"suffixes"`
	}
	if err := json.Unmarshal(data, &doc); status != http.StatusOK || err != nil {
		t.Fatalf("metadata = %d %s", status, data)
	}
	if doc.ResourceManager != e.server.URL+"/" || doc.Authentication.LoginEndpoint != e.server.URL || doc.Name == "" || doc.Graph == "" || doc.Suffixes.ACR != "azurecr.io" {
		t.Fatalf("metadata = %+v", doc)
	}
}
