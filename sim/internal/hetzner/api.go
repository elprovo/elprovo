package hetzner

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestBody = 1 << 20
	defaultPerPage = 25
	maxPerPage     = 50
)

type endpoint func(r *http.Request) (int, any, error)

// apiError is an error answer: an HTTP status, Hetzner's error code, a
// message and optional details.
type apiError struct {
	status  int
	code    string
	message string
	details any
}

func (e *apiError) Error() string { return e.message }

type fieldError struct {
	Name     string   `json:"name"`
	Messages []string `json:"messages,omitempty"`
}

func errNotFound(what string) *apiError {
	return &apiError{status: http.StatusNotFound, code: "not_found", message: what + " not found"}
}

// errInvalidInput reports one invalid request field, as Hetzner's
// invalid_input error does, with the field in its details.
func errInvalidInput(field, format string, args ...any) *apiError {
	msg := fmt.Sprintf(format, args...)
	return &apiError{
		status: http.StatusUnprocessableEntity, code: "invalid_input",
		message: fmt.Sprintf("invalid input in field '%s': %s", field, msg),
		details: map[string]any{"fields": []fieldError{{Name: field, Messages: []string{msg}}}},
	}
}

// errUnsupported reports a request that Hetzner would accept but the
// simulation does not support.
func errUnsupported(format string, args ...any) *apiError {
	return &apiError{status: http.StatusUnprocessableEntity, code: "unsupported_error", message: fmt.Sprintf(format, args...)}
}

func errUniqueness(field, message string) *apiError {
	return &apiError{
		status: http.StatusConflict, code: "uniqueness_error", message: message,
		details: map[string]any{"fields": []fieldError{{Name: field}}},
	}
}

func errLocked(what string) *apiError {
	return &apiError{status: http.StatusLocked, code: "locked", message: what + " is locked"}
}

func errResourceLimit(limit, message string) *apiError {
	return &apiError{
		status: http.StatusForbidden, code: "resource_limit_exceeded", message: message,
		details: map[string]any{"limits": []map[string]string{{"name": limit}}},
	}
}

func errService(format string, args ...any) *apiError {
	return &apiError{status: http.StatusUnprocessableEntity, code: "service_error", message: fmt.Sprintf(format, args...)}
}

func errCode(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

func (s *Simulation) handler() http.Handler {
	routes := map[string]endpoint{
		"GET /v1/locations":         s.listLocations,
		"GET /v1/locations/{id}":    s.getLocation,
		"GET /v1/server_types":      s.listServerTypes,
		"GET /v1/server_types/{id}": s.getServerType,

		"GET /v1/images":         s.listImages,
		"GET /v1/images/{id}":    s.getImage,
		"PUT /v1/images/{id}":    s.updateImage,
		"DELETE /v1/images/{id}": s.deleteImage,

		"GET /v1/ssh_keys":         s.listSSHKeys,
		"POST /v1/ssh_keys":        s.createSSHKey,
		"GET /v1/ssh_keys/{id}":    s.getSSHKey,
		"PUT /v1/ssh_keys/{id}":    s.updateSSHKey,
		"DELETE /v1/ssh_keys/{id}": s.deleteSSHKey,

		"GET /v1/servers":                        s.listServers,
		"POST /v1/servers":                       s.createServer,
		"GET /v1/servers/{id}":                   s.getServer,
		"PUT /v1/servers/{id}":                   s.updateServer,
		"DELETE /v1/servers/{id}":                s.deleteServer,
		"POST /v1/servers/{id}/actions/{action}": s.postServerAction,

		"GET /v1/primary_ips":                        s.listPrimaryIPs,
		"POST /v1/primary_ips":                       s.createPrimaryIP,
		"GET /v1/primary_ips/{id}":                   s.getPrimaryIP,
		"PUT /v1/primary_ips/{id}":                   s.updatePrimaryIP,
		"DELETE /v1/primary_ips/{id}":                s.deletePrimaryIP,
		"POST /v1/primary_ips/{id}/actions/{action}": s.postPrimaryIPAction,

		"GET /v1/firewalls":                        s.listFirewalls,
		"POST /v1/firewalls":                       s.createFirewall,
		"GET /v1/firewalls/{id}":                   s.getFirewall,
		"PUT /v1/firewalls/{id}":                   s.updateFirewall,
		"DELETE /v1/firewalls/{id}":                s.deleteFirewall,
		"POST /v1/firewalls/{id}/actions/{action}": s.postFirewallAction,

		"GET /v1/networks":                        s.listNetworks,
		"POST /v1/networks":                       s.createNetwork,
		"GET /v1/networks/{id}":                   s.getNetwork,
		"PUT /v1/networks/{id}":                   s.updateNetwork,
		"DELETE /v1/networks/{id}":                s.deleteNetwork,
		"POST /v1/networks/{id}/actions/{action}": s.postNetworkAction,

		"GET /v1/actions":      s.listActions,
		"GET /v1/actions/{id}": s.getAction,
	}
	for _, kind := range []string{"servers", "primary_ips", "firewalls", "networks"} {
		s.addActionRoutes(routes, kind)
	}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.Handle(pattern, s.serve(h))
	}
	mux.Handle("/", s.serve(unknownRoute(mux)))
	return s.authenticate(mux)
}

