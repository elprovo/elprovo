package azure

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxRequestBody = 4 << 20
	listPageSize   = 100
	// retryAfter is the Retry-After the simulation sends with long-running
	// operations, in seconds. Clients wait that long between polls.
	retryAfter = "1"
)

var apiVersionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(-preview|-beta|-alpha)?$`)

// reply is a Resource Manager response.
type reply struct {
	status  int
	body    any
	headers map[string]string
}

type params map[string]string

type handlerFunc func(r *http.Request, p params) (reply, error)

type route struct {
	method   string
	segments []string
	handle   handlerFunc
}

// armError is a Resource Manager error response.
type armError struct {
	status  int
	code    string
	message string
}

func (e *armError) Error() string { return e.code + ": " + e.message }

func errBadRequest(code, format string, args ...any) *armError {
	return &armError{http.StatusBadRequest, code, fmt.Sprintf(format, args...)}
}

func errInvalidParameter(format string, args ...any) *armError {
	return errBadRequest("InvalidParameter", format, args...)
}

func errUnsupported(format string, args ...any) *armError {
	return errBadRequest("UnsupportedBySimulation", format+" is not supported by the simulation", args...)
}

func errConflict(code, format string, args ...any) *armError {
	return &armError{http.StatusConflict, code, fmt.Sprintf(format, args...)}
}

func errGroupNotFound(name string) *armError {
	return &armError{http.StatusNotFound, "ResourceGroupNotFound", fmt.Sprintf("Resource group '%s' could not be found.", name)}
}

func errNotFound(typ, name, group string) *armError {
	return &armError{http.StatusNotFound, "ResourceNotFound", fmt.Sprintf("The Resource '%s/%s' under resource group '%s' was not found. For more details please go to https://aka.ms/ARMResourceNotFoundFix", typ, name, group)}
}

// pattern splits a route such as
// subscriptions/{sub}/resourceGroups/{group} into its segments.
func pattern(method, path string, h handlerFunc) route {
	return route{method: method, segments: strings.Split(path, "/"), handle: h}
}

// match reports whether segs fit the route; literal segments compare
// without case, as Resource Manager does.
func (rt route) match(segs []string) (params, bool) {
	if len(segs) != len(rt.segments) {
		return nil, false
	}
	p := params{}
	for i, want := range rt.segments {
		if name, ok := strings.CutPrefix(want, "{"); ok {
			if segs[i] == "" {
				return nil, false
			}
			p[strings.TrimSuffix(name, "}")] = segs[i]
			continue
		}
		if !strings.EqualFold(want, segs[i]) {
			return nil, false
		}
	}
	return p, true
}

func splitPath(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s == "" {
			continue
		}
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		segs = append(segs, s)
	}
	return segs
}

func (s *Simulation) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newUUID()
		w.Header().Set("x-ms-request-id", requestID)
		w.Header().Set("x-ms-correlation-request-id", requestID)
		segs := splitPath(r.URL.Path)
		switch {
		case len(segs) == 4 && strings.EqualFold(segs[1], "oauth2") && segs[2] == "v2.0" && segs[3] == "token":
			s.serveToken(w, r, segs[0])
		case len(segs) == 2 && segs[0] == "metadata" && segs[1] == "endpoints":
			s.serveCloudMetadata(w, r)
		default:
			s.serveARM(w, r, segs)
		}
	})
}

func (s *Simulation) serveARM(w http.ResponseWriter, r *http.Request, segs []string) {
	if err := s.authenticate(w, r); err != nil {
		writeReply(w, errorReply(err))
		return
	}
	if err := checkAPIVersion(r); err != nil {
		writeReply(w, errorReply(err))
		return
	}
	if len(segs) >= 2 && strings.EqualFold(segs[0], "subscriptions") && !strings.EqualFold(segs[1], s.cfg.subscription) {
		writeReply(w, errorReply(&armError{http.StatusNotFound, "SubscriptionNotFound", fmt.Sprintf("The subscription '%s' could not be found.", segs[1])}))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	for _, rt := range s.routes {
		if rt.method != r.Method {
			continue
		}
		if p, ok := rt.match(segs); ok {
			out, err := rt.handle(r, p)
			if err != nil {
				out = s.failure(r, err)
			}
			writeReply(w, out)
			return
		}
	}
	s.log.Warn("request the simulation does not support", "method", r.Method, "path", r.URL.Path)
	writeReply(w, errorReply(errBadRequest("InvalidResourceType", "The simulation does not support %s %s.", r.Method, r.URL.Path)))
}

func (s *Simulation) failure(r *http.Request, err error) reply {
	var apiErr *armError
	if !errors.As(err, &apiErr) {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		apiErr = &armError{http.StatusInternalServerError, "InternalServerError", "An internal error occurred in the simulation."}
	}
	return errorReply(apiErr)
}

func errorReply(err *armError) reply {
	return reply{status: err.status, body: errorBody(err)}
}

func errorBody(err *armError) map[string]any {
	return map[string]any{"error": map[string]string{"code": err.code, "message": err.message}}
}

func writeReply(w http.ResponseWriter, out reply) {
	for k, v := range out.headers {
		w.Header().Set(k, v)
	}
	if out.body == nil {
		w.WriteHeader(out.status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(out.status)
	_ = json.NewEncoder(w).Encode(out.body)
}

func checkAPIVersion(r *http.Request) *armError {
	v := r.URL.Query().Get("api-version")
	if v == "" {
		return errBadRequest("MissingApiVersionParameter", "The api-version query parameter (?api-version=) is required for all requests.")
	}
	if !apiVersionPattern.MatchString(v) {
		return errBadRequest("InvalidApiVersionParameter", "The api-version '%s' is invalid. The supported versions are 'yyyy-mm-dd' dates.", v)
	}
	return nil
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errBadRequest("InvalidRequestContent", "The request content was invalid and could not be deserialized: %v", err)
	}
	return nil
}

// baseURL is the scheme and host the client reached the simulation at, which
// links in responses point back to.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// listReply answers a list with Resource Manager paging: value, and nextLink
// while more items follow.
func listReply[T any](r *http.Request, items []T) (reply, error) {
	start := 0
	if v := r.URL.Query().Get("$skiptoken"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return reply{}, errBadRequest("InvalidSkipToken", "The $skiptoken '%s' is invalid.", v)
		}
		start = min(n, len(items))
	}
	end := min(start+listPageSize, len(items))
	page := items[start:end]
	if page == nil {
		page = []T{}
	}
	body := map[string]any{"value": page}
	if end < len(items) {
		q := r.URL.Query()
		q.Set("$skiptoken", strconv.Itoa(end))
		body["nextLink"] = baseURL(r) + r.URL.Path + "?" + q.Encode()
	}
	return reply{status: http.StatusOK, body: body}, nil
}

func okReply(body any) reply { return reply{status: http.StatusOK, body: body} }
