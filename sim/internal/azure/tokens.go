package azure

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var tokenNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-]{5,50}$`)

// builtinScopeMaps are the scope maps every registry has, each over all of
// its repositories.
var builtinScopeMaps = map[string][]string{
	"_repositories_admin":               {"repositories/*/metadata/read", "repositories/*/metadata/write", "repositories/*/content/read", "repositories/*/content/write", "repositories/*/content/delete"},
	"_repositories_pull":                {"repositories/*/content/read"},
	"_repositories_pull_metadata_read":  {"repositories/*/content/read", "repositories/*/metadata/read"},
	"_repositories_push":                {"repositories/*/content/read", "repositories/*/content/write"},
	"_repositories_push_metadata_write": {"repositories/*/metadata/read", "repositories/*/metadata/write", "repositories/*/content/read", "repositories/*/content/write"},
}

// registryToken is a repository-scoped access token of a registry, with the
// expiry of each password generated for it.
type registryToken struct {
	Name      string          `json:"name"`
	ScopeMap  string          `json:"scope_map"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	Passwords []tokenPassword `json:"passwords,omitempty"`
}

type tokenPassword struct {
	Name    string     `json:"name"`
	Created time.Time  `json:"created"`
	Expiry  *time.Time `json:"expiry,omitempty"`
}

func (s *Simulation) registryChildID(reg *registry, kind, name string) string {
	return s.resourceID(reg.Group, typeRegistry, reg.Name, kind, name)
}

func (s *Simulation) renderToken(reg *registry, t *registryToken) map[string]any {
	passwords := []map[string]string{}
	for _, p := range t.Passwords {
		item := map[string]string{"name": p.Name, "creationTime": timestamp(p.Created)}
		if p.Expiry != nil {
			item["expiry"] = timestamp(*p.Expiry)
		}
		passwords = append(passwords, item)
	}
	return map[string]any{
		"id": s.registryChildID(reg, "tokens", t.Name), "name": t.Name, "type": typeRegistry + "/tokens",
		"properties": map[string]any{
			"creationDate": timestamp(t.CreatedAt), "provisioningState": statusSucceeded, "status": t.Status,
			"scopeMapId":  s.registryChildID(reg, "scopeMaps", t.ScopeMap),
			"credentials": map[string]any{"certificates": []any{}, "passwords": passwords},
		},
	}
}

func (s *Simulation) getScopeMap(_ *http.Request, p params) (reply, error) {
	var out map[string]any
	var err error
	s.store.Read(func(sub *subscription) {
		reg := lookup(sub.Registries, p["group"], p["name"])
		actions, builtin := builtinScopeMaps[p["scopeMap"]]
		switch {
		case reg == nil:
			err = errNotFound(typeRegistry, p["name"], p["group"])
		case !builtin:
			err = errNotFound(typeRegistry+"/scopeMaps", p["name"]+"/"+p["scopeMap"], p["group"])
		default:
			out = map[string]any{
				"id": s.registryChildID(reg, "scopeMaps", p["scopeMap"]), "name": p["scopeMap"], "type": typeRegistry + "/scopeMaps",
				"properties": map[string]any{"type": "SystemDefined", "actions": actions, "provisioningState": statusSucceeded, "creationDate": timestamp(reg.CreatedAt)},
			}
		}
	})
	if err != nil {
		return reply{}, err
	}
	return okReply(out), nil
}

type tokenRequest struct {
	Properties struct {
		ScopeMapID  string `json:"scopeMapId"`
		Status      string `json:"status"`
		Credentials *struct {
			Certificates []any `json:"certificates"`
			Passwords    []any `json:"passwords"`
		} `json:"credentials"`
	} `json:"properties"`
}

