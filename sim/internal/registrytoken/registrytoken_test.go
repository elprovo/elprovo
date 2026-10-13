package registrytoken

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

const registryHost = "registry.example.com"

// newService returns a token service that knows one client, puller:secret,
// which may pull from acme/web and nothing else.
func newService(t *testing.T, now time.Time) *Service {
	t.Helper()
	signer, err := NewSigner(now)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{
		Realm: "Example", Signer: signer, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
		Authenticate: func(user, password string) (Identity, bool) {
			allowed := func(service, repository string) []string {
				if service == registryHost && repository == "acme/web" {
					return []string{"pull"}
				}
				return nil
			}
			return Identity{Subject: "puller", Allowed: allowed}, user == "puller" && password == "secret"
		},
	}
}

func requestToken(t *testing.T, s *Service, user, password string, scopes ...string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"service": {registryHost}, "scope": scopes}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/token?"+q.Encode(), nil)
	r.SetBasicAuth(user, password)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestTokenGrantsOnlyTheRequestedActionsTheClientIsAllowed(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := newService(t, now)

	w := requestToken(t, s, "puller", "secret", "repository:acme/web:pull,push,pull", "repository:acme/api:pull")

	var body struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); w.Code != http.StatusOK || err != nil {
		t.Fatalf("token service = %d, %v", w.Code, err)
	}
	claims := verify(t, s.Signer, body.Token)
	if claims.Issuer != "elprovo-sim" || claims.Subject != "puller" || claims.Audience != registryHost || claims.Expiry != now.Add(time.Hour).Unix() || body.ExpiresIn != 3600 {
		t.Fatalf("claims = %+v, expires_in %d", claims, body.ExpiresIn)
	}
	want := []Access{{Type: "repository", Name: "acme/web", Actions: []string{"pull"}}}
	if !slices.EqualFunc(claims.Access, want, func(a, b Access) bool {
		return a.Type == b.Type && a.Name == b.Name && slices.Equal(a.Actions, b.Actions)
	}) {
		t.Fatalf("access = %+v, want pull on acme/web only", claims.Access)
	}
}

func TestTokenServiceRefusesUnknownCredentials(t *testing.T) {
	s := newService(t, time.Now())

	w := requestToken(t, s, "puller", "wrong", "repository:acme/web:pull")

	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Basic realm="Example"` {
		t.Fatalf("token service = %d %q, want 401 with a basic auth challenge", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

type claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience string   `json:"aud"`
	Expiry   int64    `json:"exp"`
	Access   []Access `json:"access"`
}

// verify checks that the token is signed by the key of the certificate the
// registry trusts, which its x5c header carries, and returns its claims.
func verify(t *testing.T, signer *Signer, token string) claims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q is not a JWT", token)
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(raw, &header); err != nil || header.Alg != "RS256" || len(header.X5C) != 1 {
		t.Fatalf("token header %s: %v", raw, err)
	}
	block, _ := pem.Decode([]byte(signer.Certificate))
	if der, _ := base64.StdEncoding.DecodeString(header.X5C[0]); block == nil || !slices.Equal(block.Bytes, der) {
		t.Fatal("the token's x5c certificate is not the certificate the registry trusts")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("token signature: %v", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("token claims %s: %v", payload, err)
	}
	return c
}
