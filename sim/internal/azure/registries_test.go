package azure

import (
	"net/http"
	"testing"
)

func TestRegistryHandsOutItsAdminCredentials(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	path := resourcePath("rg", typeRegistry, "ElprovoAcme")
	var reg struct {
		Properties struct {
			LoginServer string `json:"loginServer"`
		} `json:"properties"`
	}

	e.mustCall(http.StatusCreated, http.MethodPut, path+apiVersion, map[string]any{
		"location": "westeurope", "sku": map[string]string{"name": "Basic"}, "properties": map[string]any{"adminUserEnabled": true},
	}, &reg)

	if reg.Properties.LoginServer != "elprovoacme.azurecr.io" {
		t.Fatalf("login server = %q", reg.Properties.LoginServer)
	}
	var creds struct {
		Username  string `json:"username"`
		Passwords []struct {
			Value string `json:"value"`
		} `json:"passwords"`
	}
	e.mustCall(http.StatusOK, http.MethodPost, path+"/listCredentials"+apiVersion, nil, &creds)
	if creds.Username != "ElprovoAcme" || len(creds.Passwords) != 2 || creds.Passwords[0].Value != "registry-secret" {
		t.Fatalf("credentials = %+v", creds)
	}
}

func TestRegistryWithoutAdminUserHasNoCredentials(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	path := resourcePath("rg", typeRegistry, "elprovoacme")
	e.mustCall(http.StatusCreated, http.MethodPut, path+apiVersion, map[string]any{"location": "westeurope", "sku": map[string]string{"name": "Basic"}}, nil)

	e.mustFail(http.StatusBadRequest, "UnAuthorizedForCredentialOperations", http.MethodPost, path+"/listCredentials"+apiVersion, nil)
}

func TestTokenCredentialsCarryTheirExpiry(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	path := resourcePath("rg", typeRegistry, "elprovoacme")
	e.mustCall(http.StatusCreated, http.MethodPut, path+apiVersion, map[string]any{"location": "westeurope", "sku": map[string]string{"name": "Basic"}}, nil)
	e.mustCall(http.StatusOK, http.MethodGet, path+"/scopeMaps/_repositories_pull"+apiVersion, nil, nil)
	tokenID := resourcePath("rg", typeRegistry, "elprovoacme") + "/tokens/elprovo-pull"

	e.wait(e.mustCall(http.StatusCreated, http.MethodPut, tokenID+apiVersion, map[string]any{
		"properties": map[string]any{"scopeMapId": resourcePath("rg", typeRegistry, "elprovoacme") + "/scopeMaps/_repositories_pull", "status": "enabled"},
	}, nil))
	var creds struct {
		Username  string `json:"username"`
		Passwords []struct {
			Name, Value, Expiry string
		} `json:"passwords"`
	}
	e.mustCall(http.StatusOK, http.MethodPost, path+"/generateCredentials"+apiVersion, map[string]any{
		"tokenId": tokenID, "name": "password1", "expiry": "2099-01-01T00:00:00Z",
	}, &creds)

	if creds.Username != "registry-user" || len(creds.Passwords) != 1 || creds.Passwords[0].Value != "registry-secret" || creds.Passwords[0].Name != "password1" {
		t.Fatalf("credentials = %+v", creds)
	}
	var token struct {
		Properties struct {
			Credentials struct {
				Passwords []struct{ Name, Expiry string }
			}
		}
	}
	e.mustCall(http.StatusOK, http.MethodGet, tokenID+apiVersion, nil, &token)
	if pw := token.Properties.Credentials.Passwords; len(pw) != 1 || pw[0].Expiry != "2099-01-01T00:00:00.0000000Z" {
		t.Fatalf("token passwords = %+v", pw)
	}
}

func TestTokenNeedsABuiltInScopeMap(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPut, groupPath("rg")+apiVersion, map[string]any{"location": "westeurope"}, nil)
	path := resourcePath("rg", typeRegistry, "elprovoacme")
	e.mustCall(http.StatusCreated, http.MethodPut, path+apiVersion, map[string]any{"location": "westeurope", "sku": map[string]string{"name": "Basic"}}, nil)

	e.mustFail(http.StatusBadRequest, "UnsupportedBySimulation", http.MethodPut, path+"/tokens/elprovo-pull"+apiVersion, map[string]any{
		"properties": map[string]any{"scopeMapId": resourcePath("rg", typeRegistry, "elprovoacme") + "/scopeMaps/custom"},
	})
}
