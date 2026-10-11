package digitalocean

import (
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

const maxRequestBody = 1 << 20

type endpoint func(r *http.Request) (int, any, error)

type apiError struct {
	status  int
	id      string
	message string
}

func (e *apiError) Error() string { return e.message }

func errNotFound() *apiError {
	return &apiError{http.StatusNotFound, "not_found", "The resource you requested could not be found."}
}

func errUnprocessable(format string, args ...any) *apiError {
	return &apiError{http.StatusUnprocessableEntity, "unprocessable_entity", fmt.Sprintf(format, args...)}
}

func errBadRequest(message string) *apiError {
	return &apiError{http.StatusBadRequest, "bad_request", message}
}

func errForbidden(message string) *apiError {
	return &apiError{http.StatusForbidden, "forbidden", message}
}

func errConflict(message string) *apiError {
	return &apiError{http.StatusConflict, "conflict", message}
}

func (s *Simulation) handler() http.Handler {
	routes := map[string]endpoint{
		"GET /v2/account": s.getAccount,
		"GET /v2/regions": s.listRegions,
		"GET /v2/sizes":   s.listSizes,

		"GET /v2/images":            s.listImages,
		"GET /v2/images/{id}":       s.getImage,
		"DELETE /v2/images/{id}":    s.deleteImage,
		"GET /v2/snapshots":         s.listSnapshots,
		"GET /v2/snapshots/{id}":    s.getSnapshot,
		"DELETE /v2/snapshots/{id}": s.deleteSnapshot,

		"GET /v2/account/keys":          s.listKeys,
		"POST /v2/account/keys":         s.createKey,
		"GET /v2/account/keys/{key}":    s.getKey,
		"PUT /v2/account/keys/{key}":    s.updateKey,
		"DELETE /v2/account/keys/{key}": s.deleteKey,

		"POST /v2/droplets":                      s.createDroplets,
		"GET /v2/droplets":                       s.listDroplets,
		"DELETE /v2/droplets":                    s.deleteDropletsByTag,
		"GET /v2/droplets/{id}":                  s.getDroplet,
		"DELETE /v2/droplets/{id}":               s.deleteDroplet,
		"GET /v2/droplets/{id}/snapshots":        s.listDropletSnapshots,
		"POST /v2/droplets/{id}/actions":         s.postDropletAction,
		"GET /v2/droplets/{id}/actions":          s.listDropletActions,
		"GET /v2/droplets/{id}/actions/{action}": s.getDropletAction,
		"POST /v2/droplets/actions":              s.postTagAction,
		"GET /v2/actions":                        s.listActions,
		"GET /v2/actions/{id}":                   s.getAction,

		"POST /v2/tags":                    s.createTag,
		"GET /v2/tags":                     s.listTags,
		"GET /v2/tags/{name}":              s.getTag,
		"DELETE /v2/tags/{name}":           s.deleteTag,
		"POST /v2/tags/{name}/resources":   s.tagResources,
		"DELETE /v2/tags/{name}/resources": s.untagResources,

		"POST /v2/vpcs":             s.createVPC,
		"GET /v2/vpcs":              s.listVPCs,
		"GET /v2/vpcs/{id}":         s.getVPC,
		"PUT /v2/vpcs/{id}":         s.updateVPC,
		"PATCH /v2/vpcs/{id}":       s.updateVPC,
		"DELETE /v2/vpcs/{id}":      s.deleteVPC,
		"GET /v2/vpcs/{id}/members": s.listVPCMembers,

		"POST /v2/firewalls":                 s.createFirewall,
		"GET /v2/firewalls":                  s.listFirewalls,
		"GET /v2/firewalls/{id}":             s.getFirewall,
		"PUT /v2/firewalls/{id}":             s.updateFirewall,
		"DELETE /v2/firewalls/{id}":          s.deleteFirewall,
		"POST /v2/firewalls/{id}/droplets":   s.addFirewallDroplets,
		"DELETE /v2/firewalls/{id}/droplets": s.removeFirewallDroplets,
		"POST /v2/firewalls/{id}/tags":       s.addFirewallTags,
		"DELETE /v2/firewalls/{id}/tags":     s.removeFirewallTags,
		"POST /v2/firewalls/{id}/rules":      s.addFirewallRules,
		"DELETE /v2/firewalls/{id}/rules":    s.removeFirewallRules,

		"GET /v2/reserved_ips":                       s.listReservedIPs,
		"POST /v2/reserved_ips":                      s.createReservedIP,
		"GET /v2/reserved_ips/{ip}":                  s.getReservedIP,
		"DELETE /v2/reserved_ips/{ip}":               s.deleteReservedIP,
		"POST /v2/reserved_ips/{ip}/actions":         s.postReservedIPAction,
		"GET /v2/reserved_ips/{ip}/actions":          s.listReservedIPActions,
		"GET /v2/reserved_ips/{ip}/actions/{action}": s.getReservedIPAction,

		"GET /v2/domains":                        s.listDomains,
		"POST /v2/domains":                       s.createDomain,
		"GET /v2/domains/{name}":                 s.getDomain,
		"DELETE /v2/domains/{name}":              s.deleteDomain,
		"GET /v2/domains/{name}/records":         s.listDomainRecords,
		"POST /v2/domains/{name}/records":        s.createDomainRecord,
		"GET /v2/domains/{name}/records/{id}":    s.getDomainRecord,
		"PUT /v2/domains/{name}/records/{id}":    s.updateDomainRecord,
		"PATCH /v2/domains/{name}/records/{id}":  s.updateDomainRecord,
		"DELETE /v2/domains/{name}/records/{id}": s.deleteDomainRecord,
	}
	s.addRegistryRoutes(routes)
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.Handle(pattern, s.serve(h))
	}
	mux.Handle("/", s.serve(func(*http.Request) (int, any, error) { return 0, nil, errNotFound() }))
	return s.authenticate(mux)
}

