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

var (
	repositoryIDPattern  = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	multiRegionLocations = []string{"us", "europe", "asia"}
)

// repository is an Artifact Registry repository. Only Docker repositories in
// standard mode are simulated; their images live in the registry that the
// operator runs under the location's registry host.
type repository struct {
	Name          string            `json:"name"`
	Format        string            `json:"format"`
	Description   string            `json:"description"`
	Labels        map[string]string `json:"labels"`
	ImmutableTags bool              `json:"immutable_tags"`
	Policy        policy            `json:"policy"`
	CreateTime    time.Time         `json:"create_time"`
	UpdateTime    time.Time         `json:"update_time"`
}

// longRunningOp is a google.longrunning.Operation of Artifact Registry. The
// simulation finishes each one before it answers.
type longRunningOp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

const maxLROs = 1000

func (s *Simulation) addArtifactRegistryRoutes(routes map[string]endpoint) {
	const p = "/v1/projects/{project}/locations/{location}"
	routes["POST "+p+"/repositories"] = s.createRepository
	routes["GET "+p+"/repositories"] = s.listRepositories
	routes["GET "+p+"/repositories/{repository}"] = s.getRepository
	routes["POST "+p+"/repositories/{repository}"] = s.setRepositoryPolicy
	routes["DELETE "+p+"/repositories/{repository}"] = s.deleteRepository
	routes["GET "+p+"/operations/{operation}"] = s.getLongRunningOp
}

// registryHost is the Docker registry host of a repository location.
func registryHost(location string) string {
	return location + "-docker.pkg.dev"
}

func (s *Simulation) location(r *http.Request) (string, error) {
	if p := r.PathValue("project"); p != s.cfg.project {
		return "", &apiError{http.StatusForbidden, "forbidden", "Permission denied on resource project " + p + "."}
	}
	location := r.PathValue("location")
	if !s.catalog.hasRegion(location) && !slices.Contains(multiRegionLocations, location) {
		return "", &apiError{http.StatusBadRequest, "badRequest", "Invalid location: " + location}
	}
	return location, nil
}

type repositoryJSON struct {
	Name                        string            `json:"name"`
	Format                      string            `json:"format"`
	Description                 string            `json:"description,omitempty"`
	Labels                      map[string]string `json:"labels,omitempty"`
	CreateTime                  string            `json:"createTime"`
	UpdateTime                  string            `json:"updateTime"`
	Mode                        string            `json:"mode"`
	SizeBytes                   string            `json:"sizeBytes"`
	RegistryURI                 string            `json:"registryUri"`
	DockerConfig                map[string]bool   `json:"dockerConfig,omitempty"`
	VulnerabilityScanningConfig map[string]string `json:"vulnerabilityScanningConfig"`
}

func renderRepository(repo *repository) repositoryJSON {
	parts := strings.Split(repo.Name, "/")
	out := repositoryJSON{
		Name: repo.Name, Format: repo.Format, Description: repo.Description, Labels: repo.Labels,
		CreateTime: repo.CreateTime.UTC().Format(time.RFC3339Nano), UpdateTime: repo.UpdateTime.UTC().Format(time.RFC3339Nano),
		Mode: "STANDARD_REPOSITORY", SizeBytes: "0", RegistryURI: registryHost(parts[3]) + "/" + parts[1] + "/" + parts[5],
		VulnerabilityScanningConfig: map[string]string{
			"enablementState":       "SCANNING_DISABLED",
			"enablementStateReason": "API containerscanning.googleapis.com is not enabled.",
		},
	}
	if repo.ImmutableTags {
		out.DockerConfig = map[string]bool{"immutableTags": true}
	}
	return out
}

type repositoryRequest struct {
	Format       string            `json:"format"`
	Description  string            `json:"description"`
	Labels       map[string]string `json:"labels"`
	Mode         string            `json:"mode"`
	DockerConfig struct {
		ImmutableTags bool `json:"immutableTags"`
	} `json:"dockerConfig"`
}

var (
	repositoryFields        = []string{"name", "format", "description", "labels", "mode", "dockerConfig", "cleanupPolicyDryRun"}
	ignoredRepositoryFields = []string{"kmsKeyName", "cleanupPolicies", "virtualRepositoryConfig", "remoteRepositoryConfig", "mavenConfig", "vulnerabilityScanningConfig"}
)