// unknownRoute answers a path that no route serves with 404, or with 405
// when a route serves the path with another method.
func unknownRoute(mux *http.ServeMux) endpoint {
	return func(r *http.Request) (int, any, error) {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			probe := r.Clone(r.Context())
			probe.Method = method
			if _, pattern := mux.Handler(probe); method != r.Method && pattern != "/" {
				return 0, nil, errCode(http.StatusMethodNotAllowed, "method_not_allowed", "method %s is not allowed on %s", r.Method, r.URL.Path)
			}
		}
		return 0, nil, errNotFound("resource")
	}
}

func (s *Simulation) serve(h endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body, err := h(r)
		if err != nil {
			var apiErr *apiError
			if !errors.As(err, &apiErr) {
				s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
				apiErr = errCode(http.StatusInternalServerError, "server_error", "error within the API backend")
			}
			status = apiErr.status
			body = map[string]any{"error": map[string]any{"code": apiErr.code, "message": apiErr.message, "details": apiErr.details}}
		}
		if page, ok := body.(listPage); ok && page.link != "" {
			w.Header().Set("Link", page.link)
		}
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

func (s *Simulation) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Correlation-Id", newUUID())
		usage := s.limiter.take(s.now())
		h := w.Header()
		h.Set("RateLimit-Limit", strconv.Itoa(usage.limit))
		h.Set("RateLimit-Remaining", strconv.Itoa(usage.remaining))
		h.Set("RateLimit-Reset", strconv.FormatInt(usage.reset.Unix(), 10))
		if !usage.allowed {
			s.serve(func(*http.Request) (int, any, error) {
				return 0, nil, errCode(http.StatusTooManyRequests, "rate_limit_exceeded", "limit of %d requests per hour reached", usage.limit)
			}).ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !slices.Contains(s.cfg.tokens, token) {
			s.serve(func(*http.Request) (int, any, error) {
				return 0, nil, errCode(http.StatusUnauthorized, "unauthorized", "unable to authenticate")
			}).ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errCode(http.StatusBadRequest, "json_error", "invalid JSON input in your request: %v", err)
	}
	return nil
}

// pathID returns the numeric id in the path wildcard name; a value that is
// not a positive integer finds nothing.
func pathID(r *http.Request, name, what string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0, errNotFound(what)
	}
	return id, nil
}

// idOrName reads a request value that may be an ID (a JSON number) or a
// name (a JSON string).
func idOrName(raw json.RawMessage) (int64, string) {
	var id int64
	if json.Unmarshal(raw, &id) == nil {
		return id, ""
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		if n, err := strconv.ParseInt(name, 10, 64); err == nil {
			return n, ""
		}
		return 0, name
	}
	return 0, ""
}

type paginationJSON struct {
	Page         int  `json:"page"`
	PerPage      int  `json:"per_page"`
	PreviousPage *int `json:"previous_page"`
	NextPage     *int `json:"next_page"`
	LastPage     int  `json:"last_page"`
	TotalEntries int  `json:"total_entries"`
}

// listPage is one page of a list answer and its Link header.
type listPage struct {
	body map[string]any
	link string
}

