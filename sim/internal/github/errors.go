package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const docsURL = "https://docs.github.com/rest"

type fieldError struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message,omitempty"`
}

type apiError struct {
	status  int
	message string
	errors  []fieldError
}

func (e *apiError) Error() string { return e.message }

func fail(status int, format string, args ...any) error {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

func errNotFound() error { return fail(http.StatusNotFound, "Not Found") }

func errValidation(resource, field, code string) error {
	return &apiError{
		status:  http.StatusUnprocessableEntity,
		message: "Validation Failed",
		errors:  []fieldError{{Resource: resource, Field: field, Code: code}},
	}
}

func errDenied(c caller) error {
	switch {
	case c.kind == callerAnonymous:
		return fail(http.StatusUnauthorized, "Requires authentication")
	case c.isIntegration():
		return fail(http.StatusForbidden, "Resource not accessible by integration")
	default:
		return errNotFound()
	}
}

type errorBody struct {
	Message          string       `json:"message"`
	Errors           []fieldError `json:"errors,omitempty"`
	DocumentationURL string       `json:"documentation_url"`
	Status           string       `json:"status"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Sim) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		e = &apiError{status: http.StatusInternalServerError, message: "Server Error"}
	}
	writeJSON(w, e.status, errorBody{
		Message:          e.message,
		Errors:           e.errors,
		DocumentationURL: docsURL,
		Status:           strconv.Itoa(e.status),
	})
}

func decodeJSON(r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		return fail(http.StatusBadRequest, "Problems parsing JSON")
	}
	return nil
}
