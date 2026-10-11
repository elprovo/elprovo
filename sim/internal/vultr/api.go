package vultr

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestBody = 1 << 20
	defaultPerPage = 100
	maxPerPage     = 500
)

type endpoint func(r *http.Request) (int, any, error)

// apiError is an error response: {"error": message, "status": status}.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func errNotFound(message string) *apiError {
	return &apiError{http.StatusNotFound, message}
}

func errBadRequest(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

type errorBody struct {
	Error  string `json:"error"`
	Status int    `json:"status"`
}

// handler routes each path to its methods. As on Vultr, an unknown path is
// a 404 and a known path with another method a 405, both before the API key
// is checked.
func (s *Simulation) handler() http.Handler {
	routes := map[string]map[string]endpoint{
		"/v2/account": {"GET": s.getAccount},
		"/v2/regions": {"GET": s.listRegions},
		"/v2/plans":   {"GET": s.listPlans},
		"/v2/os":      {"GET": s.listOS},

		"/v2/ssh-keys":      {"GET": s.listSSHKeys, "POST": s.createSSHKey},
		"/v2/ssh-keys/{id}": {"GET": s.getSSHKey, "PATCH": s.updateSSHKey, "DELETE": s.deleteSSHKey},

		"/v2/instances":                      {"GET": s.listInstances, "POST": s.createInstance},
		"/v2/instances/{id}":                 {"GET": s.getInstance, "PATCH": s.updateInstance, "DELETE": s.deleteInstance},
		"/v2/instances/{id}/vpcs":            {"GET": s.listInstanceVPCs},
		"/v2/instances/{id}/vpc2":            {"GET": s.listInstanceVPC2s},
		"/v2/instances/{id}/backup-schedule": {"GET": s.getBackupSchedule},
		"/v2/instances/{id}/user-data":       {"GET": s.getUserData},

		"/v2/vpcs":      {"GET": s.listVPCs, "POST": s.createVPC},
		"/v2/vpcs/{id}": {"GET": s.getVPC, "PUT": s.updateVPC, "DELETE": s.deleteVPC},

		"/v2/firewalls":                  {"GET": s.listFirewallGroups, "POST": s.createFirewallGroup},
		"/v2/firewalls/{id}":             {"GET": s.getFirewallGroup, "PUT": s.updateFirewallGroup, "DELETE": s.deleteFirewallGroup},
		"/v2/firewalls/{id}/rules":       {"GET": s.listFirewallRules, "POST": s.createFirewallRule},
		"/v2/firewalls/{id}/rules/{num}": {"GET": s.getFirewallRule, "DELETE": s.deleteFirewallRule},

		"/v2/reserved-ips":             {"GET": s.listReservedIPs, "POST": s.createReservedIP},
		"/v2/reserved-ips/{id}":        {"GET": s.getReservedIP, "PATCH": s.updateReservedIP, "DELETE": s.deleteReservedIP},
		"/v2/reserved-ips/{id}/attach": {"POST": s.attachReservedIP},
		"/v2/reserved-ips/{id}/detach": {"POST": s.detachReservedIP},

		"/v2/snapshots":      {"GET": s.listSnapshots, "POST": s.createSnapshot},
		"/v2/snapshots/{id}": {"GET": s.getSnapshot, "PUT": s.updateSnapshot, "DELETE": s.deleteSnapshot},

		"/v2/registry":                         {"POST": s.createRegistry},
		"/v2/registries":                       {"GET": s.listRegistries},
		"/v2/registry/{id}":                    {"GET": s.getRegistry, "PUT": s.updateRegistry, "DELETE": s.deleteRegistry},
		"/v2/registry/{id}/docker-credentials": {"OPTIONS": s.dockerCredentials},
		"/v2/registry/plan/list":               {"GET": s.listRegistryPlans},
	}
	mux := http.NewServeMux()
	for pattern, methods := range routes {
		mux.Handle(pattern, s.dispatch(methods))
	}
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{"Not found.", http.StatusNotFound})
	}))
	return mux
}

func (s *Simulation) dispatch(methods map[string]endpoint) http.Handler {
	allowed := strings.Join(slices.Sorted(maps.Keys(methods)), ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := methods[r.Method]
		if !ok {
			w.Header().Set("Allow", allowed)
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{"Method not allowed. Must be one of: " + allowed, http.StatusMethodNotAllowed})
			return
		}
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !slices.Contains(s.cfg.apiKeys, key) {
			writeJSON(w, http.StatusUnauthorized, errorBody{"Invalid API token.", http.StatusUnauthorized})
			return
		}
		if !s.limiter.allow(clientAddress(r), s.now()) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, errorBody{"Rate limit reached - please try your request again later.", http.StatusTooManyRequests})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		status, body, err := h(r)
		if err != nil {
			var apiErr *apiError
			if !errors.As(err, &apiErr) {
				s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
				apiErr = &apiError{http.StatusInternalServerError, "Internal server error."}
			}
			status, body = apiErr.status, errorBody{apiErr.message, apiErr.status}
		}
		writeJSON(w, status, body)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errBadRequest("Unable to parse the request body as JSON.")
	}
	return nil
}

type meta struct {
	Total int   `json:"total"`
	Links links `json:"links"`
}

type links struct {
	Next string `json:"next"`
	Prev string `json:"prev"`
}

// listResponse answers with one page of items under key and the meta object.
// per_page defaults to 100 and is at most 500; a value that is not a
// positive number means the default, as on Vultr. The cursor names the page:
// an opaque token from links.next or links.prev. A cursor the simulation did
// not hand out starts from the first page, as on Vultr.
func listResponse[T any](r *http.Request, key string, items []T) (int, any, error) {
	q := r.URL.Query()
	perPage, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = defaultPerPage
	}
	perPage = min(perPage, maxPerPage)
	start := min(cursorOffset(q.Get("cursor")), len(items))
	end := min(start+perPage, len(items))
	page := items[start:end]
	if page == nil {
		page = []T{}
	}
	m := meta{Total: len(items)}
	if end < len(items) {
		m.Links.Next = cursor("next", end)
	}
	if start > 0 {
		m.Links.Prev = cursor("prev", max(0, start-perPage))
	}
	return http.StatusOK, map[string]any{key: page, "meta": m}, nil
}

func cursor(direction string, offset int) string {
	return base64.URLEncoding.EncodeToString([]byte(direction + "__" + strconv.Itoa(offset)))
}

func cursorOffset(value string) int {
	raw, err := base64.URLEncoding.DecodeString(value)
	if err != nil {
		return 0
	}
	_, number, ok := strings.Cut(string(raw), "__")
	offset, err := strconv.Atoi(number)
	if !ok || err != nil || offset < 0 {
		return 0
	}
	return offset
}

// rateLimiter allows each client address a number of requests in any
// second, as Vultr limits by the address a request comes from.
type rateLimiter struct {
	perSecond int
	mu        sync.Mutex
	seen      map[string][]time.Time
}

func newRateLimiter(perSecond int) *rateLimiter {
	return &rateLimiter{perSecond: perSecond, seen: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	times := slices.DeleteFunc(l.seen[client], func(t time.Time) bool { return now.Sub(t) >= time.Second })
	if len(times) >= l.perSecond {
		l.seen[client] = times
		return false
	}
	l.seen[client] = append(times, now)
	return true
}
