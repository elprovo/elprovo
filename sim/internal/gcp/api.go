package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxRequestBody = 1 << 20
	computeBase    = "https://www.googleapis.com/compute/v1/"
	maxListResults = 500
)

type endpoint func(r *http.Request) (int, any, error)

// apiError is an error in Google's JSON error format. Reason is the Compute
// Engine reason, such as notFound; status is derived from the code.
type apiError struct {
	code    int
	reason  string
	message string
}

func (e *apiError) Error() string { return e.message }

func errNotFound(resource string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("The resource '%s' was not found", resource)}
}

func errInvalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...)}
}

func errUnsupported(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...) + " is not supported by the simulation"}
}

func errAlreadyExists(resource string) *apiError {
	return &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource '%s' already exists", resource)}
}

func errInUse(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "resourceInUseByAnotherResource", fmt.Sprintf(format, args...)}
}

var statusNames = map[int]string{
	http.StatusBadRequest:          "INVALID_ARGUMENT",
	http.StatusUnauthorized:        "UNAUTHENTICATED",
	http.StatusForbidden:           "PERMISSION_DENIED",
	http.StatusNotFound:            "NOT_FOUND",
	http.StatusConflict:            "ALREADY_EXISTS",
	http.StatusPreconditionFailed:  "FAILED_PRECONDITION",
	http.StatusTooManyRequests:     "RESOURCE_EXHAUSTED",
	http.StatusInternalServerError: "INTERNAL",
}

func errorBody(e *apiError) map[string]any {
	return map[string]any{"error": map[string]any{
		"code":    e.code,
		"message": e.message,
		"errors":  []map[string]string{{"message": e.message, "domain": "global", "reason": e.reason}},
		"status":  statusNames[e.code],
	}}
}

func (s *Simulation) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", s.issueAccessToken)
	mux.Handle("GET /v2/token", s.registryTokens())
	api := http.NewServeMux()
	for pattern, h := range s.routes() {
		api.Handle(pattern, s.serve(h))
	}
	api.Handle("/", s.serve(func(r *http.Request) (int, any, error) {
		return 0, nil, &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("%s %s is not supported by the simulation", r.Method, r.URL.Path)}
	}))
	mux.Handle("/", s.authenticate(api))
	return mux
}