func (p listPage) MarshalJSON() ([]byte, error) { return json.Marshal(p.body) }

// listResponse sorts the items as the request's sort parameters say, using
// the named fields, and answers one page of them under key, with
// meta.pagination and a Link header.
func listResponse[T any](r *http.Request, key string, items []T, fields sortFields[T]) (int, any, error) {
	if err := sortItems(r, items, fields); err != nil {
		return 0, nil, err
	}
	q := r.URL.Query()
	page, perPage := 1, defaultPerPage
	var err error
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			return 0, nil, errInvalidInput("page", "must be a positive integer")
		}
	}
	if v := q.Get("per_page"); v != "" {
		if perPage, err = strconv.Atoi(v); err != nil || perPage < 1 {
			return 0, nil, errInvalidInput("per_page", "must be a positive integer")
		}
		perPage = min(perPage, maxPerPage)
	}
	total := len(items)
	last := max(1, (total+perPage-1)/perPage)
	start := min((page-1)*perPage, total)
	out := items[start:min(start+perPage, total)]
	if out == nil {
		out = []T{}
	}
	meta := paginationJSON{Page: page, PerPage: perPage, LastPage: last, TotalEntries: total}
	var links []string
	if page > 1 {
		prev := min(page-1, last)
		meta.PreviousPage = &prev
		links = append(links, pageLink(r, prev, perPage, "prev"))
	}
	if page < last {
		next := page + 1
		meta.NextPage = &next
		links = append(links, pageLink(r, next, perPage, "next"))
	}
	if last > 1 {
		links = append(links, pageLink(r, last, perPage, "last"))
	}
	body := map[string]any{key: out, "meta": map[string]any{"pagination": meta}}
	return http.StatusOK, listPage{body: body, link: strings.Join(links, ", ")}, nil
}

func pageLink(r *http.Request, page, perPage int, rel string) string {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(perPage))
	u := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
	return "<" + u.String() + `>; rel="` + rel + `"`
}

// sortFields compare two items by one field each.
type sortFields[T any] map[string]func(a, b T) int

// sortItems sorts items stably by each sort parameter in turn, such as
// sort=status or sort=id:desc. Without sort parameters, items keep their
// order, which is by id.
func sortItems[T any](r *http.Request, items []T, fields sortFields[T]) error {
	params := r.URL.Query()["sort"]
	if len(params) == 0 {
		return nil
	}
	var compares []func(a, b T) int
	for _, p := range params {
		field, dir, _ := strings.Cut(p, ":")
		compare, ok := fields[field]
		if !ok || (dir != "" && dir != "asc" && dir != "desc") {
			return errInvalidInput("sort", "%q is not a sort field of this list", p)
		}
		if dir == "desc" {
			asc := compare
			compare = func(a, b T) int { return asc(b, a) }
		}
		compares = append(compares, compare)
	}
	slices.SortStableFunc(items, func(a, b T) int {
		for _, c := range compares {
			if n := c(a, b); n != 0 {
				return n
			}
		}
		return 0
	})
	return nil
}

func byID[T any](id func(T) int64) func(a, b T) int {
	return func(a, b T) int { return cmp.Compare(id(a), id(b)) }
}

func byString[T any](field func(T) string) func(a, b T) int {
	return func(a, b T) int { return strings.Compare(field(a), field(b)) }
}

// rateLimiter is the project's request budget: up to limit requests at
// once, refilled by one request a second, as Hetzner documents.
type rateLimiter struct {
	limit  int
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

type rateUsage struct {
	allowed   bool
	limit     int
	remaining int
	reset     time.Time
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{limit: limit, tokens: float64(limit)}
}

func (l *rateLimiter) take(now time.Time) rateUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		l.tokens = min(float64(l.limit), l.tokens+now.Sub(l.last).Seconds())
	}
	l.last = now
	usage := rateUsage{limit: l.limit}
	if l.tokens >= 1 {
		l.tokens--
		usage.allowed = true
	}
	usage.remaining = int(l.tokens)
	usage.reset = now.Add(time.Duration((float64(l.limit) - l.tokens) * float64(time.Second))).Truncate(time.Second)
	return usage
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}

func labelsOrEmpty(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}
