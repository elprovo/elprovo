package gcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"time"
)

const (
	maxOperations = 1000
	waitLimit     = 2 * time.Minute
	waitPoll      = 100 * time.Millisecond
)

// operation is a Compute Engine operation: zonal, regional or global. It is
// RUNNING while the simulation does its work and DONE afterwards, with an
// error when the work failed.
type operation struct {
	ID         uint64     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	Location   string     `json:"location"`
	Type       string     `json:"type"`
	TargetLink string     `json:"target_link"`
	TargetID   uint64     `json:"target_id"`
	Status     string     `json:"status"`
	InsertTime time.Time  `json:"insert_time"`
	EndTime    *time.Time `json:"end_time,omitempty"`
	Error      *opError   `json:"error,omitempty"`
}

type opError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status"`
}

func operationName(now time.Time) string {
	b := make([]byte, 15)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return fmt.Sprintf("operation-%d-%s-%s-%s", now.UnixMilli(), h[:13], h[13:21], h[21:29])
}

// newOperation records a RUNNING operation on a target in the world.
func (s *Simulation) newOperation(w *world, scope, location, typ, targetLink string, targetID uint64) *operation {
	op := &operation{
		ID: w.nextID(), Name: operationName(s.now()), Scope: scope, Location: location, Type: typ,
		TargetLink: targetLink, TargetID: targetID, Status: "RUNNING", InsertTime: s.now(),
	}
	w.Operations = append(w.Operations, op)
	if extra := len(w.Operations) - maxOperations; extra > 0 {
		dropped := 0
		w.Operations = slices.DeleteFunc(w.Operations, func(o *operation) bool {
			if dropped < extra && o.Status == "DONE" {
				dropped++
				return true
			}
			return false
		})
	}
	return op
}

func (s *Simulation) done(op *operation, err *opError) {
	now := s.now()
	op.Status, op.EndTime, op.Error = "DONE", &now, err
}

// finishOperation marks the operation DONE, with err when its work failed,
// and applies update to the world in the same change.
func (s *Simulation) finishOperation(id uint64, err *opError, update func(w *world)) {
	saveErr := s.store.Update(func(w *world) error {
		if op := find(w.Operations, func(o *operation) bool { return o.ID == id }); op != nil {
			s.done(op, err)
		}
		if update != nil {
			update(w)
		}
		return nil
	})
	if saveErr != nil {
		s.log.Error("record operation result", "operation", id, "error", saveErr)
	}
}

type operationJSON struct {
	Kind                string          `json:"kind"`
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Zone                string          `json:"zone,omitempty"`
	Region              string          `json:"region,omitempty"`
	OperationType       string          `json:"operationType"`
	TargetLink          string          `json:"targetLink"`
	TargetID            string          `json:"targetId"`
	Status              string          `json:"status"`
	User                string          `json:"user"`
	Progress            int             `json:"progress"`
	InsertTime          string          `json:"insertTime"`
	StartTime           string          `json:"startTime"`
	EndTime             string          `json:"endTime,omitempty"`
	SelfLink            string          `json:"selfLink"`
	Error               *operationError `json:"error,omitempty"`
	HTTPErrorStatusCode int             `json:"httpErrorStatusCode,omitempty"`
	HTTPErrorMessage    string          `json:"httpErrorMessage,omitempty"`
}

type operationError struct {
	Errors []operationErrorItem `json:"errors"`
}

type operationErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Simulation) renderOperation(w *world, op *operation) operationJSON {
	out := operationJSON{
		Kind: "compute#operation", ID: idString(op.ID), Name: op.Name, OperationType: op.Type, TargetLink: op.TargetLink,
		TargetID: idString(op.TargetID), Status: op.Status, User: w.Keys[0].Email,
		InsertTime: timestamp(op.InsertTime), StartTime: timestamp(op.InsertTime),
	}
	switch op.Scope {
	case "zone":
		out.Zone = s.link("zones", op.Location)
		out.SelfLink = s.link("zones", op.Location, "operations", op.Name)
	case "region":
		out.Region = s.link("regions", op.Location)
		out.SelfLink = s.link("regions", op.Location, "operations", op.Name)
	default:
		out.SelfLink = s.link("global", "operations", op.Name)
	}
	if op.EndTime != nil {
		out.Progress, out.EndTime = 100, timestamp(*op.EndTime)
	}
	if op.Error != nil {
		out.Error = &operationError{Errors: []operationErrorItem{{Code: op.Error.Code, Message: op.Error.Message}}}
		out.HTTPErrorStatusCode, out.HTTPErrorMessage = op.Error.HTTPStatus, http.StatusText(op.Error.HTTPStatus)
	}
	return out
}

// operationIn returns the rendered operation from the request path. scope is
// zone, region or global.
func (s *Simulation) operationIn(r *http.Request, scope string) (operationJSON, error) {
	location := ""
	var err error
	switch scope {
	case "zone":
		location, err = s.zone(r)
	case "region":
		location, err = s.region(r)
	default:
		err = s.checkProject(r)
	}
	if err != nil {
		return operationJSON{}, err
	}
	var out *operationJSON
	s.store.Read(func(w *world) {
		if op := w.operation(scope, location, r.PathValue("name")); op != nil {
			rendered := s.renderOperation(w, op)
			out = &rendered
		}
	})
	if out == nil {
		return operationJSON{}, errNotFound(r.URL.Path[len("/compute/v1/"):])
	}
	return *out, nil
}

func (s *Simulation) getOperation(scope string) endpoint {
	return func(r *http.Request) (int, any, error) {
		op, err := s.operationIn(r, scope)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, op, nil
	}
}

// waitOperation answers when the operation is DONE or after two minutes,
// whichever comes first, as the wait method does. It also answers when the
// simulation stops.
func (s *Simulation) waitOperation(scope string) endpoint {
	return func(r *http.Request) (int, any, error) {
		deadline := time.NewTimer(waitLimit)
		defer deadline.Stop()
		tick := time.NewTicker(waitPoll)
		defer tick.Stop()
		for {
			op, err := s.operationIn(r, scope)
			if err != nil || op.Status == "DONE" {
				return http.StatusOK, op, err
			}
			select {
			case <-r.Context().Done():
				return 0, nil, r.Context().Err()
			case <-s.bg.Done():
				return http.StatusOK, op, nil
			case <-deadline.C:
				return http.StatusOK, op, nil
			case <-tick.C:
			}
		}
	}
}