func (s *Simulation) routes() map[string]endpoint {
	const p = "/compute/v1/projects/{project}"
	routes := map[string]endpoint{
		"GET " + p:                                       s.getProject,
		"GET " + p + "/zones":                            s.listZones,
		"GET " + p + "/zones/{zone}":                     s.getZone,
		"GET " + p + "/regions":                          s.listRegions,
		"GET " + p + "/regions/{region}":                 s.getRegion,
		"GET " + p + "/zones/{zone}/machineTypes":        s.listMachineTypes,
		"GET " + p + "/zones/{zone}/machineTypes/{name}": s.getMachineType,
		"GET " + p + "/zones/{zone}/diskTypes":           s.listDiskTypes,
		"GET " + p + "/zones/{zone}/diskTypes/{name}":    s.getDiskType,

		"POST " + p + "/zones/{zone}/instances":          s.insertInstance,
		"GET " + p + "/zones/{zone}/instances":           s.listInstances,
		"GET " + p + "/zones/{zone}/instances/{name}":    s.getInstance,
		"DELETE " + p + "/zones/{zone}/instances/{name}": s.deleteInstance,

		"GET " + p + "/zones/{zone}/disks":           s.listDisks,
		"GET " + p + "/zones/{zone}/disks/{name}":    s.getDisk,
		"DELETE " + p + "/zones/{zone}/disks/{name}": s.deleteDisk,

		"GET " + p + "/global/images":                                s.listImages,
		"POST " + p + "/global/images":                               s.insertImage,
		"GET " + p + "/global/images/{name}":                         s.getImage,
		"GET " + p + "/global/images/family/{family}":                s.getImageFromFamily,
		"DELETE " + p + "/global/images/{name}":                      s.deleteImage,
		"POST " + p + "/global/images/{name}/deprecate":              s.deprecateImage,
		"GET " + p + "/global/networks":                              s.listNetworks,
		"POST " + p + "/global/networks":                             s.insertNetwork,
		"GET " + p + "/global/networks/{name}":                       s.getNetwork,
		"DELETE " + p + "/global/networks/{name}":                    s.deleteNetwork,
		"GET " + p + "/regions/{region}/subnetworks":                 s.listSubnetworks,
		"POST " + p + "/regions/{region}/subnetworks":                s.insertSubnetwork,
		"GET " + p + "/regions/{region}/subnetworks/{name}":          s.getSubnetwork,
		"DELETE " + p + "/regions/{region}/subnetworks/{name}":       s.deleteSubnetwork,
		"GET " + p + "/global/firewalls":                             s.listFirewalls,
		"POST " + p + "/global/firewalls":                            s.insertFirewall,
		"GET " + p + "/global/firewalls/{name}":                      s.getFirewall,
		"DELETE " + p + "/global/firewalls/{name}":                   s.deleteFirewall,
		"GET " + p + "/regions/{region}/addresses":                   s.listAddresses,
		"POST " + p + "/regions/{region}/addresses":                  s.insertAddress,
		"GET " + p + "/regions/{region}/addresses/{name}":            s.getAddress,
		"DELETE " + p + "/regions/{region}/addresses/{name}":         s.deleteAddress,
		"POST " + p + "/regions/{region}/addresses/{name}/setLabels": s.setAddressLabels,

		"GET " + p + "/zones/{zone}/operations/{name}":           s.getOperation("zone"),
		"POST " + p + "/zones/{zone}/operations/{name}/wait":     s.waitOperation("zone"),
		"GET " + p + "/regions/{region}/operations/{name}":       s.getOperation("region"),
		"POST " + p + "/regions/{region}/operations/{name}/wait": s.waitOperation("region"),
		"GET " + p + "/global/operations/{name}":                 s.getOperation("global"),
		"POST " + p + "/global/operations/{name}/wait":           s.waitOperation("global"),
	}
	s.addArtifactRegistryRoutes(routes)
	s.addIAMRoutes(routes)
	return routes
}

func (s *Simulation) serve(h endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body, err := h(r)
		if err != nil {
			var apiErr *apiError
			if !errors.As(err, &apiErr) {
				s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
				apiErr = &apiError{http.StatusInternalServerError, "backendError", "Internal error. Please try again or contact Google Support."}
			}
			status, body = apiErr.code, errorBody(apiErr)
		}
		writeJSON(w, status, body)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

type callerKey struct{}

// authenticate admits requests with a bearer access token that the
// simulation issued and that has not expired, or one of -gcp-access-token.
// Only the project's own service account and -gcp-access-token manage
// resources; the tokens of other service accounts, such as the registry
// accounts, may only sign in to the registry.
func (s *Simulation) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		email, ok := s.tokenOwner(token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://accounts.google.com/", error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, errorBody(&apiError{
				http.StatusUnauthorized, "authError",
				"Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.",
			}))
			return
		}
		if !s.isOwner(email) {
			writeJSON(w, http.StatusForbidden, errorBody(&apiError{
				http.StatusForbidden, "forbidden", "The caller does not have permission: " + email + " holds no role on project " + s.cfg.project + ".",
			}))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, email)))
	})
}

// tokenOwner returns the service account an access token belongs to, "" for
// a -gcp-access-token, and false for an unknown or expired token.
func (s *Simulation) tokenOwner(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	if slices.Contains(s.cfg.accessTokens, token) {
		return "", true
	}
	var issued issuedToken
	var ok bool
	s.store.Read(func(w *world) { issued, ok = w.Tokens[token] })
	return issued.Email, ok && s.now().Before(issued.Expiry)
}

