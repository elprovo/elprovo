package gcp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	tokenLife          = time.Hour
	tokenMargin        = time.Minute
)

// serviceAccountKey is the part of a service account key file that signs
// token requests.
type serviceAccountKey struct {
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
	Type         string `json:"type"`

	signer *rsa.PrivateKey
}

// parseKey reads a service account key file, given as JSON or as
// base64-encoded JSON.
func parseKey(data []byte) (*serviceAccountKey, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errors.New("google cloud provider needs a service account key")
	}
	if !strings.HasPrefix(text, "{") {
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
		if err != nil {
			return nil, errors.New("the service account key is neither JSON nor base64-encoded JSON")
		}
		text = string(decoded)
	}
	var k serviceAccountKey
	if err := json.Unmarshal([]byte(text), &k); err != nil {
		return nil, fmt.Errorf("parse the service account key: %w", err)
	}
	if k.Type != "service_account" || k.ClientEmail == "" || k.TokenURI == "" {
		return nil, errors.New("the credentials are not a service account key file with client_email and token_uri")
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, errors.New("the service account key has no PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the service account private key: %w", err)
	}
	signer, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the service account private key is not an RSA key")
	}
	k.signer = signer
	return &k, nil
}

// tokenSource gets OAuth 2.0 access tokens for a service account key with
// the JWT bearer grant (RFC 7523) at the key's token URI, and caches each
// one until shortly before it expires.
type tokenSource struct {
	key  *serviceAccountKey
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expiry.Add(-tokenMargin)) {
		return t.token, nil
	}
	assertion, err := t.assertion()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.key.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get an access token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return "", fmt.Errorf("get an access token: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("get an access token: %s: %s %s", resp.Status, body.Error, body.Description)
	}
	t.token, t.expiry = body.AccessToken, t.now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return t.token, nil
}

// forget drops the cached token, so the next call gets a new one.
func (t *tokenSource) forget() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

func (t *tokenSource) assertion() (string, error) {
	now := t.now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": t.key.PrivateKeyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": t.key.ClientEmail, "scope": cloudPlatformScope, "aud": t.key.TokenURI,
		"iat": now.Unix(), "exp": now.Add(tokenLife).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, t.key.signer, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign the token request: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
