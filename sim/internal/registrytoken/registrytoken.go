// Package registrytoken is the Docker token service of the simulated cloud
// registries. A CNCF Distribution registry with token authentication trusts
// the certificate of a Signer and sends Docker clients to a Service. The
// Service signs a client in with basic auth and gives it a bearer token that
// names the repositories and the actions, pull and push, it may use. This is
// the token protocol of the Docker registry: https://distribution.github.io/distribution/spec/auth/token/.
package registrytoken

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// Issuer is the iss claim of every token; the registry must expect it.
	Issuer   = "elprovo-sim"
	lifetime = time.Hour
)

// Signer is the key that signs tokens, and its self-signed certificate,
// which the registry trusts. It is kept in a simulation's state as JSON.
type Signer struct {
	PrivateKey  string `json:"private_key"`
	Certificate string `json:"certificate"`
}

// NewSigner returns a new RSA key with a certificate valid for ten years.
func NewSigner(now time.Time) (*Signer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "elprovo-sim registry token signer"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &Signer{
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}, nil
}

// sign returns a JWT that the registry accepts: RS256, with the signing
// certificate in its x5c header.
func (s *Signer) sign(claims any) (string, error) {
	keyBlock, _ := pem.Decode([]byte(s.PrivateKey))
	certBlock, _ := pem.Decode([]byte(s.Certificate))
	if keyBlock == nil || certBlock == nil {
		return "", errors.New("the registry token signer holds no PEM blocks")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("the registry token signer is not an RSA key")
	}
	header, err := json.Marshal(map[string]any{"typ": "JWT", "alg": "RS256", "x5c": []string{base64.StdEncoding.EncodeToString(certBlock.Bytes)}})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Access is one grant in a token: the actions on one repository.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Identity is a client that signed in to the token service.
type Identity struct {
	// Subject names the client in the token's sub claim.
	Subject string
	// Allowed returns the actions, of pull and push, that the client may
	// take on the named repository of the registry at service.
	Allowed func(service, repository string) []string
}

// Service answers GET <realm>?service=<registry>&scope=repository:<name>:<actions>,
// with the client's credentials as basic auth.
type Service struct {
	// Realm is the basic auth realm that a refusal names.
	Realm string
	// Authenticate returns the identity that the credentials belong to, and
	// false for credentials the simulation does not know.
	Authenticate func(user, password string) (Identity, bool)
	Signer       *Signer
	Now          func() time.Time
	Log          *slog.Logger
}

// ServeHTTP issues a token that grants, for each requested scope, the
// requested actions that the identity is allowed. A scope with no allowed
// action is left out, so the registry refuses it.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	var identity Identity
	if ok {
		identity, ok = s.Authenticate(user, password)
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+s.Realm+`"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []map[string]string{{"code": "UNAUTHORIZED", "message": "Not Authorized."}}})
		return
	}
	q := r.URL.Query()
	service := q.Get("service")
	access := []Access{}
	for _, scope := range q["scope"] {
		if grant, ok := grant(identity, service, scope); ok {
			access = append(access, grant)
		}
	}
	now := s.Now()
	token, err := s.Signer.sign(map[string]any{
		"iss": Issuer, "sub": identity.Subject, "aud": service, "exp": now.Add(lifetime).Unix(),
		"nbf": now.Add(-time.Minute).Unix(), "iat": now.Unix(), "jti": rand.Text(), "access": access,
	})
	if err != nil {
		s.Log.Error("sign a registry token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"errors": []map[string]string{{"code": "UNKNOWN", "message": "internal error"}}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "access_token": token, "expires_in": int(lifetime.Seconds()), "issued_at": now.UTC().Format(time.RFC3339),
	})
}

// grant returns the access that a scope such as repository:acme/web:pull,push
// gets: the requested actions that the identity may take, each once.
func grant(identity Identity, service, scope string) (Access, bool) {
	parts := strings.Split(scope, ":")
	if len(parts) != 3 || parts[0] != "repository" || parts[1] == "" {
		return Access{}, false
	}
	allowed := identity.Allowed(service, parts[1])
	var actions []string
	for _, a := range strings.Split(parts[2], ",") {
		if slices.Contains(allowed, a) && !slices.Contains(actions, a) {
			actions = append(actions, a)
		}
	}
	return Access{Type: "repository", Name: parts[1], Actions: actions}, len(actions) > 0
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