// isOwner reports whether the caller may manage the project: the project's
// own service account, or a -gcp-access-token.
func (s *Simulation) isOwner(email string) bool {
	if email == "" {
		return true
	}
	owner := false
	s.store.Read(func(w *world) { owner = email == w.Keys[0].Email })
	return owner
}

func caller(r *http.Request) string {
	email, _ := r.Context().Value(callerKey{}).(string)
	return email
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return &apiError{http.StatusBadRequest, "parseError", "Invalid JSON payload received. " + err.Error()}
	}
	return nil
}

// checkProject accepts requests for the simulated project only.
func (s *Simulation) checkProject(r *http.Request) error {
	if p := r.PathValue("project"); p != s.cfg.project {
		return errNotFound("projects/" + p)
	}
	return nil
}

// list is a page of a Compute Engine list.
type list[T any] struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Items         []T    `json:"items,omitempty"`
	NextPageToken string `json:"nextPageToken,omitempty"`
	SelfLink      string `json:"selfLink"`
}

// page returns the page of items that maxResults and pageToken select. A
// filter is an error unless the caller handled it.
func page[T any](r *http.Request, kind, selfLink string, items []T) (int, any, error) {
	q := r.URL.Query()
	size := maxListResults
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, nil, errInvalid("Invalid value for field 'maxResults': '%s'. Must be a non-negative integer.", v)
		}
		if n > 0 {
			size = min(n, maxListResults)
		}
	}
	start := 0
	if v := q.Get("pageToken"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		n, convErr := strconv.Atoi(string(raw))
		if err != nil || convErr != nil || n < 0 || n > len(items) {
			return 0, nil, errInvalid("Invalid value for field 'pageToken': '%s'.", v)
		}
		start = n
	}
	end := min(start+size, len(items))
	out := list[T]{Kind: kind, ID: selfLink, Items: items[start:end], SelfLink: selfLink}
	if end < len(items) {
		out.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return http.StatusOK, out, nil
}

// noFilter rejects a filter on a list that does not support one.
func noFilter(r *http.Request) error {
	if f := r.URL.Query().Get("filter"); f != "" {
		return errUnsupported("filter %q on this list", f)
	}
	return nil
}

func (s *Simulation) link(parts ...string) string {
	return computeBase + "projects/" + s.cfg.project + "/" + strings.Join(parts, "/")
}

// fingerprint is the opaque fingerprint of a value, which Compute Engine
// changes whenever the value changes.
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.StdEncoding.EncodeToString(sum[:8])
}

func timestamp(t time.Time) string {
	return t.Format("2006-01-02T15:04:05.000-07:00")
}

func idString(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// ref is a reference to a Compute Engine resource as clients send it: a full
// URL, a partial URL that starts with projects/ or with the scope, or a bare
// name.
type ref struct {
	project  string
	location string // zone or region; empty for global resources
	kind     string
	name     string
}

func parseRef(value string) ref {
	if _, rest, ok := strings.Cut(value, "/compute/v1/"); ok {
		value = rest
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	var r ref
	if len(parts) >= 2 && parts[0] == "projects" {
		r.project, parts = parts[1], parts[2:]
	}
	switch {
	case len(parts) >= 3 && parts[0] == "global":
		r.kind, r.name = parts[1], strings.Join(parts[2:], "/")
	case len(parts) >= 4 && (parts[0] == "zones" || parts[0] == "regions"):
		r.location, r.kind, r.name = parts[1], parts[2], strings.Join(parts[3:], "/")
	case len(parts) == 1:
		r.name = parts[0]
	}
	return r
}

// belongs reports whether the reference names a resource of kind in the
// simulated project and, for zonal and regional kinds, in location.
func (s *Simulation) belongs(r ref, kind, location string) bool {
	return r.name != "" && (r.project == "" || r.project == s.cfg.project) &&
		(r.kind == "" || r.kind == kind) && (r.location == "" || r.location == location)
}
