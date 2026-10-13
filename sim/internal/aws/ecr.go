package aws

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ecrTargetPrefix = "AmazonEC2ContainerRegistry_V20150921."
	// ecrTokenLifetime is how long an ECR authorization token is valid.
	ecrTokenLifetime = 12 * time.Hour
)

var (
	repositoryNamePattern = regexp.MustCompile(`^(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	templatePrefixPattern = regexp.MustCompile(`^((?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*/?|ROOT)$`)
	appliedForValues      = []string{"REPLICATION", "PULL_THROUGH_CACHE", "CREATE_ON_PUSH"}
	mutabilityValues      = []string{"MUTABLE", "IMMUTABLE", "IMMUTABLE_WITH_EXCLUSION", "MUTABLE_WITH_EXCLUSION"}
)

type repository struct {
	Name       string          `json:"name"`
	Region     string          `json:"region"`
	CreatedAt  time.Time       `json:"created_at"`
	Mutability string          `json:"mutability"`
	ScanOnPush bool            `json:"scan_on_push"`
	Encryption json.RawMessage `json:"encryption"`
	Exclusions json.RawMessage `json:"exclusions,omitempty"`
}

// creationTemplate is a repository creation template. Its settings are
// stored and reported; the registry the operator runs does not apply them.
type creationTemplate struct {
	Prefix    string         `json:"prefix"`
	Region    string         `json:"region"`
	Settings  templateFields `json:"settings"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type templateFields struct {
	Description                        *string         `json:"description,omitempty"`
	EncryptionConfiguration            json.RawMessage `json:"encryptionConfiguration,omitempty"`
	ResourceTags                       json.RawMessage `json:"resourceTags,omitempty"`
	ImageTagMutability                 *string         `json:"imageTagMutability,omitempty"`
	ImageTagMutabilityExclusionFilters json.RawMessage `json:"imageTagMutabilityExclusionFilters,omitempty"`
	RepositoryPolicy                   *string         `json:"repositoryPolicy,omitempty"`
	LifecyclePolicy                    *string         `json:"lifecyclePolicy,omitempty"`
	AppliedFor                         []string        `json:"appliedFor,omitempty"`
	CustomRoleArn                      *string         `json:"customRoleArn,omitempty"`
}

// ecrCall is one ECR request, from the IAM user or from a federated
// session.
type ecrCall struct {
	region  string
	account string
	session *session
	body    []byte
}

// decode reads the request into v and refuses fields v does not have, so
// an unsupported option fails loudly.
func (c *ecrCall) decode(v any) error {
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &apiError{http.StatusBadRequest, "SerializationException", "the request is not supported by the simulation: " + err.Error()}
	}
	return nil
}

func ecrError(code, format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, code, fmt.Sprintf(format, args...)}
}

func (s *Simulation) ecrOperations() map[string]func(c *ecrCall) (any, error) {
	return map[string]func(c *ecrCall) (any, error){
		"GetAuthorizationToken":               s.getAuthorizationToken,
		"CreateRepository":                    s.createRepository,
		"DescribeRepositories":                s.describeRepositories,
		"DeleteRepository":                    s.deleteRepository,
		"CreateRepositoryCreationTemplate":    s.createCreationTemplate,
		"DescribeRepositoryCreationTemplates": s.describeCreationTemplates,
		"UpdateRepositoryCreationTemplate":    s.updateCreationTemplate,
		"DeleteRepositoryCreationTemplate":    s.deleteCreationTemplate,
	}
}

// serveECR answers the ECR JSON 1.1 API: the operation is in X-Amz-Target.
func (s *Simulation) serveECR(w http.ResponseWriter, r *http.Request, body []byte, sc scope, failure authFailure, requestID string) {
	if failure != 0 {
		writeECRError(w, requestID, ecrAuthError(failure, sc))
		return
	}
	name, _ := strings.CutPrefix(r.Header.Get("X-Amz-Target"), ecrTargetPrefix)
	op, found := s.ecrOperations()[name]
	if !found {
		s.log.Warn("unsupported ECR operation", "target", r.Header.Get("X-Amz-Target"))
		writeECRError(w, requestID, ecrError("UnknownOperationException", "operation %s is not supported by the simulation", r.Header.Get("X-Amz-Target")))
		return
	}
	c := &ecrCall{region: sc.region, session: sc.session, body: body}
	s.store.Read(func(wd *world) { c.account = wd.AccountID })
	if c.session != nil && !c.session.Policy.allows("ecr:"+name, "*") {
		writeECRError(w, requestID, ecrError("AccessDeniedException", "User: %s is not authorized to perform: ecr:%s on resource: * because no session policy allows the ecr:%s action",
			c.session.arn(c.account), name, name))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		c.body = []byte("{}")
	}
	out, err := op(c)
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			s.log.Error("ECR request failed", "operation", name, "error", err)
			apiErr = &apiError{http.StatusInternalServerError, "ServerException", "An internal error has occurred"}
		}
		writeECRError(w, requestID, apiErr)
		return
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("X-Amzn-Requestid", requestID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

func ecrAuthError(failure authFailure, sc scope) *apiError {
	switch failure {
	case authMissing:
		return &apiError{http.StatusBadRequest, "MissingAuthenticationTokenException", "Missing Authentication Token"}
	case authIncomplete:
		return &apiError{http.StatusBadRequest, "IncompleteSignatureException", "The request signature does not conform to AWS standards."}
	case authUnknownKey, authBadToken:
		return &apiError{http.StatusBadRequest, "UnrecognizedClientException", "The security token included in the request is invalid."}
	case authTokenExpired:
		return &apiError{http.StatusBadRequest, "ExpiredTokenException", "The security token included in the request is expired"}
	case authBadScope:
		return &apiError{http.StatusBadRequest, "InvalidSignatureException", fmt.Sprintf("Credential should be scoped to a valid region, not '%s'.", sc.region)}
	default:
		return &apiError{http.StatusBadRequest, "InvalidSignatureException", "The request signature we calculated does not match the signature you provided."}
	}
}

func writeECRError(w http.ResponseWriter, requestID string, e *apiError) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("X-Amzn-Requestid", requestID)
	w.Header().Set("X-Amzn-Errortype", e.code)
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": e.code, "message": e.message})
}

// registryHost is the host name of the account's registry in a region.
func registryHost(account, region string) string {
	return account + ".dkr.ecr." + region + ".amazonaws.com"
}

func epoch(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}

func checkRegistryID(c *ecrCall, id string) error {
	if id != "" && id != c.account {
		return ecrError("InvalidParameterException", "Invalid parameter at 'registryId' failed to satisfy constraint: 'must be the caller's account'")
	}
	return nil
}

// getAuthorizationToken returns a token for the user AWS with a new
// password, which signs Docker in to the registry of the request's region.
// The token may do in the registry what its caller may do: everything for
// the IAM user, and what the session policy allows for a federated session.
// It expires after 12 hours, as on ECR.
func (s *Simulation) getAuthorizationToken(c *ecrCall) (any, error) {
	var req struct {
		RegistryIDs []string `json:"registryIds"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	for _, id := range req.RegistryIDs {
		if err := checkRegistryID(c, id); err != nil {
			return nil, err
		}
	}
	now := s.now()
	password := rand.Text()
	t := &registryToken{Region: c.region, Subject: "arn:aws:iam::" + c.account + ":user/elprovo-sim", Expires: now.Add(ecrTokenLifetime).UTC()}
	if c.session != nil {
		t.Subject, t.Policy = c.session.arn(c.account), &c.session.Policy
	}
	err := s.store.Update(func(w *world) error {
		for p, old := range w.RegistryTokens {
			if !now.Before(old.Expires) {
				delete(w.RegistryTokens, p)
			}
		}
		w.RegistryTokens[password] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"authorizationData": []map[string]any{{
		"authorizationToken": base64.StdEncoding.EncodeToString([]byte("AWS:" + password)),
		"expiresAt":          epoch(t.Expires),
		"proxyEndpoint":      "https://" + registryHost(c.account, c.region),
	}}}, nil
}

func renderRepository(c *ecrCall, r *repository) map[string]any {
	out := map[string]any{
		"repositoryArn":              "arn:aws:ecr:" + r.Region + ":" + c.account + ":repository/" + r.Name,
		"registryId":                 c.account,
		"repositoryName":             r.Name,
		"repositoryUri":              registryHost(c.account, r.Region) + "/" + r.Name,
		"createdAt":                  epoch(r.CreatedAt),
		"imageTagMutability":         r.Mutability,
		"imageScanningConfiguration": map[string]bool{"scanOnPush": r.ScanOnPush},
		"encryptionConfiguration":    r.Encryption,
	}
	if len(r.Exclusions) > 0 {
		out["imageTagMutabilityExclusionFilters"] = r.Exclusions
	}
	return out
}

func (s *Simulation) createRepository(c *ecrCall) (any, error) {
	var req struct {
		RegistryID                         string          `json:"registryId"`
		RepositoryName                     string          `json:"repositoryName"`
		ImageTagMutability                 string          `json:"imageTagMutability"`
		ImageTagMutabilityExclusionFilters json.RawMessage `json:"imageTagMutabilityExclusionFilters"`
		ImageScanningConfiguration         *struct {
			ScanOnPush bool `json:"scanOnPush"`
		} `json:"imageScanningConfiguration"`
		EncryptionConfiguration json.RawMessage `json:"encryptionConfiguration"`
		Tags                    []struct {
			Key   string `json:"Key"`
			Value string `json:"Value"`
		} `json:"tags"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	if err := checkRegistryID(c, req.RegistryID); err != nil {
		return nil, err
	}
	if len(req.RepositoryName) < 2 || len(req.RepositoryName) > 256 || !repositoryNamePattern.MatchString(req.RepositoryName) {
		return nil, ecrError("InvalidParameterException", "Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must satisfy regular expression '%s''", repositoryNamePattern)
	}
	if req.ImageTagMutability == "" {
		req.ImageTagMutability = "MUTABLE"
	}
	if !slices.Contains(mutabilityValues, req.ImageTagMutability) {
		return nil, ecrError("InvalidParameterException", "Invalid parameter at 'imageTagMutability'")
	}
	if len(req.Tags) > 0 {
		return nil, ecrError("InvalidParameterException", "repository tags are not supported by the simulation")
	}
	if len(req.EncryptionConfiguration) == 0 {
		req.EncryptionConfiguration = json.RawMessage(`{"encryptionType":"AES256"}`)
	}
	r := &repository{Name: req.RepositoryName, Region: c.region, CreatedAt: s.now().UTC(), Mutability: req.ImageTagMutability, Encryption: req.EncryptionConfiguration, Exclusions: req.ImageTagMutabilityExclusionFilters}
	if req.ImageScanningConfiguration != nil {
		r.ScanOnPush = req.ImageScanningConfiguration.ScanOnPush
	}
	err := s.store.Update(func(w *world) error {
		if slices.ContainsFunc(w.Repositories, func(o *repository) bool { return o.Region == c.region && o.Name == r.Name }) {
			return ecrError("RepositoryAlreadyExistsException", "The repository with name '%s' already exists in the registry with id '%s'", r.Name, c.account)
		}
		w.Repositories = append(w.Repositories, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"repository": renderRepository(c, r)}, nil
}

func (s *Simulation) describeRepositories(c *ecrCall) (any, error) {
	var req struct {
		RegistryID      string   `json:"registryId"`
		RepositoryNames []string `json:"repositoryNames"`
		MaxResults      int      `json:"maxResults"`
		NextToken       string   `json:"nextToken"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	if err := checkRegistryID(c, req.RegistryID); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	var err error
	s.store.Read(func(w *world) {
		for _, name := range req.RepositoryNames {
			if !slices.ContainsFunc(w.Repositories, func(r *repository) bool { return r.Region == c.region && r.Name == name }) {
				err = ecrError("RepositoryNotFoundException", "The repository with name '%s' does not exist in the registry with id '%s'", name, c.account)
				return
			}
		}
		for _, r := range w.Repositories {
			if r.Region == c.region && (len(req.RepositoryNames) == 0 || slices.Contains(req.RepositoryNames, r.Name)) {
				out = append(out, renderRepository(c, r))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"repositories": out}, nil
}

func (s *Simulation) deleteRepository(c *ecrCall) (any, error) {
	var req struct {
		RegistryID     string `json:"registryId"`
		RepositoryName string `json:"repositoryName"`
		Force          bool   `json:"force"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	if err := checkRegistryID(c, req.RegistryID); err != nil {
		return nil, err
	}
	var deleted map[string]any
	err := s.store.Update(func(w *world) error {
		r := find(w.Repositories, func(r *repository) bool { return r.Region == c.region && r.Name == req.RepositoryName })
		if r == nil {
			return ecrError("RepositoryNotFoundException", "The repository with name '%s' does not exist in the registry with id '%s'", req.RepositoryName, c.account)
		}
		deleted = renderRepository(c, r)
		w.Repositories = slices.DeleteFunc(w.Repositories, func(x *repository) bool { return x == r })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"repository": deleted}, nil
}

type templateRequest struct {
	Prefix string `json:"prefix"`
	templateFields
}

func checkTemplate(f templateFields, creating bool) error {
	if creating && len(f.AppliedFor) == 0 {
		return ecrError("InvalidParameterException", "Invalid parameter at 'appliedFor' failed to satisfy constraint: 'Member must have length greater than or equal to 1'")
	}
	for _, v := range f.AppliedFor {
		if !slices.Contains(appliedForValues, v) {
			return ecrError("InvalidParameterException", "Invalid parameter at 'appliedFor': %s is not one of %v", v, appliedForValues)
		}
	}
	if f.ImageTagMutability != nil && !slices.Contains(mutabilityValues, *f.ImageTagMutability) {
		return ecrError("InvalidParameterException", "Invalid parameter at 'imageTagMutability'")
	}
	return nil
}

func renderTemplate(t *creationTemplate) map[string]any {
	f := t.Settings
	out := map[string]any{
		"prefix": t.Prefix, "appliedFor": f.AppliedFor, "createdAt": epoch(t.CreatedAt), "updatedAt": epoch(t.UpdatedAt),
		"imageTagMutability": "MUTABLE", "encryptionConfiguration": json.RawMessage(`{"encryptionType":"AES256"}`),
		"resourceTags": json.RawMessage(`[]`),
	}
	if f.Description != nil {
		out["description"] = *f.Description
	}
	if len(f.EncryptionConfiguration) > 0 {
		out["encryptionConfiguration"] = f.EncryptionConfiguration
	}
	if len(f.ResourceTags) > 0 {
		out["resourceTags"] = f.ResourceTags
	}
	if f.ImageTagMutability != nil && *f.ImageTagMutability != "" {
		out["imageTagMutability"] = *f.ImageTagMutability
	}
	if len(f.ImageTagMutabilityExclusionFilters) > 0 {
		out["imageTagMutabilityExclusionFilters"] = f.ImageTagMutabilityExclusionFilters
	}
	if f.RepositoryPolicy != nil {
		out["repositoryPolicy"] = *f.RepositoryPolicy
	}
	if f.LifecyclePolicy != nil {
		out["lifecyclePolicy"] = *f.LifecyclePolicy
	}
	if f.CustomRoleArn != nil {
		out["customRoleArn"] = *f.CustomRoleArn
	}
	return out
}

func templateNotFound(prefix string) *apiError {
	return ecrError("TemplateNotFoundException", "The specified repository creation template with prefix '%s' does not exist", prefix)
}

func (s *Simulation) createCreationTemplate(c *ecrCall) (any, error) {
	var req templateRequest
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	if !templatePrefixPattern.MatchString(req.Prefix) {
		return nil, ecrError("InvalidParameterException", "Invalid parameter at 'prefix'")
	}
	if err := checkTemplate(req.templateFields, true); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	t := &creationTemplate{Prefix: req.Prefix, Region: c.region, Settings: req.templateFields, CreatedAt: now, UpdatedAt: now}
	err := s.store.Update(func(w *world) error {
		if slices.ContainsFunc(w.CreationTemplates, func(o *creationTemplate) bool { return o.Region == c.region && o.Prefix == t.Prefix }) {
			return ecrError("TemplateAlreadyExistsException", "The repository creation template with prefix '%s' already exists", t.Prefix)
		}
		w.CreationTemplates = append(w.CreationTemplates, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"registryId": c.account, "repositoryCreationTemplate": renderTemplate(t)}, nil
}

func (s *Simulation) describeCreationTemplates(c *ecrCall) (any, error) {
	var req struct {
		Prefixes   []string `json:"prefixes"`
		MaxResults int      `json:"maxResults"`
		NextToken  string   `json:"nextToken"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	var err error
	s.store.Read(func(w *world) {
		for _, p := range req.Prefixes {
			if !slices.ContainsFunc(w.CreationTemplates, func(t *creationTemplate) bool { return t.Region == c.region && t.Prefix == p }) {
				err = templateNotFound(p)
				return
			}
		}
		for _, t := range w.CreationTemplates {
			if t.Region == c.region && (len(req.Prefixes) == 0 || slices.Contains(req.Prefixes, t.Prefix)) {
				out = append(out, renderTemplate(t))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"registryId": c.account, "repositoryCreationTemplates": out}, nil
}

func (s *Simulation) updateCreationTemplate(c *ecrCall) (any, error) {
	var req templateRequest
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	if err := checkTemplate(req.templateFields, false); err != nil {
		return nil, err
	}
	var out map[string]any
	err := s.store.Update(func(w *world) error {
		t := find(w.CreationTemplates, func(t *creationTemplate) bool { return t.Region == c.region && t.Prefix == req.Prefix })
		if t == nil {
			return templateNotFound(req.Prefix)
		}
		next, f := t.Settings, req.templateFields
		if f.Description != nil {
			next.Description = f.Description
		}
		if f.EncryptionConfiguration != nil {
			next.EncryptionConfiguration = f.EncryptionConfiguration
		}
		if f.ResourceTags != nil {
			next.ResourceTags = f.ResourceTags
		}
		if f.ImageTagMutability != nil {
			next.ImageTagMutability = f.ImageTagMutability
		}
		if f.ImageTagMutabilityExclusionFilters != nil {
			next.ImageTagMutabilityExclusionFilters = f.ImageTagMutabilityExclusionFilters
		}
		if f.RepositoryPolicy != nil {
			next.RepositoryPolicy = f.RepositoryPolicy
		}
		if f.LifecyclePolicy != nil {
			next.LifecyclePolicy = f.LifecyclePolicy
		}
		if f.AppliedFor != nil {
			next.AppliedFor = f.AppliedFor
		}
		if f.CustomRoleArn != nil {
			next.CustomRoleArn = f.CustomRoleArn
		}
		t.Settings, t.UpdatedAt = next, s.now().UTC()
		out = renderTemplate(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"registryId": c.account, "repositoryCreationTemplate": out}, nil
}

func (s *Simulation) deleteCreationTemplate(c *ecrCall) (any, error) {
	var req struct {
		Prefix string `json:"prefix"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	var out map[string]any
	err := s.store.Update(func(w *world) error {
		t := find(w.CreationTemplates, func(t *creationTemplate) bool { return t.Region == c.region && t.Prefix == req.Prefix })
		if t == nil {
			return templateNotFound(req.Prefix)
		}
		out = renderTemplate(t)
		w.CreationTemplates = slices.DeleteFunc(w.CreationTemplates, func(x *creationTemplate) bool { return x == t })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"registryId": c.account, "repositoryCreationTemplate": out}, nil
}
