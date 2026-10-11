package gcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	tokenCreatorRole     = "roles/iam.serviceAccountTokenCreator"
	maxImpersonationLife = time.Hour
)

var (
	accountIDPattern = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]$`)
	memberPattern    = regexp.MustCompile(`^(serviceAccount|user|group|domain):[^\s]+$`)
)

// iamAccount is a service account of the project. Only the project's own
// account has a key; the simulation issues tokens for the others when a
// caller that their policy lets impersonate them asks.
type iamAccount struct {
	Email       string    `json:"email"`
	UniqueID    string    `json:"unique_id"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	Policy      policy    `json:"policy"`
	CreatedAt   time.Time `json:"created_at"`
}

// policy is an IAM allow policy. Conditions are not simulated.
type policy struct {
	Bindings []binding `json:"bindings"`
	Serial   int       `json:"serial"`
}

type binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

func (p policy) etag() string {
	return fingerprint(fmt.Sprint(p.Serial, jsonKey(p.Bindings)))
}

// has reports whether member holds one of roles.
func (p policy) has(member string, roles ...string) bool {
	return slices.ContainsFunc(p.Bindings, func(b binding) bool {
		return slices.Contains(roles, b.Role) && slices.ContainsFunc(b.Members, func(m string) bool { return strings.EqualFold(m, member) })
	})
}

type policyJSON struct {
	Version  int       `json:"version"`
	Etag     string    `json:"etag"`
	Bindings []binding `json:"bindings,omitempty"`
}

func renderPolicy(p policy) policyJSON {
	return policyJSON{Version: 1, Etag: p.etag(), Bindings: p.Bindings}
}

