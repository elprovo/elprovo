package gcp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	keyFileName      = "service-account.json"
	jwtBearerGrant   = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	accessTokenLife  = time.Hour
	maxAssertionLife = time.Hour
	clockSkew        = 5 * time.Minute
)

// serviceAccountKey is a key of the project's service account. The
// simulation keeps the private half too, to write the key file that Google
// Cloud hands out once.
type serviceAccountKey struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	ClientID   string    `json:"client_id"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

func newServiceAccountKey(project string, projectNumber int64, now time.Time) (*serviceAccountKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 20)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return &serviceAccountKey{
		ID:         hex.EncodeToString(id),
		Email:      "elprovo-sim@" + project + ".iam.gserviceaccount.com",
		ClientID:   fmt.Sprintf("1%020d", projectNumber),
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		CreatedAt:  now.UTC(),
	}, nil
}

func (k *serviceAccountKey) publicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("service account key %s holds no PEM block", k.ID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	private, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("service account key %s is not an RSA key", k.ID)
	}
	return &private.PublicKey, nil
}

// keyFile is the JSON key file of a service account, as Google Cloud lets
// you download it.
type keyFile struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
	UniverseDomain          string `json:"universe_domain"`
}

func (s *Simulation) keyFile(k *serviceAccountKey) keyFile {
	return keyFile{
		Type: "service_account", ProjectID: s.cfg.project, PrivateKeyID: k.ID, PrivateKey: k.PrivateKey,
		ClientEmail: k.Email, ClientID: k.ClientID, AuthURI: "https://accounts.google.com/o/oauth2/auth",
		TokenURI: s.cfg.tokenURI, AuthProviderX509CertURL: "https://www.googleapis.com/oauth2/v1/certs",
		ClientX509CertURL: "https://www.googleapis.com/robot/v1/metadata/x509/" + url.PathEscape(k.Email),
		UniverseDomain:    "googleapis.com",
	}
}

// writeFiles writes the service account key file and the registry token
// signing certificate into the data directory, where the operator picks
// them up.
func (s *Simulation) writeFiles() error {
	if s.dataDir == "" {
		return nil
	}
	var key keyFile
	var cert string
	s.store.Read(func(w *world) { key, cert = s.keyFile(w.Keys[0]), w.Registry.Certificate })
	data, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.dataDir, err)
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, keyFileName), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the service account key file: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, registryCertFileName), []byte(cert), 0o644); err != nil {
		return fmt.Errorf("write the registry token certificate: %w", err)
	}
	return nil
}

type oauthError struct {
	code        string
	description string
}

func (e *oauthError) Error() string { return e.code + ": " + e.description }

// issueAccessToken serves the OAuth 2.0 token endpoint for the JWT bearer
// grant (RFC 7523) that service account keys use: the assertion is a JWT
// that the key signed with RS256.
func (s *Simulation) issueAccessToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "Could not parse the request body."})
		return
	}
	if grant := r.PostForm.Get("grant_type"); grant != jwtBearerGrant {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type", "error_description": "Invalid grant_type: " + grant})
		return
	}
	email, err := s.verifyAssertion(r.PostForm.Get("assertion"))
	if err != nil {
		var oe *oauthError
		if !errors.As(err, &oe) {
			oe = &oauthError{"invalid_grant", err.Error()}
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": oe.code, "error_description": oe.description})
		return
	}
	token, err := s.issueToken(email, accessTokenLife)
	if err != nil {
		s.log.Error("issue an access token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_failure", "error_description": "Could not issue a token."})
		return
	}
	s.log.Info("issued an access token", "service_account", email)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "expires_in": int(accessTokenLife.Seconds()) - 1, "token_type": "Bearer",
	})
}

type jwtClaims struct {
	Iss            string `json:"iss"`
	Sub            string `json:"sub"`
	Scope          string `json:"scope"`
	Aud            string `json:"aud"`
	TargetAudience string `json:"target_audience"`
	Exp            int64  `json:"exp"`
	Iat            int64  `json:"iat"`
}

// verifyAssertion checks the JWT that a service account key signed and
// returns the service account's email.
func (s *Simulation) verifyAssertion(assertion string) (string, error) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return "", &oauthError{"invalid_request", "Invalid JWT: assertion must have three parts."}
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	var claims jwtClaims
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", &oauthError{"invalid_request", "Invalid JWT header."}
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", &oauthError{"invalid_request", "Invalid JWT payload."}
	}
	if header.Alg != "RS256" {
		return "", &oauthError{"invalid_grant", "Invalid JWT: alg must be RS256."}
	}
	var key *serviceAccountKey
	s.store.Read(func(w *world) {
		key = find(w.Keys, func(k *serviceAccountKey) bool {
			return k.Email == claims.Iss && (header.Kid == "" || k.ID == header.Kid)
		})
	})
	if key == nil {
		return "", &oauthError{"invalid_grant", "Invalid JWT Signature."}
	}
	public, err := key.publicKey()
	if err != nil {
		return "", err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", &oauthError{"invalid_grant", "Invalid JWT Signature."}
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) != nil {
		return "", &oauthError{"invalid_grant", "Invalid JWT Signature."}
	}
	return claims.Iss, s.checkClaims(claims)
}

func (s *Simulation) checkClaims(c jwtClaims) error {
	now := s.now()
	iat, exp := time.Unix(c.Iat, 0), time.Unix(c.Exp, 0)
	switch {
	case !strings.HasSuffix(c.Aud, "/token"):
		return &oauthError{"invalid_grant", "Invalid JWT: aud must be the token URI."}
	case c.Sub != "" && c.Sub != c.Iss:
		return &oauthError{"unauthorized_client", "Client is unauthorized to retrieve access tokens using this method; domain-wide delegation is not simulated."}
	case c.Scope == "" && c.TargetAudience == "":
		return &oauthError{"invalid_scope", "Invalid OAuth scope or ID token audience provided."}
	case c.TargetAudience != "":
		return &oauthError{"invalid_request", "ID tokens (target_audience) are not supported by the simulation."}
	case exp.Sub(iat) > maxAssertionLife || exp.Before(iat):
		return &oauthError{"invalid_grant", "Invalid JWT: Token must be a short-lived token (60 minutes) and in a reasonable timeframe."}
	case now.After(exp.Add(clockSkew)) || iat.After(now.Add(clockSkew)):
		return &oauthError{"invalid_grant", "Invalid JWT: Token must be a short-lived token (60 minutes) and in a reasonable timeframe. Check your iat and exp values in the JWT claim."}
	}
	return nil
}

// issueToken records a new access token of the service account that lasts
// life, and drops the tokens that expired.
func (s *Simulation) issueToken(email string, life time.Duration) (string, error) {
	token, err := randomToken("ya29.c.")
	if err != nil {
		return "", err
	}
	now := s.now()
	err = s.store.Update(func(w *world) error {
		for t, issued := range w.Tokens {
			if !now.Before(issued.Expiry) {
				delete(w.Tokens, t)
			}
		}
		w.Tokens[token] = issuedToken{Email: email, Expiry: now.Add(life)}
		return nil
	})
	return token, err
}

func decodeSegment(segment string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func randomToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
