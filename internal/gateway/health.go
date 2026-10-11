package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/protocol"
)

const (
	routeHealthInterval    = 20 * time.Second
	routeCheckTimeout      = 3 * time.Second
	healthRequestTimeout   = 5 * time.Second
	maxHealthHeaderBytes   = 64 << 10
	maxConcurrentChecks    = 16
	maxStatesPerMessage    = 100
	healthCheckUserAgent   = "stagely-gateway"
	unreachableHealthError = "the gateway cannot reach the app"
)

type routeKey struct {
	stageletID string
	upstream   string
}

// routeCheck is one route to check: its key, and the host name and health
// path of the health request.
type routeCheck struct {
	routeKey
	host       string
	healthPath string
}

// routeHealth is the part of a route's state whose change the gateway
// reports.
type routeHealth struct {
	reachable bool
	healthy   bool
}

func (g *Gateway) reportRouteHealth(ctx context.Context, tick <-chan time.Time, checkNow <-chan struct{}, send func(context.Context, protocol.RouteHealth) error) error {
	client := newHealthClient()
	defer client.CloseIdleConnections()
	reported := map[routeKey]routeHealth{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		case <-checkNow:
		}
		states := checkRoutes(ctx, client, g.routeChecks(), g.now())
		if ctx.Err() != nil {
			return nil
		}
		var changed []protocol.RouteState
		changed, reported = changedStates(reported, states)
		for batch := range slices.Chunk(changed, maxStatesPerMessage) {
			if err := send(ctx, protocol.RouteHealth{Routes: batch}); err != nil {
				return err
			}
		}
	}
}

// newHealthClient returns the client for health requests. It connects
// directly, keeps no idle connections, does not follow redirects and reads
// at most maxHealthHeaderBytes of response headers, because the app is not
// trusted.
func newHealthClient() *http.Client {
	return &http.Client{
		Timeout: healthRequestTimeout,
		Transport: &http.Transport{
			DialContext:            (&net.Dialer{Timeout: routeCheckTimeout}).DialContext,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxHealthHeaderBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func checkRoutes(ctx context.Context, client *http.Client, routes []routeCheck, checkedAt time.Time) []protocol.RouteState {
	states := make([]protocol.RouteState, len(routes))
	slots := make(chan struct{}, maxConcurrentChecks)
	var wg sync.WaitGroup
	for i, r := range routes {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			states[i] = checkRoute(ctx, client, r, checkedAt)
		})
	}
	wg.Wait()
	return states
}

// checkRoute opens a TCP connection to the route's upstream, and then asks
// the app for its health path.
func checkRoute(ctx context.Context, client *http.Client, r routeCheck, checkedAt time.Time) protocol.RouteState {
	state := protocol.RouteState{StageletID: r.stageletID, Upstream: r.upstream, CheckedAt: checkedAt}
	dialer := net.Dialer{Timeout: routeCheckTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", r.upstream)
	if err != nil {
		state.Error, state.HealthError = err.Error(), unreachableHealthError
		return state
	}
	_ = conn.Close()
	state.Reachable = true
	if err := requestHealthPath(ctx, client, r); err != nil {
		state.HealthError = err.Error()
		return state
	}
	state.Healthy = true
	return state
}

// requestHealthPath sends GET <health path> to the upstream with the
// stagelet's host name, as a visitor's request would carry it, and accepts
// only a 2xx answer.
func requestHealthPath(ctx context.Context, client *http.Client, r routeCheck) error {
	if !strings.HasPrefix(r.healthPath, "/") {
		return errors.New("the route has no health path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.upstream+r.healthPath, nil)
	if err != nil {
		return fmt.Errorf("invalid health path %q", r.healthPath)
	}
	req.Host = r.host
	req.Header.Set("User-Agent", healthCheckUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("GET %s: %w", r.healthPath, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: status %d", r.healthPath, resp.StatusCode)
	}
	return nil
}

func changedStates(reported map[routeKey]routeHealth, states []protocol.RouteState) ([]protocol.RouteState, map[routeKey]routeHealth) {
	var changed []protocol.RouteState
	current := make(map[routeKey]routeHealth, len(states))
	for _, s := range states {
		k := routeKey{stageletID: s.StageletID, upstream: s.Upstream}
		h := routeHealth{reachable: s.Reachable, healthy: s.Healthy}
		current[k] = h
		if was, ok := reported[k]; !ok || was != h {
			changed = append(changed, s)
		}
	}
	return changed, current
}