// setPolicy replaces p with the policy in a setIamPolicy request, when the
// request's etag, if any, matches.
func setPolicy(p *policy, r *http.Request) (policyJSON, error) {
	var req struct {
		Policy struct {
			Etag     string            `json:"etag"`
			Bindings []json.RawMessage `json:"bindings"`
		} `json:"policy"`
	}
	if err := decodeBody(r, &req); err != nil {
		return policyJSON{}, err
	}
	if req.Policy.Etag != "" && req.Policy.Etag != p.etag() {
		return policyJSON{}, &apiError{http.StatusConflict, "aborted", "There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff."}
	}
	var bindings []binding
	for _, raw := range req.Policy.Bindings {
		var b struct {
			binding
			Condition json.RawMessage `json:"condition"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return policyJSON{}, &apiError{http.StatusBadRequest, "parseError", "Invalid JSON payload received. " + err.Error()}
		}
		switch {
		case len(b.Condition) > 0 && !emptyJSON(b.Condition):
			return policyJSON{}, errUnsupported("An IAM condition")
		case !strings.HasPrefix(b.Role, "roles/"):
			return policyJSON{}, errInvalid("Role %s is not supported for this resource.", b.Role)
		}
		for _, m := range b.Members {
			if !memberPattern.MatchString(m) {
				return policyJSON{}, errInvalid("Invalid member: %s", m)
			}
		}
		if len(b.Members) > 0 {
			bindings = append(bindings, b.binding)
		}
	}
	p.Bindings, p.Serial = bindings, p.Serial+1
	return renderPolicy(*p), nil
}

type accountJSON struct {
	Name           string `json:"name"`
	ProjectID      string `json:"projectId"`
	UniqueID       string `json:"uniqueId"`
	Email          string `json:"email"`
	DisplayName    string `json:"displayName,omitempty"`
	Description    string `json:"description,omitempty"`
	Etag           string `json:"etag"`
	OAuth2ClientID string `json:"oauth2ClientId"`
	Disabled       bool   `json:"disabled"`
}

func (s *Simulation) renderAccount(a *iamAccount) accountJSON {
	return accountJSON{
		Name: "projects/" + s.cfg.project + "/serviceAccounts/" + a.Email, ProjectID: s.cfg.project, UniqueID: a.UniqueID,
		Email: a.Email, DisplayName: a.DisplayName, Description: a.Description, Etag: "MDEwMjE5MjA=", OAuth2ClientID: a.UniqueID,
	}
}

func (s *Simulation) addIAMRoutes(routes map[string]endpoint) {
	const p = "/v1/projects/{project}/serviceAccounts"
	routes["POST "+p] = s.createAccount
	routes["GET "+p+"/{account}"] = s.getAccount
	routes["DELETE "+p+"/{account}"] = s.deleteAccount
	routes["POST "+p+"/{account}"] = s.accountMethod
}

func (s *Simulation) checkIAMProject(r *http.Request) error {
	if p := r.PathValue("project"); p != s.cfg.project && p != "-" {
		return &apiError{http.StatusForbidden, "forbidden", "Permission denied on resource project " + p + "."}
	}
	return nil
}

func (s *Simulation) createAccount(r *http.Request) (int, any, error) {
	if p := r.PathValue("project"); p != s.cfg.project {
		return 0, nil, &apiError{http.StatusForbidden, "forbidden", "Permission denied on resource project " + p + "."}
	}
	var req struct {
		AccountID      string `json:"accountId"`
		ServiceAccount struct {
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"serviceAccount"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if !accountIDPattern.MatchString(req.AccountID) {
		return 0, nil, errInvalid("Account ID %q must be 6 to 30 lowercase letters, digits or hyphens, starting with a letter.", req.AccountID)
	}
	email := req.AccountID + "@" + s.cfg.project + ".iam.gserviceaccount.com"
	var out accountJSON
	err := s.store.Update(func(w *world) error {
		if w.account(email) != nil {
			return &apiError{http.StatusConflict, "alreadyExists", "Service account " + req.AccountID + " already exists within project projects/" + s.cfg.project + "."}
		}
		a := &iamAccount{
			Email: email, UniqueID: fmt.Sprintf("1%020d", w.nextID()), DisplayName: req.ServiceAccount.DisplayName,
			Description: req.ServiceAccount.Description, CreatedAt: s.now().UTC(),
		}
		w.Accounts = append(w.Accounts, a)
		out = s.renderAccount(a)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) getAccount(r *http.Request) (int, any, error) {
	if err := s.checkIAMProject(r); err != nil {
		return 0, nil, err
	}
	var out *accountJSON
	s.store.Read(func(w *world) {
		if a := w.account(r.PathValue("account")); a != nil {
			rendered := s.renderAccount(a)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", "Unknown service account"}
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteAccount(r *http.Request) (int, any, error) {
	if err := s.checkIAMProject(r); err != nil {
		return 0, nil, err
	}
	email := r.PathValue("account")
	err := s.store.Update(func(w *world) error {
		a := w.account(email)
		if a == nil {
			return &apiError{http.StatusNotFound, "notFound", "Unknown service account"}
		}
		if a.Email == w.Keys[0].Email {
			return errUnsupported("Deleting the project's own service account")
		}
		w.Accounts = slices.DeleteFunc(w.Accounts, func(other *iamAccount) bool { return other == a })
		for t, issued := range w.Tokens {
			if issued.Email == a.Email {
				delete(w.Tokens, t)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{}, nil
}

// accountMethod serves the custom methods on a service account:
// getIamPolicy and setIamPolicy of the IAM API, and generateAccessToken of
// the IAM Service Account Credentials API.
func (s *Simulation) accountMethod(r *http.Request) (int, any, error) {
	if err := s.checkIAMProject(r); err != nil {
		return 0, nil, err
	}
	email, method, _ := strings.Cut(r.PathValue("account"), ":")
	switch method {
	case "getIamPolicy", "setIamPolicy":
		return s.accountPolicy(r, email, method)
	case "generateAccessToken":
		return s.generateAccessToken(r, email)
	}
	return 0, nil, &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("%s %s is not supported by the simulation", r.Method, r.URL.Path)}
}

func (s *Simulation) accountPolicy(r *http.Request, email, method string) (int, any, error) {
	var out policyJSON
	err := s.store.Update(func(w *world) error {
		a := w.account(email)
		if a == nil {
			return &apiError{http.StatusNotFound, "notFound", "Unknown service account"}
		}
		if method == "getIamPolicy" {
			out = renderPolicy(a.Policy)
			return nil
		}
		var err error
		out, err = setPolicy(&a.Policy, r)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

// generateAccessToken issues an access token of the service account when
// the account's policy gives the caller roles/iam.serviceAccountTokenCreator.
// The token lasts the requested lifetime, an hour at most.
func (s *Simulation) generateAccessToken(r *http.Request, email string) (int, any, error) {
	var req struct {
		Scope     []string `json:"scope"`
		Lifetime  string   `json:"lifetime"`
		Delegates []string `json:"delegates"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	life := maxImpersonationLife
	if req.Lifetime != "" {
		d, err := time.ParseDuration(req.Lifetime)
		if err != nil || !strings.HasSuffix(req.Lifetime, "s") || d <= 0 {
			return 0, nil, errInvalid("Invalid value at 'lifetime' (type.googleapis.com/google.protobuf.Duration), %q", req.Lifetime)
		}
		life = d
	}
	switch {
	case len(req.Scope) == 0:
		return 0, nil, errInvalid("Scope required.")
	case len(req.Delegates) > 0:
		return 0, nil, errUnsupported("Delegation chains (delegates)")
	case life > maxImpersonationLife:
		return 0, nil, errInvalid("The lifetime of the requested token must be at most 3600 seconds.")
	}
	allowed := false
	s.store.Read(func(w *world) {
		a := w.account(email)
		allowed = a != nil && a.Policy.has("serviceAccount:"+caller(r), tokenCreatorRole)
	})
	if !allowed {
		return 0, nil, &apiError{http.StatusForbidden, "forbidden", "Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist)."}
	}
	token, err := s.issueToken(email, life)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]string{"accessToken": token, "expireTime": s.now().Add(life).UTC().Format(time.RFC3339)}, nil
}
