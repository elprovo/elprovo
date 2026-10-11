package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// API calls the control plane's JSON API as the signed-in operator.
type API interface {
	Do(ctx context.Context, method, path string, body, out any) error
}

// Environment is the control plane's record of the cloud being set up.
type Environment struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	StageletDomain string `json:"stagelet_domain"`
	StageletPort   int    `json:"stagelet_port"`
}

// Client is an enrolled agent or gateway.
type Client struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	EnrolledAt time.Time `json:"enrolled_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Connected  bool      `json:"connected"`
}

// account returns the one account the operator's token works in.
func account(ctx context.Context, api API) (string, error) {
	var me struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := api.Do(ctx, http.MethodGet, "/me", nil, &me); err != nil {
		return "", err
	}
	if len(me.Accounts) != 1 {
		return "", errors.New("the sign-in is not bound to one account; run `stagely login` again")
	}
	return me.Accounts[0].ID, nil
}

// ensureEnvironment returns the account's environment with the name,
// creating it when it does not exist yet.
func ensureEnvironment(ctx context.Context, api API, accountID string, want Environment) (Environment, error) {
	base := "/accounts/" + url.PathEscape(accountID) + "/environments"
	var list struct {
		Environments []Environment `json:"environments"`
	}
	if err := api.Do(ctx, http.MethodGet, base, nil, &list); err != nil {
		return Environment{}, err
	}
	for _, env := range list.Environments {
		if !strings.EqualFold(env.Name, want.Name) {
			continue
		}
		if env.Provider != want.Provider {
			return Environment{}, fmt.Errorf("environment %q already exists with provider %s, not %s", env.Name, env.Provider, want.Provider)
		}
		if env.StageletDomain != want.StageletDomain || env.StageletPort != want.StageletPort {
			return Environment{}, fmt.Errorf("environment %q already serves stagelets at %s port %d, not %s port %d; run setup with those values, or delete the environment first",
				env.Name, env.StageletDomain, env.StageletPort, want.StageletDomain, want.StageletPort)
		}
		return env, nil
	}
	var created Environment
	body := map[string]any{"name": want.Name, "provider": want.Provider, "stagelet_domain": want.StageletDomain, "stagelet_port": want.StageletPort}
	if err := api.Do(ctx, http.MethodPost, base, body, &created); err != nil {
		return Environment{}, fmt.Errorf("create environment %q: %w", want.Name, err)
	}
	return created, nil
}

func environmentPath(accountID, envID string) string {
	return "/accounts/" + url.PathEscape(accountID) + "/environments/" + url.PathEscape(envID)
}

func enrollmentToken(ctx context.Context, api API, accountID, envID, role string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := api.Do(ctx, http.MethodPost, environmentPath(accountID, envID)+"/enrollment-tokens", map[string]string{"role": role}, &out); err != nil {
		return "", fmt.Errorf("create %s enrollment token: %w", role, err)
	}
	return out.Token, nil
}

// enrollmentFor returns a one-time enrollment token for a daemon, or "" when
// its key is already enrolled. A key alone does not prove that: the
// environment may have been deleted and created again since, so the
// environment must also list a client with the role.
func enrollmentFor(ctx context.Context, api API, accountID, envID, role string, hasKey bool) (string, error) {
	if hasKey {
		list, err := clients(ctx, api, accountID, envID)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(list, func(c Client) bool { return c.Role == role }) {
			return "", nil
		}
	}
	return enrollmentToken(ctx, api, accountID, envID, role)
}

func clients(ctx context.Context, api API, accountID, envID string) ([]Client, error) {
	var out struct {
		Clients []Client `json:"clients"`
	}
	if err := api.Do(ctx, http.MethodGet, environmentPath(accountID, envID), nil, &out); err != nil {
		return nil, err
	}
	return out.Clients, nil
}

// waitConnected waits until a client with the role connects after before was
// taken, and returns its id. That is the daemon setup just started: a client
// that before lacks, which a new enrollment makes, or else one the control
// plane has seen since. A daemon that stopped still counts as connected for a
// while, so being connected alone proves nothing.
func waitConnected(ctx context.Context, api API, accountID, envID, role string, before []Client, poll time.Duration) (string, error) {
	lastSeen := map[string]time.Time{}
	for _, c := range before {
		lastSeen[c.ID] = c.LastSeenAt
	}
	for {
		list, err := clients(ctx, api, accountID, envID)
		if err != nil {
			return "", err
		}
		var reconnected string
		for _, c := range list {
			if c.Role != role || !c.Connected {
				continue
			}
			seen, known := lastSeen[c.ID]
			if !known {
				return c.ID, nil
			}
			if reconnected == "" && c.LastSeenAt.After(seen) {
				reconnected = c.ID
			}
		}
		if reconnected != "" {
			return reconnected, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("no %s connected to the control plane: %w", role, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// keepOne removes every client with the role except keep. An environment has
// one agent and one gateway.
func keepOne(ctx context.Context, api API, accountID, envID, role, keep string) error {
	list, err := clients(ctx, api, accountID, envID)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.Role != role || c.ID == keep {
			continue
		}
		if err := api.Do(ctx, http.MethodDelete, environmentPath(accountID, envID)+"/clients/"+url.PathEscape(c.ID), nil, nil); err != nil {
			return fmt.Errorf("remove old %s %s: %w", role, c.ID, err)
		}
	}
	return nil
}
