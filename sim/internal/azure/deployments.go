package azure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// deployableTypes are the resource types a template may deploy, each with
// the API path under its resource group that creates it.
var deployableTypes = []string{typePublicIP, typeVNet, typeNSG, typeNIC, typeVM}

type deployment struct {
	meta
	Mode              string            `json:"mode"`
	Parameters        json.RawMessage   `json:"parameters"`
	TemplateHash      string            `json:"template_hash"`
	ProvisioningState string            `json:"provisioning_state"`
	Started           time.Time         `json:"started"`
	Ended             *time.Time        `json:"ended,omitempty"`
	CorrelationID     string            `json:"correlation_id"`
	Operations        []deploymentStep  `json:"operations"`
	Error             *opError          `json:"error,omitempty"`
	Resources         []plannedResource `json:"resources"`
}

// deploymentStep is one resource's deployment operation.
type deploymentStep struct {
	ID         string     `json:"id"`
	ResourceID string     `json:"resource_id"`
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	APIVersion string     `json:"api_version"`
	State      string     `json:"state"`
	StatusCode string     `json:"status_code"`
	Started    time.Time  `json:"started"`
	Ended      *time.Time `json:"ended,omitempty"`
	Error      *opError   `json:"error,omitempty"`
}

// plannedResource is a template resource with every expression evaluated.
type plannedResource struct {
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	APIVersion string          `json:"api_version"`
	Body       json.RawMessage `json:"body"`
	DependsOn  []string        `json:"depends_on"`
}

func (r plannedResource) key() string { return strings.ToLower(r.Type + "/" + r.Name) }

type deploymentRequest struct {
	Location   string `json:"location"`
	Properties struct {
		Mode           string          `json:"mode"`
		Template       json.RawMessage `json:"template"`
		Parameters     json.RawMessage `json:"parameters"`
		TemplateLink   json.RawMessage `json:"templateLink"`
		ParametersLink json.RawMessage `json:"parametersLink"`
	} `json:"properties"`
}

type templateDoc struct {
	Parameters map[string]struct {
		Type         string `json:"type"`
		DefaultValue any    `json:"defaultValue"`
	} `json:"parameters"`
	Variables map[string]any   `json:"variables"`
	Resources []map[string]any `json:"resources"`
}

func errInvalidTemplate(format string, args ...any) *armError {
	return errBadRequest("InvalidTemplate", "Deployment template validation failed: '"+format+"'.", args...)
}

// planDeployment evaluates the template against the group and returns its
// resources in an order that creates each after the ones it depends on.
func (s *Simulation) planDeployment(g *group, req deploymentRequest) ([]plannedResource, string, error) {
	props := req.Properties
	switch {
	case len(props.TemplateLink) > 0 && string(props.TemplateLink) != "null", len(props.ParametersLink) > 0 && string(props.ParametersLink) != "null":
		return nil, "", errUnsupported("A linked template or parameters file")
	case !strings.EqualFold(props.Mode, "Incremental"):
		return nil, "", errUnsupported("The deployment mode %q; use Incremental", props.Mode)
	case len(props.Template) == 0:
		return nil, "", errInvalidTemplate("The template is missing")
	}
	var doc templateDoc
	if err := json.Unmarshal(props.Template, &doc); err != nil {
		return nil, "", errInvalidTemplate("The template is not valid JSON: %v", err)
	}
	scope := &templateScope{sim: s, group: g, parameters: map[string]any{}, variables: doc.Variables, evaluated: map[string]any{}, evaluating: map[string]bool{}}
	given := map[string]struct {
		Value     any             `json:"value"`
		Reference json.RawMessage `json:"reference"`
	}{}
	if len(props.Parameters) > 0 && string(props.Parameters) != "null" {
		if err := json.Unmarshal(props.Parameters, &given); err != nil {
			return nil, "", errInvalidTemplate("The parameters are not valid: %v", err)
		}
	}
	for name, def := range doc.Parameters {
		v, ok := given[name]
		switch {
		case ok && len(v.Reference) > 0:
			return nil, "", errUnsupported("A Key Vault parameter reference")
		case ok:
			scope.parameters[name] = v.Value
		case def.DefaultValue != nil:
			scope.parameters[name] = def.DefaultValue
		default:
			return nil, "", errInvalidTemplate("The value for the template parameter '%s' is not provided", name)
		}
	}
	var planned []plannedResource
	for i, raw := range doc.Resources {
		r, keep, err := s.planResource(scope, raw)
		if err != nil {
			return nil, "", errInvalidTemplate("resource %d: %v", i+1, err)
		}
		if keep {
			planned = append(planned, r)
		}
	}
	ordered, err := orderResources(planned)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(props.Template)
	return ordered, hex.EncodeToString(hash[:8]), nil
}