func (s *Simulation) createRepository(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	id := r.URL.Query().Get("repositoryId")
	if id == "" {
		id = r.URL.Query().Get("repository_id")
	}
	if !repositoryIDPattern.MatchString(id) {
		return 0, nil, &apiError{http.StatusBadRequest, "badRequest", "Invalid repository id " + id + ": it must start with a lowercase letter and contain only lowercase letters, digits and hyphens."}
	}
	var raw map[string]json.RawMessage
	if err := decodeBody(r, &raw); err != nil {
		return 0, nil, err
	}
	for key, value := range raw {
		if !slices.Contains(repositoryFields, key) && (!slices.Contains(ignoredRepositoryFields, key) || !emptyJSON(value)) {
			return 0, nil, errUnsupported("The repository field %s", key)
		}
	}
	var req repositoryRequest
	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, nil, &apiError{http.StatusBadRequest, "parseError", "Invalid JSON payload received. " + err.Error()}
	}
	switch {
	case req.Format != "DOCKER":
		return 0, nil, errUnsupported("Repository format %q", req.Format)
	case req.Mode != "" && req.Mode != "STANDARD_REPOSITORY":
		return 0, nil, errUnsupported("Repository mode %s", req.Mode)
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	name := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/" + id
	var op longRunningOp
	err = s.store.Update(func(w *world) error {
		if w.repository(name) != nil {
			return &apiError{http.StatusConflict, "alreadyExists", "the repository already exists"}
		}
		now := s.now()
		repo := &repository{Name: name, Format: req.Format, Description: req.Description, Labels: req.Labels, ImmutableTags: req.DockerConfig.ImmutableTags, CreateTime: now, UpdateTime: now}
		w.Repositories = append(w.Repositories, repo)
		op = s.newLongRunningOp(w, location, "type.googleapis.com/google.devtools.artifactregistry.v1.Repository", renderRepository(repo))
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, renderLongRunningOp(op), nil
}

func (s *Simulation) newLongRunningOp(w *world, location, responseType string, response any) longRunningOp {
	data, _ := json.Marshal(response)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields["@type"] = responseType
	body, _ := json.Marshal(fields)
	op := longRunningOp{Name: "projects/" + s.cfg.project + "/locations/" + location + "/operations/" + newUUID(), Response: body}
	w.LROs = append(w.LROs, &op)
	if len(w.LROs) > maxLROs {
		w.LROs = w.LROs[len(w.LROs)-maxLROs:]
	}
	return op
}

func renderLongRunningOp(op longRunningOp) map[string]any {
	return map[string]any{
		"name":     op.Name,
		"metadata": map[string]string{"@type": "type.googleapis.com/google.devtools.artifactregistry.v1.OperationMetadata"},
		"done":     true,
		"response": op.Response,
	}
}

func (s *Simulation) listRepositories(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	prefix := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/"
	out := []repositoryJSON{}
	s.store.Read(func(w *world) {
		for _, repo := range w.Repositories {
			if strings.HasPrefix(repo.Name, prefix) {
				out = append(out, renderRepository(repo))
			}
		}
	})
	return http.StatusOK, map[string]any{"repositories": out}, nil
}

// getRepository answers a repository, or its IAM policy for
// <repository>:getIamPolicy.
func (s *Simulation) getRepository(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	id, method, _ := strings.Cut(r.PathValue("repository"), ":")
	if method != "" && method != "getIamPolicy" {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("%s %s is not supported by the simulation", r.Method, r.URL.Path)}
	}
	name := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/" + id
	var out any
	s.store.Read(func(w *world) {
		repo := w.repository(name)
		switch {
		case repo == nil:
		case method == "getIamPolicy":
			out = renderPolicy(repo.Policy)
		default:
			out = renderRepository(repo)
		}
	})
	if out == nil {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", "Requested entity was not found."}
	}
	return http.StatusOK, out, nil
}

// setRepositoryPolicy serves <repository>:setIamPolicy.
func (s *Simulation) setRepositoryPolicy(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	id, method, _ := strings.Cut(r.PathValue("repository"), ":")
	if method != "setIamPolicy" {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("%s %s is not supported by the simulation", r.Method, r.URL.Path)}
	}
	name := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/" + id
	var out policyJSON
	err = s.store.Update(func(w *world) error {
		repo := w.repository(name)
		if repo == nil {
			return &apiError{http.StatusNotFound, "notFound", "Requested entity was not found."}
		}
		var err error
		out, err = setPolicy(&repo.Policy, r)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteRepository(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	name := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/" + r.PathValue("repository")
	var op longRunningOp
	err = s.store.Update(func(w *world) error {
		repo := w.repository(name)
		if repo == nil {
			return &apiError{http.StatusNotFound, "notFound", "Requested entity was not found."}
		}
		w.Repositories = slices.DeleteFunc(w.Repositories, func(other *repository) bool { return other == repo })
		op = s.newLongRunningOp(w, location, "type.googleapis.com/google.protobuf.Empty", nil)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, renderLongRunningOp(op), nil
}

func (s *Simulation) getLongRunningOp(r *http.Request) (int, any, error) {
	location, err := s.location(r)
	if err != nil {
		return 0, nil, err
	}
	name := "projects/" + s.cfg.project + "/locations/" + location + "/operations/" + r.PathValue("operation")
	var out *longRunningOp
	s.store.Read(func(w *world) {
		if op := find(w.LROs, func(op *longRunningOp) bool { return op.Name == name }); op != nil {
			found := *op
			out = &found
		}
	})
	if out == nil {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", "Requested entity was not found."}
	}
	return http.StatusOK, renderLongRunningOp(*out), nil
}
