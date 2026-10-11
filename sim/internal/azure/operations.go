package azure

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	statusInProgress = "InProgress"
	statusSucceeded  = "Succeeded"
	statusFailed     = "Failed"
	// operationRetention is how long a finished operation stays readable.
	operationRetention = time.Hour
)

// operation is a long-running operation. Clients poll it through the
// Azure-AsyncOperation and Location links of the response that started it.
type operation struct {
	ID        string     `json:"id"`
	Namespace string     `json:"namespace"`
	Location  string     `json:"location"`
	Status    string     `json:"status"`
	Error     *opError   `json:"error,omitempty"`
	Started   time.Time  `json:"started"`
	Ended     *time.Time `json:"ended,omitempty"`
}

type opError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func opErrorOf(err error) *opError {
	var e *armError
	if errors.As(err, &e) {
		return &opError{Code: e.code, Message: e.message}
	}
	return &opError{Code: "InternalServerError", Message: err.Error()}
}

// startOperation records a running operation of a resource provider, such
// as Microsoft.Compute, in a location.
func (s *Simulation) startOperation(sub *subscription, namespace, location string) *operation {
	now := s.now().UTC()
	sub.Operations = slices.DeleteFunc(sub.Operations, func(op *operation) bool {
		return op.Ended != nil && now.Sub(*op.Ended) > operationRetention
	})
	op := &operation{ID: newUUID(), Namespace: namespace, Location: location, Status: statusInProgress, Started: now}
	sub.Operations = append(sub.Operations, op)
	return op
}

// doneOperation records an operation that finished when it started, for a
// change the simulation makes at once.
func (s *Simulation) doneOperation(sub *subscription, namespace, location string) *operation {
	op := s.startOperation(sub, namespace, location)
	op.Status, op.Ended = statusSucceeded, &op.Started
	return op
}

// finishOperation ends the operation; a nil err means it succeeded.
func (s *Simulation) finishOperation(sub *subscription, id string, err error) {
	for _, op := range sub.Operations {
		if op.ID != id {
			continue
		}
		now := s.now().UTC()
		op.Ended = &now
		op.Status = statusSucceeded
		if err != nil {
			op.Status, op.Error = statusFailed, opErrorOf(err)
		}
	}
}

func (s *Simulation) operationURL(r *http.Request, op *operation, kind string) string {
	return baseURL(r) + "/subscriptions/" + s.cfg.subscription + "/providers/" + op.Namespace + "/locations/" + op.Location + "/" + kind + "/" + op.ID +
		"?api-version=" + r.URL.Query().Get("api-version")
}

// asyncHeaders links a response to its operation: Azure-AsyncOperation for
// the status, and for deletes and actions, Location for the result.
func (s *Simulation) asyncHeaders(r *http.Request, op *operation) map[string]string {
	h := map[string]string{"Azure-AsyncOperation": s.operationURL(r, op, "operations"), "Retry-After": retryAfter}
	if r.Method == http.MethodDelete || r.Method == http.MethodPost {
		h["Location"] = s.operationURL(r, op, "operationResults")
	}
	return h
}

// accepted answers a delete or an action that runs as op.
func (s *Simulation) accepted(r *http.Request, op *operation) reply {
	return reply{status: http.StatusAccepted, headers: s.asyncHeaders(r, op)}
}

func (s *Simulation) findOperation(p params) *operation {
	var found *operation
	s.store.Read(func(sub *subscription) {
		for _, op := range sub.Operations {
			if op.ID == p["id"] && strings.EqualFold(op.Namespace, p["namespace"]) {
				c := *op
				found = &c
			}
		}
	})
	return found
}

func (s *Simulation) getOperationStatus(_ *http.Request, p params) (reply, error) {
	op := s.findOperation(p)
	if op == nil {
		return reply{}, &armError{http.StatusNotFound, "OperationNotFound", "The operation '" + p["id"] + "' could not be found."}
	}
	body := map[string]any{"name": op.ID, "status": op.Status, "startTime": timestamp(op.Started)}
	if op.Ended != nil {
		body["endTime"] = timestamp(*op.Ended)
	}
	if op.Error != nil {
		body["error"] = op.Error
	}
	out := okReply(body)
	if op.Status == statusInProgress {
		out.headers = map[string]string{"Retry-After": retryAfter}
	}
	return out, nil
}

func (s *Simulation) getOperationResult(r *http.Request, p params) (reply, error) {
	op := s.findOperation(p)
	switch {
	case op == nil:
		return reply{}, &armError{http.StatusNotFound, "OperationNotFound", "The operation '" + p["id"] + "' could not be found."}
	case op.Status == statusInProgress:
		return reply{status: http.StatusAccepted, headers: map[string]string{"Location": baseURL(r) + r.URL.RequestURI(), "Retry-After": retryAfter}}, nil
	case op.Error != nil:
		return reply{status: http.StatusBadRequest, body: map[string]any{"error": op.Error}}, nil
	default:
		return reply{status: http.StatusNoContent}, nil
	}
}
