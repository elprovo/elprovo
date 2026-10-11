package github

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

type tokenKind string

const (
	kindInstallation tokenKind = "installation"
	kindUser         tokenKind = "user"
	kindRefresh      tokenKind = "refresh"
	kindPersonal     tokenKind = "personal"
)

var tokenPrefixes = map[tokenKind]string{
	kindInstallation: "ghs_",
	kindUser:         "ghu_",
	kindRefresh:      "ghr_",
	kindPersonal:     "ghp_",
}

const (
	installationTokenTTL = time.Hour
	userTokenTTL         = 8 * time.Hour
	refreshTokenTTL      = 15897600 * time.Second
	authCodeTTL          = 10 * time.Minute
)

type token struct {
	Value          string            `json:"value"`
	Kind           tokenKind         `json:"kind"`
	AppID          int64             `json:"app_id,omitempty"`
	InstallationID int64             `json:"installation_id,omitempty"`
	UserID         int64             `json:"user_id,omitempty"`
	RepoIDs        []int64           `json:"repo_ids,omitempty"`
	Permissions    map[string]string `json:"permissions,omitempty"`
	Pair           string            `json:"pair,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
}

func newSecret(prefix string) string {
	return prefix + (rand.Text() + rand.Text())[:36]
}

func randomHex(bytes int) string {
	b := make([]byte, bytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (w *world) issueToken(t token, ttl time.Duration) *token {
	w.pruneTokens()
	t.Value = newSecret(tokenPrefixes[t.Kind])
	if ttl > 0 {
		exp := now().Add(ttl)
		t.ExpiresAt = &exp
	}
	w.Tokens = append(w.Tokens, &t)
	return &t
}

func (w *world) liveToken(value string) *token {
	for _, t := range w.Tokens {
		if t.Value == value && (t.ExpiresAt == nil || now().Before(*t.ExpiresAt)) {
			return t
		}
	}
	return nil
}

func (w *world) revokeTokens(match func(*token) bool) {
	w.Tokens = slices.DeleteFunc(w.Tokens, match)
}

func (w *world) pruneTokens() {
	t := now()
	w.revokeTokens(func(tok *token) bool { return tok.ExpiresAt != nil && !t.Before(*tok.ExpiresAt) })
	w.Codes = slices.DeleteFunc(w.Codes, func(c *authCode) bool { return !t.Before(c.ExpiresAt) })
}

func looksLikeToken(s string) bool {
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
