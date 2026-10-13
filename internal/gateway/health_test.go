package gateway

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elprovo/elprovo/protocol"
)

var checkedAt = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type healthRun struct {
	checkNow chan struct{}
	sent     chan protocol.RouteHealth
}

func startRouteHealth(t *testing.T, gw *Gateway) *healthRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &healthRun{checkNow: make(chan struct{}), sent: make(chan protocol.RouteHealth, 10)}
	done := make(chan error, 1)
	go func() {
		done <- gw.reportRouteHealth(ctx, nil, h.checkNow, func(_ context.Context, m protocol.RouteHealth) error {
			h.sent <- m
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("reportRouteHealth: %v", err)
		}
	})
	return h
}

// check starts a check round. It returns once the loop takes the request,
// which means the previous round has finished.
func (h *healthRun) check() { h.checkNow <- struct{}{} }

func (h *healthRun) next(t *testing.T) []protocol.RouteState {
	t.Helper()
	select {
	case m := <-h.sent:
		return slices.SortedFunc(slices.Values(m.Routes), func(a, b protocol.RouteState) int {
			return cmp.Compare(a.PreviewID, b.PreviewID)
		})
	case <-time.After(5 * time.Second):
		t.Fatal("no route health report")
		return nil
	}
}

func newHealthGateway() *Gateway {
	return New(&fakeControlPlane{}, 50*time.Second, func() time.Time { return checkedAt }, slog.New(slog.DiscardHandler))
}

const healthPath = "/healthz"

// startApp starts a fake upstream app whose handler answers every request.
func startApp(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

// startUpstream starts an app that answers its health path with 200.
func startUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return startApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			http.NotFound(w, r)
		}
	})
}

func answering(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }
}

func checkOnce(t *testing.T, r routeCheck) protocol.RouteState {
	t.Helper()
	return checkRoute(t.Context(), newHealthClient(), r, checkedAt)
}

func routeTo(upstream string) routeCheck {
	return routeCheck{routeKey: routeKey{previewID: "s1", upstream: upstream}, host: host, healthPath: healthPath}
}

func addr(s *httptest.Server) string { return s.Listener.Addr().String() }

func refusingAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return a
}

func TestFirstHealthReportHoldsEveryRoute(t *testing.T) {
	gw := newHealthGateway()
	up, down := addr(startUpstream(t)), refusingAddr(t)
	gw.SetRoutes([]protocol.Route{
		{PreviewID: "s-down", Host: "down.previews.localhost", Upstream: down, HealthPath: healthPath},
		{PreviewID: "s-up", Host: "up.previews.localhost", Upstream: up, HealthPath: healthPath},
	})
	h := startRouteHealth(t, gw)

	h.check()
	got := h.next(t)

	if len(got) != 2 {
		t.Fatalf("report = %+v, want both routes", got)
	}
	if s := got[0]; s.PreviewID != "s-down" || s.Upstream != down || s.Reachable || s.Error == "" || s.Healthy || s.HealthError != unreachableHealthError || !s.CheckedAt.Equal(checkedAt) {
		t.Fatalf("down route = %+v, want unreachable and unhealthy with errors", s)
	}
	if want := (protocol.RouteState{PreviewID: "s-up", Upstream: up, Reachable: true, Healthy: true, CheckedAt: checkedAt}); got[1] != want {
		t.Fatalf("up route = %+v, want %+v", got[1], want)
	}
}

func TestAppIsHealthyOnlyWhenItsHealthPathAnswers2xx(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		healthy     bool
		healthError string
	}{
		{name: "ok", status: http.StatusOK, healthy: true},
		{name: "no content", status: http.StatusNoContent, healthy: true},
		{name: "server error", status: http.StatusServiceUnavailable, healthError: "GET /healthz: status 503"},
		{name: "redirect is not followed", status: http.StatusFound, healthError: "GET /healthz: status 302"},
		{name: "not found", status: http.StatusNotFound, healthError: "GET /healthz: status 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := addr(startApp(t, answering(tt.status)))

			got := checkOnce(t, routeTo(up))

			want := protocol.RouteState{PreviewID: "s1", Upstream: up, Reachable: true, Healthy: tt.healthy, HealthError: tt.healthError, CheckedAt: checkedAt}
			if got != want {
				t.Fatalf("state = %+v, want %+v", got, want)
			}
		})
	}
}

func TestHealthRequestAsksForTheHealthPathWithThePreviewHost(t *testing.T) {
	requests := make(chan *http.Request, 1)
	up := addr(startApp(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r
	}))

	route := routeTo(up)
	route.healthPath = "/up?full=1"
	got := checkOnce(t, route)

	r := <-requests
	if r.Method != http.MethodGet || r.URL.RequestURI() != "/up?full=1" || r.Host != host {
		t.Fatalf("request = %s %s on %s, want GET /up?full=1 on %s", r.Method, r.URL.RequestURI(), r.Host, host)
	}
	if !got.Healthy {
		t.Fatalf("state = %+v, want healthy", got)
	}
}

func TestRouteWithoutAHealthPathIsReachableButNotHealthy(t *testing.T) {
	route := routeTo(addr(startUpstream(t)))
	route.healthPath = ""

	got := checkOnce(t, route)

	if !got.Reachable || got.Healthy || got.HealthError != "the route has no health path" {
		t.Fatalf("state = %+v, want reachable and unhealthy for lack of a health path", got)
	}
}

