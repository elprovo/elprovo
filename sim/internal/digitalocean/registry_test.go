package digitalocean

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryLifecycleAndDockerCredentials(t *testing.T) {
	e := newEnv(t)
	var created struct {
		Registry registryJSON `json:"registry"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/registry", map[string]string{"name": "elprovo", "subscription_tier_slug": "basic", "region": "fra1"}, &created)
	if created.Registry.Name != "elprovo" || created.Registry.Region != "fra1" || created.Registry.Subscription.Tier.Slug != "basic" {
		t.Fatalf("registry = %+v", created.Registry)
	}
	if status := e.call(http.MethodGet, "/v2/registries/elprovo", nil, nil); status != http.StatusOK {
		t.Fatalf("GET /v2/registries/elprovo = %d", status)
	}
	var sub struct {
		Subscription subscriptionJSON `json:"subscription"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/registries/subscription", nil, &sub)
	if sub.Subscription.Tier.Slug != "basic" || sub.Subscription.Tier.MonthlyPriceInCents != 500 {
		t.Fatalf("subscription = %+v", sub.Subscription)
	}

	var creds struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/registries/elprovo/docker-credentials?expiry_seconds=3600&read_write=true", nil, &creds)
	auth, err := base64.StdEncoding.DecodeString(creds.Auths["registry.test:5000"].Auth)
	if err != nil || string(auth) != "user:secret" {
		t.Fatalf("credentials = %+v (%q)", creds, auth)
	}
	if status := e.call(http.MethodGet, "/v2/registry/docker-credentials?expiry_seconds=-1", nil, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("negative expiry = %d, want 422", status)
	}
	if status := e.call(http.MethodPost, "/v2/registry", map[string]string{"name": "second", "subscription_tier_slug": "basic"}, nil); status != http.StatusConflict {
		t.Fatalf("second registry = %d, want 409", status)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/registries/elprovo", nil, nil)
	if status := e.call(http.MethodGet, "/v2/registry", nil, nil); status != http.StatusNotFound {
		t.Fatalf("registry after delete = %d, want 404", status)
	}
}

func TestRegistryRejectsInvalidSettings(t *testing.T) {
	e := newEnv(t)
	for _, req := range []map[string]string{
		{"name": "Upper", "subscription_tier_slug": "basic"},
		{"name": "ok", "subscription_tier_slug": "gold"},
		{"name": "ok", "subscription_tier_slug": "basic", "region": "nyc1"},
	} {
		if status := e.call(http.MethodPost, "/v2/registry", req, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("create %v = %d, want 422", req, status)
		}
	}
}

func TestRepositoriesComeFromTheRegistry(t *testing.T) {
	registryAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "user" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/_catalog":
			_, _ = w.Write([]byte(`{"repositories": ["elprovo/web", "elprovo/worker", "other/app"]}`))
		case "/v2/elprovo/web/tags/list":
			_, _ = w.Write([]byte(`{"name": "elprovo/web", "tags": ["v1", "latest"]}`))
		case "/v2/elprovo/worker/tags/list":
			_, _ = w.Write([]byte(`{"name": "elprovo/worker", "tags": ["v1"]}`))
		case "/v2/elprovo/web/manifests/v1", "/v2/elprovo/web/manifests/latest", "/v2/elprovo/worker/manifests/v1":
			w.Header().Set("Docker-Content-Digest", "sha256:one")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(registryAPI.Close)
	e := newEnv(t, func(c *config) { c.registry.apiURL = registryAPI.URL })
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/registry", map[string]string{"name": "elprovo", "subscription_tier_slug": "basic"}, nil)

	var got struct {
		Repositories []repositoryJSON `json:"repositories"`
		Meta         meta             `json:"meta"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/registry/elprovo/repositoriesV2", nil, &got)

	want := []repositoryJSON{{RegistryName: "elprovo", Name: "web", TagCount: 2, ManifestCount: 1}, {RegistryName: "elprovo", Name: "worker", TagCount: 1, ManifestCount: 1}}
	if got.Meta.Total != 2 || len(got.Repositories) != 2 || got.Repositories[0] != want[0] || got.Repositories[1] != want[1] {
		t.Fatalf("repositories = %+v", got.Repositories)
	}
}
