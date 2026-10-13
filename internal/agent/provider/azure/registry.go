package azure

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	registryAPIVersion = "2023-07-01"
	// registryCredentialsExpiry is how long registry credentials on a
	// machine stay valid: long enough for a long build before its push.
	registryCredentialsExpiry = 2 * time.Hour
	// jobTokenPrefix names the registry tokens the provider makes, one for
	// each set of credentials, and removes after they expire.
	jobTokenPrefix = "elprovo-job-"
)

// RegistryCredentials returns credentials for the environment's container
// registry at host: a password, valid for registryCredentialsExpiry, of a
// new repository-scoped token that can push only when push is set. A token
// has two passwords, so each set of credentials gets its own token, and
// jobs running at once never replace each other's password. Expired tokens
// are removed first. The credentials reach only the registry.
func (p *Provider) RegistryCredentials(ctx context.Context, host string, push bool) (string, string, error) {
	server, err := p.registryLoginServer(ctx)
	if err != nil {
		return "", "", err
	}
	if !strings.EqualFold(host, server) {
		return "", "", fmt.Errorf("registry %s is not the environment's registry %s", host, server)
	}
	if err := p.removeExpiredTokens(ctx); err != nil {
		return "", "", err
	}
	name, err := jobTokenName()
	if err != nil {
		return "", "", err
	}
	scope := "_repositories_pull"
	if push {
		scope = "_repositories_push"
	}
	tokenID := p.cfg.Registry + "/tokens/" + name
	body := map[string]any{"properties": map[string]string{"scopeMapId": p.cfg.Registry + "/scopeMaps/" + scope, "status": "enabled"}}
	resp, err := p.api.do(ctx, http.MethodPut, tokenID, registryAPIVersion, body, nil)
	if err == nil && resp.operation != "" {
		err = p.api.wait(ctx, resp.operation, p.pollEvery)
	}
	if err != nil {
		return "", "", fmt.Errorf("create registry token: %w", err)
	}
	return p.generateTokenPassword(ctx, tokenID)
}

func jobTokenName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("name a registry token: %w", err)
	}
	return jobTokenPrefix + hex.EncodeToString(b), nil
}

func (p *Provider) registryLoginServer(ctx context.Context) (string, error) {
	if p.cfg.Registry == "" {
		return "", errors.New("the Azure provider has no container registry")
	}
	var reg struct {
		Properties struct {
			LoginServer string `json:"loginServer"`
		} `json:"properties"`
	}
	if _, err := p.api.do(ctx, http.MethodGet, p.cfg.Registry, registryAPIVersion, nil, &reg); err != nil {
		return "", fmt.Errorf("read container registry: %w", err)
	}
	return reg.Properties.LoginServer, nil
}

func (p *Provider) generateTokenPassword(ctx context.Context, tokenID string) (string, string, error) {
	body := map[string]string{
		"tokenId": tokenID, "name": "password1", "expiry": p.api.now().Add(registryCredentialsExpiry).UTC().Format(time.RFC3339),
	}
	var creds struct {
		Username  string `json:"username"`
		Passwords []struct {
			Value string `json:"value"`
		} `json:"passwords"`
	}
	resp, err := p.api.do(ctx, http.MethodPost, p.cfg.Registry+"/generateCredentials", registryAPIVersion, body, &creds)
	for err == nil && resp.status == http.StatusAccepted && resp.location != "" {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(p.pollEvery):
		}
		var u *url.URL
		if u, err = url.Parse(resp.location); err == nil {
			resp, err = p.api.do(ctx, http.MethodGet, u.Path, u.Query().Get("api-version"), nil, &creds)
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("generate registry token password: %w", err)
	}
	if creds.Username == "" || len(creds.Passwords) == 0 || creds.Passwords[0].Value == "" {
		return "", "", errors.New("generate registry token password: Azure returned no credentials")
	}
	return creds.Username, creds.Passwords[0].Value, nil
}

// removeExpiredTokens deletes the provider's tokens whose passwords have
// expired.
func (p *Provider) removeExpiredTokens(ctx context.Context) error {
	cutoff := p.api.now().Add(-registryCredentialsExpiry)
	var expired []string
	err := p.api.list(ctx, p.cfg.Registry+"/tokens", registryAPIVersion, func(item json.RawMessage) error {
		var t struct {
			Name       string `json:"name"`
			Properties struct {
				CreationDate time.Time `json:"creationDate"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return fmt.Errorf("decode registry token: %w", err)
		}
		if strings.HasPrefix(t.Name, jobTokenPrefix) && t.Properties.CreationDate.Before(cutoff) {
			expired = append(expired, t.Name)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("list registry tokens: %w", err)
	}
	for _, name := range expired {
		if _, err := p.api.do(ctx, http.MethodDelete, p.cfg.Registry+"/tokens/"+url.PathEscape(name), registryAPIVersion, nil, nil); err != nil && !errors.Is(err, errNotFound) {
			return fmt.Errorf("delete expired registry token %s: %w", name, err)
		}
	}
	return nil
}
