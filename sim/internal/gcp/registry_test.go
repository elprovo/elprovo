package gcp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/elprovo/elprovo/sim/internal/registrytoken"
)

const repositoriesPath = "/v1/projects/" + testProject + "/locations/us-central1/repositories"

func TestCreateRepositoryFinishesItsOperation(t *testing.T) {
	e := newEnv(t)
	var op map[string]any

	e.mustCall(http.StatusOK, http.MethodPost, repositoriesPath+"?repositoryId=images", map[string]any{"format": "DOCKER", "description": "previews"}, &op)

	if op["done"] != true || !strings.HasPrefix(op["name"].(string), "projects/"+testProject+"/locations/us-central1/operations/") {
		t.Fatalf("create operation = %v", op)
	}
	var again map[string]any
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/"+op["name"].(string), nil, &again)
	if again["done"] != true {
		t.Fatalf("operation read again = %v", again)
	}
	var repo repositoryJSON
	e.mustCall(http.StatusOK, http.MethodGet, repositoriesPath+"/images", nil, &repo)
	if repo.Name != "projects/"+testProject+"/locations/us-central1/repositories/images" || repo.Format != "DOCKER" ||
		repo.RegistryURI != "us-central1-docker.pkg.dev/"+testProject+"/images" || repo.Mode != "STANDARD_REPOSITORY" {
		t.Fatalf("repository = %+v", repo)
	}
	e.fail(http.StatusConflict, http.MethodPost, repositoriesPath+"?repositoryId=images", map[string]any{"format": "DOCKER"})
	e.mustCall(http.StatusOK, http.MethodDelete, repositoriesPath+"/images", nil, nil)
	e.fail(http.StatusNotFound, http.MethodGet, repositoriesPath+"/images", nil)
}

func TestRepositoryFormatsOtherThanDockerFailLoudly(t *testing.T) {
	e := newEnv(t)

	got := e.fail(http.StatusBadRequest, http.MethodPost, repositoriesPath+"?repositoryId=pkgs", map[string]any{"format": "NPM"})

	if !strings.Contains(got.Error.Message, "not supported by the simulation") {
		t.Fatalf("error = %+v", got)
	}
}

// registryToken asks the registry token service for a token, signing in as
// a Docker client does.
func (e *testEnv) registryToken(user, password, scope string) (int, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.server.URL+"/v2/token?service=us-central1-docker.pkg.dev&scope="+scope, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.SetBasicAuth(user, password)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestRegistryTokenGrantsPushAndPullOnTheProjectsRepositories(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusOK, http.MethodPost, repositoriesPath+"?repositoryId=images", map[string]any{"format": "DOCKER"}, nil)
	keyJSON, err := os.ReadFile(filepath.Join(e.dataDir, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	scope := "repository:" + testProject + "/images/app:pull,push"

	status, body := e.registryToken("_json_key_base64", base64.StdEncoding.EncodeToString(keyJSON), scope)

	if status != http.StatusOK {
		t.Fatalf("token service = %d %v", status, body)
	}
	header, claims := e.verifyRegistryToken(body["token"].(string))
	if header["alg"] != "RS256" || claims["iss"] != registrytoken.Issuer || claims["aud"] != "us-central1-docker.pkg.dev" {
		t.Fatalf("token header %v, claims %v", header, claims)
	}
	if access := accessOf(claims); len(access) != 1 || access[0].Type != "repository" || access[0].Name != testProject+"/images/app" ||
		!slices.Equal(access[0].Actions, []string{"pull", "push"}) {
		t.Fatalf("access = %+v", access)
	}
	_, body = e.registryToken("oauth2accesstoken", testToken, "repository:"+testProject+"/missing/app:pull")
	_, claims = e.verifyRegistryToken(body["token"].(string))
	if access := accessOf(claims); len(access) != 0 {
		t.Fatalf("access to a missing repository = %+v, want none", access)
	}
}

func TestRegistryTokenRejectsUnknownCredentials(t *testing.T) {
	e := newEnv(t)
	for _, creds := range [][2]string{{"oauth2accesstoken", "ya29.unknown"}, {"_json_key", `{"type":"service_account"}`}, {"admin", "admin"}} {
		if status, _ := e.registryToken(creds[0], creds[1], "repository:"+testProject+"/images/app:pull"); status != http.StatusUnauthorized {
			t.Errorf("token service for %s = %d, want 401", creds[0], status)
		}
	}
}

func accessOf(claims map[string]any) []registrytoken.Access {
	data, _ := json.Marshal(claims["access"])
	var access []registrytoken.Access
	_ = json.Unmarshal(data, &access)
	return access
}

// verifyRegistryToken checks the token's signature with the certificate in
// its x5c header, which must be the certificate the registry trusts.
func (e *testEnv) verifyRegistryToken(token string) (map[string]any, map[string]any) {
	e.t.Helper()
	parts := strings.Split(token, ".")
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(raw, &header); err != nil || len(header.X5C) != 1 {
		e.t.Fatalf("token header %s: %v", raw, err)
	}
	der, _ := base64.StdEncoding.DecodeString(header.X5C[0])
	trusted, err := os.ReadFile(filepath.Join(e.dataDir, registryCertFileName))
	if err != nil {
		e.t.Fatal(err)
	}
	block, _ := pem.Decode(trusted)
	if block == nil || !slices.Equal(block.Bytes, der) {
		e.t.Fatal("the token's x5c certificate is not the certificate the registry trusts")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		e.t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], sig); err != nil {
		e.t.Fatalf("token signature: %v", err)
	}
	var h, claims map[string]any
	_ = json.Unmarshal(raw, &h)
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(payload, &claims)
	return h, claims
}