// putToken creates or updates a token over one of the registry's built-in
// scope maps.
func (s *Simulation) putToken(r *http.Request, p params) (reply, error) {
	var req tokenRequest
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	props := req.Properties
	status, ok := canonical(props.Status, "enabled", "disabled")
	switch {
	case !tokenNamePattern.MatchString(p["token"]):
		return reply{}, errInvalidParameter("Token name '%s' is invalid. It must have 5 to 50 letters, digits or dashes.", p["token"])
	case props.Status == "":
		status = "enabled"
	case !ok:
		return reply{}, errInvalidParameter("Token status '%s' is invalid; use enabled or disabled.", props.Status)
	}
	if c := props.Credentials; c != nil && len(c.Certificates)+len(c.Passwords) > 0 {
		return reply{}, errUnsupported("Credentials in a token request; generate them with generateCredentials")
	}
	var out map[string]any
	var op *operation
	code := http.StatusOK
	err := s.store.Update(func(sub *subscription) error {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if reg == nil {
			return errNotFound(typeRegistry, p["name"], p["group"])
		}
		scopeMap := ""
		for name := range builtinScopeMaps {
			if sameID(props.ScopeMapID, s.registryChildID(reg, "scopeMaps", name)) {
				scopeMap = name
			}
		}
		if scopeMap == "" {
			return errUnsupported("The scope map %q; use one of the registry's built-in scope maps", props.ScopeMapID)
		}
		i := slices.IndexFunc(reg.Tokens, func(t *registryToken) bool { return strings.EqualFold(t.Name, p["token"]) })
		if i < 0 {
			reg.Tokens = append(reg.Tokens, &registryToken{Name: p["token"], CreatedAt: s.now().UTC()})
			i = len(reg.Tokens) - 1
			code = http.StatusCreated
		}
		reg.Tokens[i].ScopeMap, reg.Tokens[i].Status = scopeMap, status
		op = s.doneOperation(sub, "Microsoft.ContainerRegistry", reg.Location)
		out = s.renderToken(reg, reg.Tokens[i])
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: code, body: out, headers: s.asyncHeaders(r, op)}, nil
}

func (s *Simulation) getToken(_ *http.Request, p params) (reply, error) {
	var out map[string]any
	s.store.Read(func(sub *subscription) {
		if reg := lookup(sub.Registries, p["group"], p["name"]); reg != nil {
			for _, t := range reg.Tokens {
				if strings.EqualFold(t.Name, p["token"]) {
					out = s.renderToken(reg, t)
				}
			}
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeRegistry+"/tokens", p["name"]+"/"+p["token"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listTokens(r *http.Request, p params) (reply, error) {
	var out []map[string]any
	var found bool
	s.store.Read(func(sub *subscription) {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if found = reg != nil; found {
			for _, t := range reg.Tokens {
				out = append(out, s.renderToken(reg, t))
			}
		}
	})
	if !found {
		return reply{}, errNotFound(typeRegistry, p["name"], p["group"])
	}
	return listReply(r, out)
}

func (s *Simulation) deleteToken(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if reg == nil {
			return nil
		}
		before := len(reg.Tokens)
		reg.Tokens = slices.DeleteFunc(reg.Tokens, func(t *registryToken) bool { return strings.EqualFold(t.Name, p["token"]) })
		if len(reg.Tokens) < before {
			op = s.doneOperation(sub, "Microsoft.ContainerRegistry", reg.Location)
		}
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}

// generateCredentials makes a password for a token. The registry is a
// stock registry with static users, so every generated credential is the
// -azure-registry-username and -azure-registry-password; the expiry and the
// token's scope are recorded, not enforced.
func (s *Simulation) generateCredentials(r *http.Request, p params) (reply, error) {
	var req struct {
		TokenID string     `json:"tokenId"`
		Expiry  *time.Time `json:"expiry"`
		Name    string     `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	names := []string{"password1", "password2"}
	if req.Name != "" {
		name, ok := canonical(req.Name, names...)
		if !ok {
			return reply{}, errInvalidParameter("The password name '%s' is invalid; use password1 or password2.", req.Name)
		}
		names = []string{name}
	}
	var out map[string]any
	err := s.store.Update(func(sub *subscription) error {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if reg == nil {
			return errNotFound(typeRegistry, p["name"], p["group"])
		}
		var token *registryToken
		for _, t := range reg.Tokens {
			if sameID(req.TokenID, s.registryChildID(reg, "tokens", t.Name)) {
				token = t
			}
		}
		switch {
		case token == nil:
			return errNotFound(typeRegistry+"/tokens", req.TokenID, p["group"])
		case token.Status != "enabled":
			return errBadRequest("TokenDisabled", "The token %s is disabled.", token.Name)
		case req.Expiry != nil && !req.Expiry.After(s.now()):
			return errInvalidParameter("The expiry %s is in the past.", req.Expiry.Format(time.RFC3339))
		}
		now := s.now().UTC()
		var passwords []map[string]string
		for _, name := range names {
			token.Passwords = slices.DeleteFunc(token.Passwords, func(pw tokenPassword) bool { return pw.Name == name })
			token.Passwords = append(token.Passwords, tokenPassword{Name: name, Created: now, Expiry: req.Expiry})
			item := map[string]string{"name": name, "value": s.cfg.registry.password, "creationTime": timestamp(now)}
			if req.Expiry != nil {
				item["expiry"] = timestamp(*req.Expiry)
			}
			passwords = append(passwords, item)
		}
		out = map[string]any{"username": s.cfg.registry.username, "passwords": passwords}
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return okReply(out), nil
}