func (s *Simulation) serve(h endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body, err := h(r)
		if err != nil {
			var apiErr *apiError
			if !errors.As(err, &apiErr) {
				s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
				apiErr = &apiError{http.StatusInternalServerError, "server_error", "Unexpected server-side error"}
			}
			status = apiErr.status
			body = map[string]string{"id": apiErr.id, "message": apiErr.message, "request_id": w.Header().Get("X-Request-Id")}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}

func (s *Simulation) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", newUUID())
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !slices.Contains(s.cfg.tokens, token) {
			s.serve(func(*http.Request) (int, any, error) {
				return 0, nil, &apiError{http.StatusUnauthorized, "unauthorized", "Unable to authenticate you."}
			}).ServeHTTP(w, r)
			return
		}
		usage := s.limiter.take(token, s.now())
		h := w.Header()
		h.Set("Ratelimit-Limit", strconv.Itoa(usage.limit))
		h.Set("Ratelimit-Remaining", strconv.Itoa(usage.remaining))
		h.Set("Ratelimit-Reset", strconv.FormatInt(usage.reset.Unix(), 10))
		if usage.retryAfter > 0 {
			h.Set("Retry-After", strconv.Itoa(int(usage.retryAfter.Seconds()+0.999)))
			s.serve(func(*http.Request) (int, any, error) {
				return 0, nil, &apiError{http.StatusTooManyRequests, "too_many_requests", "API Rate limit exceeded."}
			}).ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errBadRequest("error parsing request body")
	}
	return nil
}

type links struct {
	Pages   *pageLinks   `json:"pages,omitempty"`
	Actions []actionLink `json:"actions,omitempty"`
}

type pageLinks struct {
	First string `json:"first,omitempty"`
	Prev  string `json:"prev,omitempty"`
	Next  string `json:"next,omitempty"`
	Last  string `json:"last,omitempty"`
}

type actionLink struct {
	ID   int64  `json:"id"`
	Rel  string `json:"rel"`
	HREF string `json:"href"`
}

type meta struct {
	Total int `json:"total"`
}

func paginate[T any](r *http.Request, items []T) ([]T, links, meta, error) {
	q := r.URL.Query()
	page, perPage := 1, 20
	var err error
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			return nil, links{}, meta{}, errUnprocessable("page must be a positive integer")
		}
	}
	if v := q.Get("per_page"); v != "" {
		if perPage, err = strconv.Atoi(v); err != nil || perPage < 1 {
			return nil, links{}, meta{}, errUnprocessable("per_page must be a positive integer")
		}
		perPage = min(perPage, 200)
	}
	total := len(items)
	pages := max(1, (total+perPage-1)/perPage)
	start := min((page-1)*perPage, total)
	end := min(start+perPage, total)
	out := items[start:end]
	if out == nil {
		out = []T{}
	}
	var l links
	if pages > 1 {
		l.Pages = &pageLinks{}
		if page > 1 {
			l.Pages.First, l.Pages.Prev = pageURL(r, 1, perPage), pageURL(r, min(page-1, pages), perPage)
		}
		if page < pages {
			l.Pages.Next, l.Pages.Last = pageURL(r, page+1, perPage), pageURL(r, pages, perPage)
		}
	}
	return out, l, meta{Total: total}, nil
}

func pageURL(r *http.Request, page, perPage int) string {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(perPage))
	u := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
	return u.String()
}

func baseURL(r *http.Request) string {
	return "http://" + r.Host
}

type rateLimiter struct {
	limits rateLimits
	mu     sync.Mutex
	seen   map[string][]time.Time
}

type rateUsage struct {
	limit      int
	remaining  int
	reset      time.Time
	retryAfter time.Duration
}

func newRateLimiter(limits rateLimits) *rateLimiter {
	return &rateLimiter{limits: limits, seen: map[string][]time.Time{}}
}

func (l *rateLimiter) take(token string, now time.Time) rateUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	times := l.seen[token]
	cut := 0
	for cut < len(times) && now.Sub(times[cut]) >= time.Hour {
		cut++
	}
	times = times[cut:]
	usage := rateUsage{limit: l.limits.perHour, reset: now.Add(time.Hour)}
	if len(times) > 0 {
		usage.reset = times[0].Add(time.Hour)
	}
	lastMinute := 0
	for _, t := range times {
		if now.Sub(t) < time.Minute {
			lastMinute++
		}
	}
	switch {
	case len(times) >= l.limits.perHour:
		usage.retryAfter = usage.reset.Sub(now)
	case lastMinute >= l.limits.perMinute:
		usage.retryAfter = times[len(times)-lastMinute].Add(time.Minute).Sub(now)
	default:
		times = append(times, now)
	}
	l.seen[token] = times
	usage.remaining = max(0, l.limits.perHour-len(times))
	return usage
}