func TestHealthReportsAnAppThatStopsAnsweringWhileItsPortStaysReachable(t *testing.T) {
	gw := newHealthGateway()
	var mu sync.Mutex
	status := http.StatusOK
	up := addr(startApp(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: host, Upstream: up, HealthPath: healthPath}})
	h := startRouteHealth(t, gw)
	h.check()
	if first := h.next(t); len(first) != 1 || !first[0].Healthy {
		t.Fatalf("first report = %+v, want s1 healthy", first)
	}

	mu.Lock()
	status = http.StatusInternalServerError
	mu.Unlock()
	h.check()
	got := h.next(t)

	if len(got) != 1 || !got[0].Reachable || got[0].Healthy || got[0].HealthError != "GET /healthz: status 500" {
		t.Fatalf("report = %+v, want s1 reachable and unhealthy with status 500", got)
	}
}

func TestHealthReportsOnlyRoutesThatChanged(t *testing.T) {
	gw := newHealthGateway()
	stable, flapping := startUpstream(t), startUpstream(t)
	gw.SetRoutes([]protocol.Route{
		{PreviewID: "s-stable", Host: "stable.previews.localhost", Upstream: addr(stable), HealthPath: healthPath},
		{PreviewID: "s-flapping", Host: "flapping.previews.localhost", Upstream: addr(flapping), HealthPath: healthPath},
	})
	h := startRouteHealth(t, gw)
	h.check()
	h.next(t)

	h.check()
	flapping.Close()
	h.check()
	got := h.next(t)

	if len(got) != 1 || got[0].PreviewID != "s-flapping" || got[0].Reachable {
		t.Fatalf("report = %+v, want only s-flapping as unreachable", got)
	}
}

func TestHealthReportsANewReasonWhileTheAppStaysUnhealthy(t *testing.T) {
	gw := newHealthGateway()
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	up := addr(startApp(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: host, Upstream: up, HealthPath: healthPath}})
	h := startRouteHealth(t, gw)
	h.check()
	if first := h.next(t); len(first) != 1 || first[0].HealthError != "GET /healthz: status 503" {
		t.Fatalf("first report = %+v, want s1 unhealthy with status 503", first)
	}

	status.Store(http.StatusInternalServerError)
	h.check()
	got := h.next(t)

	if len(got) != 1 || !got[0].Reachable || got[0].Healthy || got[0].HealthError != "GET /healthz: status 500" {
		t.Fatalf("report = %+v, want s1 reachable and unhealthy with status 500", got)
	}
}

func TestHealthReportsNewUpstreamOfPreview(t *testing.T) {
	gw := newHealthGateway()
	oldUp, newUp := addr(startUpstream(t)), addr(startUpstream(t))
	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: host, Upstream: oldUp, HealthPath: healthPath}})
	h := startRouteHealth(t, gw)
	h.check()
	h.next(t)

	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: host, Upstream: newUp, HealthPath: healthPath}})
	h.check()
	got := h.next(t)

	if len(got) != 1 || got[0].Upstream != newUp || !got[0].Reachable || !got[0].Healthy {
		t.Fatalf("report = %+v, want s1 reachable and healthy on %s", got, newUp)
	}
}

func TestLargeHealthReportIsSplitIntoBoundedMessages(t *testing.T) {
	gw := newHealthGateway()
	up := addr(startUpstream(t))
	routes := make([]protocol.Route, 101)
	for i := range routes {
		id := fmt.Sprintf("s%03d", i)
		routes[i] = protocol.Route{PreviewID: id, Host: id + ".previews.localhost", Upstream: up}
	}
	gw.SetRoutes(routes)
	h := startRouteHealth(t, gw)

	h.check()
	first, second := h.next(t), h.next(t)

	if len(first) != 100 || len(second) != 1 {
		t.Fatalf("message sizes = %d and %d, want 100 and 1", len(first), len(second))
	}
}

func TestHealthReportingStopsWhenSendFails(t *testing.T) {
	gw := newHealthGateway()
	gw.SetRoutes([]protocol.Route{{PreviewID: "s1", Host: host, Upstream: addr(startUpstream(t))}})
	sendErr := errors.New("session closed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checkNow := make(chan struct{}, 1)
	checkNow <- struct{}{}

	err := gw.reportRouteHealth(ctx, nil, checkNow, func(context.Context, protocol.RouteHealth) error { return sendErr })

	if !errors.Is(err, sendErr) {
		t.Fatalf("reportRouteHealth = %v, want %v", err, sendErr)
	}
}

// startResettingApp starts an upstream that reads each request and then
// resets the connection.
func startResettingApp(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

func TestHealthReasonOfAResetConnectionStaysTheSameAcrossChecks(t *testing.T) {
	gw := newHealthGateway()
	resetting, stable := startResettingApp(t), startUpstream(t)
	gw.SetRoutes([]protocol.Route{
		{PreviewID: "s-resetting", Host: "resetting.previews.localhost", Upstream: resetting, HealthPath: healthPath},
		{PreviewID: "s-stable", Host: "stable.previews.localhost", Upstream: addr(stable), HealthPath: healthPath},
	})
	h := startRouteHealth(t, gw)
	h.check()
	first := h.next(t)
	if len(first) != 2 || first[0].Healthy || first[0].HealthError == "" || strings.Contains(first[0].HealthError, "127.0.0.1") {
		t.Fatalf("first report = %+v, want s-resetting unhealthy with a reason that names no address", first)
	}

	h.check()
	stable.Close()
	h.check()
	got := h.next(t)

	if len(got) != 1 || got[0].PreviewID != "s-stable" {
		t.Fatalf("report = %+v, want only s-stable: the reset route's reason must not change between checks", got)
	}
}