func (s *Simulation) planResource(scope *templateScope, raw map[string]any) (plannedResource, bool, error) {
	if cond, ok := raw["condition"]; ok {
		v, err := scope.eval(cond)
		if err != nil {
			return plannedResource{}, false, err
		}
		if b, ok := v.(bool); ok && !b {
			return plannedResource{}, false, nil
		}
	}
	for _, unsupported := range []string{"copy", "resources", "scope", "kind"} {
		if _, ok := raw[unsupported]; ok {
			return plannedResource{}, false, fmt.Errorf("the resource property %q is not supported by the simulation", unsupported)
		}
	}
	str := func(key string) (string, error) {
		v, err := scope.eval(raw[key])
		if err != nil {
			return "", err
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("the resource %s must be a string", key)
		}
		return s, nil
	}
	typ, err := str("type")
	if err != nil {
		return plannedResource{}, false, err
	}
	known, ok := canonical(typ, deployableTypes...)
	if !ok {
		return plannedResource{}, false, fmt.Errorf("the resource type %s is not supported by the simulation", typ)
	}
	r := plannedResource{Type: known}
	if r.Name, err = str("name"); err != nil {
		return r, false, err
	}
	if r.APIVersion, err = str("apiVersion"); err != nil {
		return r, false, err
	}
	body := map[string]any{}
	for _, key := range []string{"location", "tags", "sku", "zones", "properties", "plan", "identity"} {
		if v, ok := raw[key]; ok {
			if body[key], err = scope.eval(v); err != nil {
				return r, false, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if r.Body, err = json.Marshal(body); err != nil {
		return r, false, err
	}
	deps, err := scope.eval(raw["dependsOn"])
	if err != nil {
		return r, false, err
	}
	list, _ := deps.([]any)
	for _, d := range list {
		if ds, ok := d.(string); ok {
			r.DependsOn = append(r.DependsOn, ds)
		}
	}
	return r, true, nil
}

// orderResources sorts the resources so each comes after those it depends
// on. A dependency names a resource by id or by type/name.
func orderResources(resources []plannedResource) ([]plannedResource, error) {
	byKey := map[string]int{}
	for i, r := range resources {
		byKey[r.key()] = i
	}
	resolve := func(dep string) (int, bool) {
		d := strings.ToLower(strings.TrimSuffix(dep, "/"))
		for key, i := range byKey {
			if d == key || strings.HasSuffix(d, "/"+key) {
				return i, true
			}
		}
		return 0, false
	}
	state := make([]int, len(resources))
	var out []plannedResource
	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case 1:
			return errInvalidTemplate("Circular dependency detected on resource '%s'", resources[i].key())
		case 2:
			return nil
		}
		state[i] = 1
		for _, dep := range resources[i].DependsOn {
			j, ok := resolve(dep)
			if !ok {
				return errInvalidTemplate("The resource '%s' is not defined in the template", dep)
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = 2
		out = append(out, resources[i])
		return nil
	}
	for i := range resources {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Simulation) deploymentID(group, name string) string {
	return s.groupID(group) + "/providers/" + typeDeployment + "/" + name
}

func (s *Simulation) renderDeployment(d *deployment) map[string]any {
	props := map[string]any{
		"provisioningState": d.ProvisioningState, "mode": "Incremental", "templateHash": d.TemplateHash, "correlationId": d.CorrelationID,
		"timestamp": timestamp(d.Started), "parameters": d.Parameters, "providers": []any{}, "dependencies": []any{}, "outputs": map[string]any{},
	}
	if d.Ended != nil {
		props["timestamp"], props["duration"] = timestamp(*d.Ended), isoDuration(d.Ended.Sub(d.Started))
	}
	var outputs []map[string]string
	for _, op := range d.Operations {
		if op.State == statusSucceeded {
			outputs = append(outputs, map[string]string{"id": op.ResourceID})
		}
	}
	props["outputResources"] = nonNil(outputs)
	if d.Error != nil {
		props["error"] = d.Error
	}
	return map[string]any{"id": s.deploymentID(d.Group, d.Name), "name": d.Name, "type": typeDeployment, "properties": props}
}

func isoDuration(d time.Duration) string {
	return fmt.Sprintf("PT%.7fS", d.Seconds())
}

func (s *Simulation) validateDeployment(r *http.Request, p params) (reply, error) {
	var req deploymentRequest
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	var g group
	var err error
	s.store.Read(func(sub *subscription) {
		found, e := requireGroup(sub, p["group"])
		if err = e; e == nil {
			g = *found
		}
	})
	if err != nil {
		return reply{}, err
	}
	resources, hash, err := s.planDeployment(&g, req)
	if err != nil {
		return reply{}, err
	}
	var validated []map[string]string
	for _, res := range resources {
		validated = append(validated, map[string]string{"id": s.resourceID(g.Name, res.Type, res.Name)})
	}
	return okReply(map[string]any{
		"id": s.deploymentID(g.Name, p["name"]), "name": p["name"], "type": typeDeployment,
		"properties": map[string]any{
			"provisioningState": statusSucceeded, "mode": "Incremental", "templateHash": hash, "timestamp": timestamp(s.now()),
			"correlationId": newUUID(), "parameters": req.Properties.Parameters, "validatedResources": nonNil(validated),
		},
	}), nil
}

func (s *Simulation) putDeployment(r *http.Request, p params) (reply, error) {
	var req deploymentRequest
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	var out map[string]any
	var op *operation
	var d *deployment
	status := http.StatusCreated
	err := s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		resources, hash, err := s.planDeployment(g, req)
		if err != nil {
			return err
		}
		if old := lookup(sub.Deployments, g.Name, p["name"]); old != nil {
			if old.ProvisioningState == "Running" {
				return errConflict("DeploymentActive", "The deployment '%s' is still active.", old.Name)
			}
			sub.Deployments = without(sub.Deployments, g.Name, old.Name)
			status = http.StatusOK
		}
		d = &deployment{
			meta: meta{Group: g.Name, Name: p["name"], Location: g.Location, GUID: newUUID()}, Mode: "Incremental",
			Parameters: req.Properties.Parameters, TemplateHash: hash, ProvisioningState: "Running", Started: s.now().UTC(),
			CorrelationID: newUUID(), Resources: resources,
		}
		sub.Deployments = append(sub.Deployments, d)
		op = s.startOperation(sub, "Microsoft.Resources", g.Location)
		out = s.renderDeployment(d)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	group, name, resources := d.Group, d.Name, d.Resources
	s.background(func(ctx context.Context) { s.deploy(ctx, group, name, op.ID, resources) })
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// deploy creates the planned resources one after the other through the same
// handlers as direct requests, waiting for each long-running operation.
func (s *Simulation) deploy(ctx context.Context, group, name, opID string, resources []plannedResource) {
	var failure error
	for _, res := range resources {
		id := s.resourceID(group, res.Type, res.Name)
		step := deploymentStep{ID: strings.ToUpper(strings.ReplaceAll(newUUID(), "-", "")[:16]), ResourceID: id, Type: res.Type, Name: res.Name, APIVersion: res.APIVersion, State: "Running", Started: s.now().UTC()}
		s.recordStep(group, name, step)
		status, err := s.applyResource(ctx, id, res)
		now := s.now().UTC()
		step.Ended, step.State, step.StatusCode = &now, statusSucceeded, http.StatusText(status)
		if err != nil {
			step.State, step.Error, step.StatusCode = statusFailed, opErrorOf(err), "BadRequest"
			failure = err
		}
		s.recordStep(group, name, step)
		if failure != nil {
			break
		}
	}
	var deployErr error
	if failure != nil {
		deployErr = &armError{http.StatusBadRequest, "DeploymentFailed", "At least one resource deployment operation failed. Please list deployment operations for details. Please see https://aka.ms/arm-deployment-operations for usage details. " + failure.Error()}
	}
	err := s.store.Update(func(sub *subscription) error {
		if d := lookup(sub.Deployments, group, name); d != nil {
			now := s.now().UTC()
			d.Ended, d.ProvisioningState = &now, statusSucceeded
			if deployErr != nil {
				d.ProvisioningState, d.Error = statusFailed, opErrorOf(deployErr)
			}
		}
		s.finishOperation(sub, opID, deployErr)
		return nil
	})
	if err != nil {
		s.log.Error("record a finished deployment", "deployment", name, "error", err)
	}
}

func (s *Simulation) recordStep(group, name string, step deploymentStep) {
	_ = s.store.Update(func(sub *subscription) error {
		d := lookup(sub.Deployments, group, name)
		if d == nil {
			return nil
		}
		if i := slices.IndexFunc(d.Operations, func(o deploymentStep) bool { return o.ID == step.ID }); i >= 0 {
			d.Operations[i] = step
		} else {
			d.Operations = append(d.Operations, step)
		}
		return nil
	})
}

// applyResource PUTs the resource through its route and waits for the
// operation it starts. It returns the PUT's status.
func (s *Simulation) applyResource(ctx context.Context, id string, res plannedResource) (int, error) {
	u := &url.URL{Path: id, RawQuery: url.Values{"api-version": {res.APIVersion}}.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(res.Body))
	if err != nil {
		return 0, err
	}
	segs := splitPath(id)
	for _, rt := range s.routes {
		p, ok := rt.match(segs)
		if !ok || rt.method != http.MethodPut {
			continue
		}
		out, err := rt.handle(req, p)
		if err != nil {
			return 0, err
		}
		if link := out.headers["Azure-AsyncOperation"]; link != "" {
			return out.status, s.waitOperation(ctx, path.Base(strings.SplitN(link, "?", 2)[0]))
		}
		return out.status, nil
	}
	return 0, fmt.Errorf("no route creates %s", id)
}

func (s *Simulation) waitOperation(ctx context.Context, id string) error {
	for {
		var status string
		var opErr *opError
		s.store.Read(func(sub *subscription) {
			for _, op := range sub.Operations {
				if op.ID == id {
					status, opErr = op.Status, op.Error
				}
			}
		})
		switch status {
		case statusSucceeded:
			return nil
		case statusFailed:
			return &armError{http.StatusBadRequest, opErr.Code, opErr.Message}
		case "":
			return errors.New("the operation " + id + " is gone")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Simulation) getDeployment(_ *http.Request, p params) (reply, error) {
	var out map[string]any
	s.store.Read(func(sub *subscription) {
		if d := lookup(sub.Deployments, p["group"], p["name"]); d != nil {
			out = s.renderDeployment(d)
		}
	})
	if out == nil {
		return reply{}, &armError{http.StatusNotFound, "DeploymentNotFound", fmt.Sprintf("Deployment '%s' could not be found.", p["name"])}
	}
	return okReply(out), nil
}

func (s *Simulation) listDeploymentOperations(r *http.Request, p params) (reply, error) {
	var out []map[string]any
	var found bool
	s.store.Read(func(sub *subscription) {
		d := lookup(sub.Deployments, p["group"], p["name"])
		if found = d != nil; !found {
			return
		}
		for _, op := range d.Operations {
			props := map[string]any{
				"provisioningOperation": "Create", "provisioningState": op.State, "timestamp": timestamp(op.Started), "trackingId": newUUID(),
				"statusCode": op.StatusCode, "targetResource": map[string]string{"id": op.ResourceID, "resourceType": op.Type, "resourceName": op.Name, "apiVersion": op.APIVersion},
			}
			if op.Ended != nil {
				props["duration"] = isoDuration(op.Ended.Sub(op.Started))
			}
			if op.Error != nil {
				props["statusMessage"] = map[string]any{"status": "Failed", "error": op.Error}
			}
			out = append(out, map[string]any{"id": s.deploymentID(d.Group, d.Name) + "/operations/" + op.ID, "operationId": op.ID, "properties": props})
		}
	})
	if !found {
		return reply{}, &armError{http.StatusNotFound, "DeploymentNotFound", fmt.Sprintf("Deployment '%s' could not be found.", p["name"])}
	}
	return listReply(r, out)
}

// deleteDeployment removes the deployment's record. Its resources stay.
func (s *Simulation) deleteDeployment(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		d := lookup(sub.Deployments, p["group"], p["name"])
		if d == nil {
			return nil
		}
		if d.ProvisioningState == "Running" {
			return errConflict("DeploymentActive", "The deployment '%s' is still active.", d.Name)
		}
		sub.Deployments = without(sub.Deployments, d.Group, d.Name)
		op = s.doneOperation(sub, "Microsoft.Resources", d.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}
