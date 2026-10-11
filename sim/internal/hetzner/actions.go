package hetzner

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type actionJSON struct {
	ID        int64         `json:"id"`
	Command   string        `json:"command"`
	Status    string        `json:"status"`
	Progress  int           `json:"progress"`
	Started   string        `json:"started"`
	Finished  *string       `json:"finished"`
	Resources []resourceRef `json:"resources"`
	Error     *actionError  `json:"error"`
}

func (a *action) render() actionJSON {
	out := actionJSON{
		ID: a.ID, Command: a.Command, Status: a.Status, Progress: a.Progress, Started: timestamp(a.Started),
		Resources: nonNil(a.Resources), Error: a.Error,
	}
	if a.Finished != nil {
		done := timestamp(*a.Finished)
		out.Finished = &done
	}
	return out
}

// startAction records a running action on resources; the first resource is
// the one the action belongs to.
func (s *Simulation) startAction(p *project, command string, resources ...resourceRef) *action {
	a := &action{ID: p.nextID(), Command: command, Status: "running", Started: s.now().UTC(), Resources: resources}
	p.Actions = append(p.Actions, a)
	return a
}

// succeeded records an action that finished as soon as it started.
func (s *Simulation) succeeded(p *project, command string, resources ...resourceRef) *action {
	a := s.startAction(p, command, resources...)
	s.finish(a, nil)
	return a
}

// finish ends a running action: success without an error, or error with
// the error's code and message.
func (s *Simulation) finish(a *action, failure *actionError) {
	now := s.now().UTC()
	a.Finished, a.Progress = &now, 100
	a.Status = "success"
	if failure != nil {
		a.Status, a.Error = "error", failure
	}
}

// finishAction ends the action with the id, and runs update on the project
// in the same change.
func (s *Simulation) finishAction(actionID int64, failure *actionError, update func(p *project)) {
	err := s.store.Update(func(p *project) error {
		if a := p.action(actionID); a != nil && a.Status == "running" {
			s.finish(a, failure)
		}
		if update != nil {
			update(p)
		}
		return nil
	})
	if err != nil {
		s.log.Error("record action result", "action", actionID, "error", err)
	}
	s.firewalls.kick()
}

// running reports whether a server action, one that belongs to the
// resource, is running. Hetzner locks a resource while one runs.
func (p *project) running(ref resourceRef) bool {
	return slices.ContainsFunc(p.Actions, func(a *action) bool {
		return a.Status == "running" && len(a.Resources) > 0 && a.Resources[0] == ref
	})
}

func (a *action) concerns(ref resourceRef) bool {
	return slices.Contains(a.Resources, ref)
}

var actionSortFields = sortFields[actionJSON]{
	"id":       byID(func(a actionJSON) int64 { return a.ID }),
	"command":  byString(func(a actionJSON) string { return a.Command }),
	"status":   byString(func(a actionJSON) string { return a.Status }),
	"started":  byString(func(a actionJSON) string { return a.Started }),
	"finished": byString(func(a actionJSON) string { return deref(a.Finished) }),
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// actionsWhere returns the actions that keep selects and that match the
// request's id and status filters, oldest first.
func (s *Simulation) actionsWhere(r *http.Request, keep func(*action) bool) ([]actionJSON, error) {
	q := r.URL.Query()
	var ids []int64
	for _, v := range q["id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			return nil, errInvalidInput("id", "%q is not an action id", v)
		}
		ids = append(ids, id)
	}
	for _, st := range q["status"] {
		if st != "running" && st != "success" && st != "error" {
			return nil, errInvalidInput("status", "%q is not running, success or error", st)
		}
	}
	var out []actionJSON
	s.store.Read(func(p *project) {
		for _, a := range p.Actions {
			if keep(a) && (len(ids) == 0 || slices.Contains(ids, a.ID)) && (len(q["status"]) == 0 || slices.Contains(q["status"], a.Status)) {
				out = append(out, a.render())
			}
		}
	})
	return out, nil
}

// listActions answers GET /actions, which Hetzner serves only for the
// actions that its id parameters name.
func (s *Simulation) listActions(r *http.Request) (int, any, error) {
	if len(r.URL.Query()["id"]) == 0 {
		return 0, nil, errInvalidInput("id", "is required")
	}
	out, err := s.actionsWhere(r, func(*action) bool { return true })
	if err != nil {
		return 0, nil, err
	}
	return listResponse(r, "actions", out, actionSortFields)
}

func (s *Simulation) getAction(r *http.Request) (int, any, error) {
	return s.actionByID(r, r.PathValue("id"), func(*action) bool { return true })
}

func (s *Simulation) actionByID(_ *http.Request, idText string, keep func(*action) bool) (int, any, error) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return 0, nil, errNotFound("action")
	}
	var out *actionJSON
	s.store.Read(func(p *project) {
		if a := p.action(id); a != nil && keep(a) {
			j := a.render()
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("action")
	}
	return http.StatusOK, map[string]any{"action": out}, nil
}

// resourceKinds maps a collection in the API path to the resource type its
// actions name.
var resourceKinds = map[string]string{"servers": "server", "primary_ips": "primary_ip", "firewalls": "firewall", "networks": "network"}

// addActionRoutes adds the action endpoints of a resource collection:
// /<kind>/actions, /<kind>/actions/{id}, /<kind>/{id}/actions and
// /<kind>/{id}/actions/{action_id}. The second and third share one path
// shape, so one route serves both.
func (s *Simulation) addActionRoutes(routes map[string]endpoint, kind string) {
	typ := resourceKinds[kind]
	ofKind := func(a *action) bool { return len(a.Resources) > 0 && a.Resources[0].Type == typ }
	routes["GET /v1/"+kind+"/actions"] = func(r *http.Request) (int, any, error) {
		out, err := s.actionsWhere(r, ofKind)
		if err != nil {
			return 0, nil, err
		}
		return listResponse(r, "actions", out, actionSortFields)
	}
	routes["GET /v1/"+kind+"/{first}/{second}"] = func(r *http.Request) (int, any, error) {
		first, second := r.PathValue("first"), r.PathValue("second")
		if first == "actions" {
			return s.actionByID(r, second, ofKind)
		}
		if second != "actions" {
			return 0, nil, errNotFound("resource")
		}
		ref, err := s.existingResource(typ, first)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.actionsWhere(r, func(a *action) bool { return a.concerns(ref) })
		if err != nil {
			return 0, nil, err
		}
		return listResponse(r, "actions", out, actionSortFields)
	}
	routes["GET /v1/"+kind+"/{id}/actions/{action}"] = func(r *http.Request) (int, any, error) {
		ref, err := s.existingResource(typ, r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		return s.actionByID(r, r.PathValue("action"), func(a *action) bool { return a.concerns(ref) })
	}
}

// existingResource returns the reference of a resource that exists.
func (s *Simulation) existingResource(typ, idText string) (resourceRef, error) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return resourceRef{}, errNotFound(strings.ReplaceAll(typ, "_", " "))
	}
	exists := false
	s.store.Read(func(p *project) {
		switch typ {
		case "server":
			exists = p.server(id) != nil
		case "primary_ip":
			exists = p.primaryIP(id) != nil
		case "firewall":
			exists = p.firewall(id) != nil
		case "network":
			exists = p.network(id) != nil
		}
	})
	if !exists {
		return resourceRef{}, errNotFound(strings.ReplaceAll(typ, "_", " "))
	}
	return resourceRef{ID: id, Type: typ}, nil
}
