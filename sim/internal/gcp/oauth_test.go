package gcp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *testEnv) keyFile() keyFile {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.dataDir, keyFileName))
	if err != nil {
		e.t.Fatalf("read the key file: %v", err)
	}
	var k keyFile
	if err := json.Unmarshal(data, &k); err != nil {
		e.t.Fatalf("parse the key file: %v", err)
	}
	return k
}

// assertion signs a JWT bearer assertion with the key file's key, as Google's
// client libraries do.
func assertion(t *testing.T, k keyFile, claims map[string]any) string {
	t.Helper()
	block, _ := pem.Decode([]byte(k.PrivateKey))
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.PrivateKeyID})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, parsed.(*rsa.PrivateKey), crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func standardClaims(k keyFile, now time.Time) map[string]any {
	return map[string]any{
		"iss": k.ClientEmail, "scope": "https://www.googleapis.com/auth/cloud-platform", "aud": k.TokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

func (e *testEnv) exchange(jwt string) (int, map[string]any) {
	e.t.Helper()
	form := url.Values{"grant_type": {jwtBearerGrant}, "assertion": {jwt}}
	status, data := e.do(http.MethodPost, "/token", "", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		e.t.Fatalf("token response %q: %v", data, err)
	}
	return status, body
}

func TestKeyFileNamesTheServiceAccountAndTheTokenURI(t *testing.T) {
	e := newEnv(t)

	k := e.keyFile()

	if k.Type != "service_account" || k.ProjectID != testProject || k.TokenURI != "http://sim.test/token" ||
		k.ClientEmail != "elprovo-sim@"+testProject+".iam.gserviceaccount.com" || len(k.PrivateKeyID) != 40 ||
		!strings.HasPrefix(k.PrivateKey, "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("key file = %+v", k)
	}
	info, err := os.Stat(filepath.Join(e.dataDir, keyFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode(), err)
	}
}

func TestSignedAssertionGetsAnAccessTokenThatTheAPIAccepts(t *testing.T) {
	e := newEnv(t)
	k := e.keyFile()

	status, body := e.exchange(assertion(t, k, standardClaims(k, time.Now())))

	token, _ := body["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, "ya29.") || body["token_type"] != "Bearer" || body["expires_in"] != float64(3599) {
		t.Fatalf("token response = %d %v", status, body)
	}
	if got, _ := e.do(http.MethodGet, computePath, token, nil, ""); got != http.StatusOK {
		t.Fatalf("GET project with the issued token = %d, want 200", got)
	}
}

func TestAssertionsThatGoogleRejectsGetNoToken(t *testing.T) {
	e := newEnv(t)
	k := e.keyFile()
	now := time.Now()
	other := k
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	other.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	tests := []struct {
		name  string
		jwt   string
		error string
	}{
		{"signed by another key", assertion(t, other, standardClaims(k, now)), "invalid_grant"},
		{"expired", assertion(t, k, merge(standardClaims(k, now), map[string]any{"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()})), "invalid_grant"},
		{"longer than an hour", assertion(t, k, merge(standardClaims(k, now), map[string]any{"exp": now.Add(2 * time.Hour).Unix()})), "invalid_grant"},
		{"without a scope", assertion(t, k, merge(standardClaims(k, now), map[string]any{"scope": ""})), "invalid_scope"},
		{"not a JWT", "not-a-jwt", "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := e.exchange(tt.jwt)
			if status != http.StatusBadRequest || body["error"] != tt.error || body["access_token"] != nil {
				t.Fatalf("token response = %d %v, want 400 %s", status, body, tt.error)
			}
		})
	}
}

func TestAPIRejectsRequestsWithoutAValidToken(t *testing.T) {
	e := newEnv(t)

	status, data := e.do(http.MethodGet, computePath, "ya29.unknown", nil, "")

	var body googleError
	_ = json.Unmarshal(data, &body)
	if status != http.StatusUnauthorized || body.Error.Code != 401 || body.Error.Status != "UNAUTHENTICATED" {
		t.Fatalf("GET project with an unknown token = %d %s", status, data)
	}
}

func TestIssuedTokensSurviveARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gcp")
	first := newTestEnv(t, dir, newFakeMachines())
	k := first.keyFile()
	_, body := first.exchange(assertion(t, k, standardClaims(k, time.Now())))
	token := body["access_token"].(string)

	second := newTestEnv(t, dir, newFakeMachines())

	if got, _ := second.do(http.MethodGet, computePath, token, nil, ""); got != http.StatusOK {
		t.Fatalf("GET project after a restart = %d, want 200", got)
	}
	if second.keyFile().PrivateKeyID != k.PrivateKeyID {
		t.Fatal("the service account key changed across a restart")
	}
}
