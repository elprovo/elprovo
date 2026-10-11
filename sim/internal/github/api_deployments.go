package github

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

var (
	statusStates           = []string{"error", "failure", "pending", "success"}
	deploymentStatusStates = []string{"error", "failure", "inactive", "in_progress", "queued", "pending", "success"}
)

func (s *Sim) writableHandle(c caller, r *http.Request, perm string) (repoHandle, error) {
	var h repoHandle
	var err error
	s.read(func(v view) {
		var rp *repo
		if rp, err = v.repoFor(c, r, permWrite, perm); err != nil {
			return
		}
		owner := v.w.account(rp.OwnerID).Login
		h = repoHandle{owner: owner, name: rp.Name, defaultBranch: rp.DefaultBranch, git: s.gitRepo(owner, rp.Name)}
	})
	return h, err
}

type statusInput struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

func (s *Sim) createStatus(w http.ResponseWriter, r *http.Request, c caller) {
	var in statusInput
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !slices.Contains(statusStates, in.State) {
		s.writeError(w, r, errValidation("Status", "state", "invalid"))
		return
	}
	h, err := s.writableHandle(c, r, "statuses")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sha, err := h.git.ResolveCommit(r.Context(), r.PathValue("sha"))
	if isNotFound(err) {
		err = fail(http.StatusUnprocessableEntity, "No commit found for SHA: %s", r.PathValue("sha"))
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out statusJSON
	err = s.update(func(t *tx) error {
		rp := t.w.repo(h.owner, h.name)
		creator, _ := t.actor(c)
		st := &status{
			ID: t.w.newID(), SHA: sha, State: in.State, TargetURL: in.TargetURL, Description: in.Description,
			Context: cmp.Or(in.Context, "default"), CreatorID: creator, CreatedAt: now(),
		}
		rp.Statuses = append(rp.Statuses, st)
		out = t.status(rp, st)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type deploymentInput struct {
	Ref                   string          `json:"ref"`
	Task                  string          `json:"task"`
	RequiredContexts      *[]string       `json:"required_contexts"`
	Payload               json.RawMessage `json:"payload"`
	Environment           string          `json:"environment"`
	Description           string          `json:"description"`
	TransientEnvironment  bool            `json:"transient_environment"`
	ProductionEnvironment *bool           `json:"production_environment"`
}

func (s *Sim) createDeployment(w http.ResponseWriter, r *http.Request, c caller) {
	var in deploymentInput
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if in.Ref == "" {
		s.writeError(w, r, errValidation("Deployment", "ref", "missing_field"))
		return
	}
	h, err := s.writableHandle(c, r, "deployments")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sha, err := h.git.ResolveCommit(r.Context(), in.Ref)
	if isNotFound(err) {
		err = fail(http.StatusUnprocessableEntity, "No ref found for: %s", in.Ref)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out deploymentJSON
	err = s.update(func(t *tx) error {
		rp := t.w.repo(h.owner, h.name)
		if err := checkRequiredContexts(rp, sha, in); err != nil {
			return err
		}
		d := newDeployment(t, c, sha, in)
		rp.Deployments = append(rp.Deployments, d)
		out = t.deployment(rp, d)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func newDeployment(t *tx, c caller, sha string, in deploymentInput) *deployment {
	env := cmp.Or(in.Environment, "production")
	production := env == "production"
	if in.ProductionEnvironment != nil {
		production = *in.ProductionEnvironment
	}
	payload := in.Payload
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage("{}")
	}
	creator, appID := t.actor(c)
	return &deployment{
		ID: t.w.newID(), SHA: sha, Ref: in.Ref, Task: cmp.Or(in.Task, "deploy"), Payload: payload,
		Environment: env, Description: in.Description, Transient: in.TransientEnvironment, Production: production,
		CreatorID: creator, AppID: appID, CreatedAt: now(),
	}
}

func checkRequiredContexts(r *repo, sha string, in deploymentInput) error {
	latest := map[string]string{}
	for _, st := range r.Statuses {
		if st.SHA == sha {
			latest[st.Context] = st.State
		}
	}
	var required []string
	if in.RequiredContexts != nil {
		required = *in.RequiredContexts
	} else {
		for ctx := range latest {
			required = append(required, ctx)
		}
	}
	var failed []string
	for _, ctx := range required {
		if latest[ctx] != "success" {
			failed = append(failed, ctx)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	slices.Sort(failed)
	return &apiError{
		status:  http.StatusConflict,
		message: fmt.Sprintf("Conflict: Commit status checks failed for %s.", in.Ref),
		errors:  []fieldError{{Resource: "Deployment", Field: "required_contexts", Code: "invalid", Message: fmt.Sprint(failed)}},
	}
}

type deploymentStatusInput struct {
	State          string `json:"state"`
	TargetURL      string `json:"target_url"`
	LogURL         string `json:"log_url"`
	Description    string `json:"description"`
	Environment    string `json:"environment"`
	EnvironmentURL string `json:"environment_url"`
}

func (s *Sim) createDeploymentStatus(w http.ResponseWriter, r *http.Request, c caller) {
	var in deploymentStatusInput
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !slices.Contains(deploymentStatusStates, in.State) {
		s.writeError(w, r, errValidation("DeploymentStatus", "state", "invalid"))
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out deploymentStatusJSON
	err = s.update(func(t *tx) error {
		rp, err := t.repoFor(c, r, permWrite, "deployments")
		if err != nil {
			return err
		}
		d := rp.deployment(id)
		if d == nil {
			return errNotFound()
		}
		creator, appID := t.actor(c)
		d.Environment = cmp.Or(in.Environment, d.Environment)
		st := &deploymentStatus{
			ID: t.w.newID(), State: in.State, TargetURL: in.TargetURL, LogURL: cmp.Or(in.LogURL, in.TargetURL),
			Description: in.Description, Environment: d.Environment, EnvironmentURL: in.EnvironmentURL,
			CreatorID: creator, AppID: appID, CreatedAt: now(),
		}
		d.Statuses = append(d.Statuses, st)
		out = t.deploymentStatus(rp, d, st)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
