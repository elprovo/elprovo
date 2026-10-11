// Package gateway serves stagelet traffic. It routes each stagelet host name
// to its upstream, admits visitors through single-use handoffs, and drops any
// session it could not re-confirm with the control plane recently.
package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/protocol"
)

// SessionCookie holds the visitor's session on a stagelet host.
const SessionCookie = "__Host-stagely_session"

// ControlPlane redeems handoffs and confirms sessions.
type ControlPlane interface {
	ExchangeHandoff(ctx context.Context, token string) (protocol.HandoffResult, error)
	ConfirmSessions(ctx context.Context, ids []string) ([]string, error)
}

// Gateway is the HTTP handler for all stagelet hosts.
type Gateway struct {
	cp            ControlPlane
	confirmWindow time.Duration
	now           func() time.Time
	log           *slog.Logger

	mu       sync.RWMutex
	routes   map[string]route
	sessions map[string]session
}

type route struct {
	stageletID string
	upstream   string
	healthPath string
	proxy      *httputil.ReverseProxy
}

type session struct {
	stageletID  string
	expiresAt   time.Time
	confirmedAt time.Time
	requests    map[*http.Request]context.CancelFunc
}

// New returns a gateway. A session older than confirmWindow since its last
// confirmation is refused.
func New(cp ControlPlane, confirmWindow time.Duration, now func() time.Time, log *slog.Logger) *Gateway {
	return &Gateway{
		cp: cp, confirmWindow: confirmWindow, now: now, log: log,
		routes: map[string]route{}, sessions: map[string]session{},
	}
}

// SetRoutes replaces the route table and returns the stagelets it now serves.
func (g *Gateway) SetRoutes(routes []protocol.Route) []string {
	table := make(map[string]route, len(routes))
	applied := make([]string, 0, len(routes))
	for _, r := range routes {
		target := &url.URL{Scheme: "http", Host: r.Upstream}
		if _, _, err := net.SplitHostPort(r.Upstream); err != nil {
			g.log.Warn("skipping route with invalid upstream", "stagelet_id", r.StageletID, "upstream", r.Upstream)
			continue
		}
		table[strings.ToLower(r.Host)] = route{stageletID: r.StageletID, upstream: r.Upstream, healthPath: r.HealthPath, proxy: newProxy(target)}
		applied = append(applied, r.StageletID)
	}
	g.mu.Lock()
	g.routes = table
	g.mu.Unlock()
	return applied
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := g.route(r.Host)
	if !ok {
		http.Error(w, "unknown stagelet", http.StatusNotFound)
		return
	}
	if r.URL.Path == protocol.HandoffPath {
		g.handoff(w, r, rt)
		return
	}
	ctx, done, ok := g.admit(r, rt)
	if !ok {
		http.Error(w, "open this stagelet through Stagely", http.StatusUnauthorized)
		return
	}
	defer done()
	rt.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (g *Gateway) route(hostport string) (route, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rt, ok := g.routes[hostName(hostport)]
	return rt, ok
}

func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

func (g *Gateway) routeChecks() []routeCheck {
	g.mu.RLock()
	defer g.mu.RUnlock()
	seen := make(map[routeKey]bool, len(g.routes))
	checks := make([]routeCheck, 0, len(g.routes))
	for host, rt := range g.routes {
		k := routeKey{stageletID: rt.stageletID, upstream: rt.upstream}
		if !seen[k] {
			seen[k] = true
			checks = append(checks, routeCheck{routeKey: k, host: host, healthPath: rt.healthPath})
		}
	}
	return checks
}

func (g *Gateway) handoff(w http.ResponseWriter, r *http.Request, rt route) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := g.cp.ExchangeHandoff(ctx, r.URL.Query().Get(protocol.HandoffTokenParam))
	if err != nil {
		g.log.Warn("handoff exchange failed", "error", err)
		http.Error(w, "Stagely is unreachable, try again", http.StatusServiceUnavailable)
		return
	}
	if !res.OK || res.StageletID != rt.stageletID {
		http.Error(w, "handoff rejected", http.StatusForbidden)
		return
	}
	now := g.now()
	g.mu.Lock()
	g.sessions[res.SessionID] = session{stageletID: res.StageletID, expiresAt: res.ExpiresAt, confirmedAt: now, requests: map[*http.Request]context.CancelFunc{}}
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: res.SessionID, Path: "/", Expires: res.ExpiresAt,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// admit checks the session of r. For an admitted request, it returns a
// context that ends when the gateway stops the session's requests, and done,
// which the caller must call when the request ends.
func (g *Gateway) admit(r *http.Request, rt route) (context.Context, func(), bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, nil, false
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[c.Value]
	if !ok || s.stageletID != rt.stageletID || !now.Before(s.expiresAt) || now.Sub(s.confirmedAt) > g.confirmWindow {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(r.Context())
	s.requests[r] = cancel
	return ctx, func() {
		g.mu.Lock()
		delete(s.requests, r)
		g.mu.Unlock()
		cancel()
	}, true
}

// stopRequests ends the session's open requests. The caller holds g.mu.
func (s session) stopRequests() {
	for _, cancel := range s.requests {
		cancel()
	}
	clear(s.requests)
}

// ConfirmSessions asks the control plane which sessions are still valid. It
// keeps and refreshes those and forgets the rest. If the control plane is
// unreachable, nothing is refreshed, so sessions run out within the window.
// It ends the open requests of every session that it forgets or that ran out
// of the window.
func (g *Gateway) ConfirmSessions(ctx context.Context) error {
	now := g.now()
	g.mu.Lock()
	ids := make([]string, 0, len(g.sessions))
	for id, s := range g.sessions {
		if now.After(s.expiresAt) {
			s.stopRequests()
			delete(g.sessions, id)
			continue
		}
		if now.Sub(s.confirmedAt) > g.confirmWindow {
			s.stopRequests()
		}
		ids = append(ids, id)
	}
	g.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}

	valid, err := g.cp.ConfirmSessions(ctx, ids)
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(valid))
	for _, id := range valid {
		keep[id] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		s, ok := g.sessions[id]
		if !ok {
			continue
		}
		if !keep[id] {
			s.stopRequests()
			delete(g.sessions, id)
			continue
		}
		s.confirmedAt = now
		g.sessions[id] = s
	}
	return nil
}

// RunConfirmLoop calls ConfirmSessions every interval until ctx ends. Each
// call gives up after half the interval, so a lost or slow reply never holds
// up the next call.
func (g *Gateway) RunConfirmLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, interval/2)
			err := g.ConfirmSessions(callCtx)
			cancel()
			if err != nil {
				g.log.Warn("confirm sessions", "error", err)
			}
		}
	}
}

func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
			removeCookie(pr.Out, SessionCookie)
		},
	}
}

func removeCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}
