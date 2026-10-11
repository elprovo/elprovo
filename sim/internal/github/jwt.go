package github

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"time"
)

const maxJWTLifetime = 10 * time.Minute

type jwtError string

func (e jwtError) Error() string { return string(e) }

const (
	errJWTDecode   jwtError = "A JSON web token could not be decoded"
	errJWTExpired  jwtError = "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires"
	errJWTTooLong  jwtError = "'Expiration time' claim ('exp') is too far in the future"
	errJWTIssuedAt jwtError = "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued"
	errJWTIssuer   jwtError = "Integration not found"
)

type jwtClaims struct {
	Issuer    json.RawMessage `json:"iss"`
	IssuedAt  *int64          `json:"iat"`
	ExpiresAt *int64          `json:"exp"`
}

func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && strings.HasPrefix(s, "ey")
}

func (w *world) verifyAppJWT(raw string, at time.Time) (*app, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errJWTDecode
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims jwtClaims
	if decodeSegment(parts[0], &header) != nil || header.Alg != "RS256" || decodeSegment(parts[1], &claims) != nil {
		return nil, errJWTDecode
	}
	a := w.appByIssuer(claims.Issuer)
	if a == nil {
		return nil, errJWTIssuer
	}
	if !verifyRS256(a.PrivateKeyPEM, parts[0]+"."+parts[1], parts[2]) {
		return nil, errJWTDecode
	}
	return a, checkJWTTimes(claims, at)
}

func checkJWTTimes(c jwtClaims, at time.Time) error {
	if c.IssuedAt == nil || time.Unix(*c.IssuedAt, 0).After(at) {
		return errJWTIssuedAt
	}
	if c.ExpiresAt == nil || !time.Unix(*c.ExpiresAt, 0).After(at) {
		return errJWTExpired
	}
	if time.Unix(*c.ExpiresAt, 0).After(at.Add(maxJWTLifetime)) {
		return errJWTTooLong
	}
	return nil
}

func (w *world) appByIssuer(raw json.RawMessage) *app {
	var iss string
	if json.Unmarshal(raw, &iss) != nil {
		iss = string(raw)
	}
	if a := w.appByClientID(iss); a != nil {
		return a
	}
	id, err := strconv.ParseInt(iss, 10, 64)
	if err != nil {
		return nil
	}
	return w.app(id)
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func verifyRS256(privatePEM, signed, sig string) bool {
	key, err := parsePrivateKey(privatePEM)
	if err != nil {
		return false
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(signed))
	return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], rawSig) == nil
}

func parsePrivateKey(privatePEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
