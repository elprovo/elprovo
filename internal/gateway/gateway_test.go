package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/stagely-dev/stagely/protocol"
)

const host = "s1.stagelets.localhost"

type fakeControlPlane struct {
	handoffs   map[string]protocol.HandoffResult
	valid      map[string]bool
	confirmErr error
}

func (f *fakeControlPlane) ExchangeHandoff(_ context.Context, token string) (protocol.HandoffResult, error) {
	res, ok := f.handoffs[token]
	if !ok {
		return protocol.HandoffResult{OK: false, Error: "unknown handoff"}, nil
	}
	delete(f.handoffs, token)
	return res, nil
}

func (f *fakeControlPlane) ConfirmSessions(_ context.Context, ids []string) ([]string, error) {
	if f.confirmErr != nil {
		return nil, f.confirmErr
	}
	var valid []string
	for _, id := range ids {
		if f.valid[id] {
			valid = append(valid, id)
		}
	}
	return valid, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	gw       *Gateway
	cp       *fakeControlPlane
	clock    *clock
	upstream *httptest.Server
	seen     *http.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clock: &clock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen = r
		_, _ = w.Write([]byte("app"))
	}))
	t.Cleanup(f.upstream.Close)
	f.cp = &fakeControlPlane{
		handoffs: map[string]protocol.HandoffResult{
			"good":  {OK: true, SessionID: "sess1", StageletID: "s1", ExpiresAt: f.clock.t.Add(time.Hour)},
			"other": {OK: true, SessionID: "sess2", StageletID: "s2", ExpiresAt: f.clock.t.Add(time.Hour)},
		},
		valid: map[string]bool{"sess1": true},
	}
	f.gw = New(f.cp, 50*time.Second, f.clock.now, slog.New(slog.DiscardHandler))
	upstream, _ := url.Parse(f.upstream.URL)
	f.gw.SetRoutes([]protocol.Route{{StageletID: "s1", Host: host, Upstream: upstream.Host}})
	return f
}

func (f *fixture) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+host+":8443"+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.gw.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) enter(t *testing.T, token string) *http.Cookie {
	t.Helper()
	rec := f.get(protocol.HandoffPath + "?token=" + token)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("handoff status = %d, want 303", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatal("handoff set no session cookie")
	return nil
}

func TestAnonymousRequestIsRefused(t *testing.T) {
	f := newFixture(t)
	if rec := f.get("/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if f.seen != nil {
		t.Fatal("upstream received an anonymous request")
	}
}

func TestUnknownHostIsNotFound(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://nope.stagelets.localhost/", nil)
	rec := httptest.NewRecorder()
	f.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandoffAdmitsVisitorWithoutLeakingSessionCookie(t *testing.T) {
	f := newFixture(t)
	cookie := f.enter(t, "good")
	if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("cookie = %+v, want a host-only Secure HttpOnly cookie on /", cookie)
	}

	rec := f.get("/", cookie, &http.Cookie{Name: "app", Value: "keep"})
	if rec.Code != http.StatusOK || rec.Body.String() != "app" {
		t.Fatalf("proxied response = %d %q, want 200 app", rec.Code, rec.Body.String())
	}
	if got := f.seen.Header.Get("Cookie"); strings.Contains(got, SessionCookie) || !strings.Contains(got, "app=keep") {
		t.Fatalf("upstream Cookie header = %q, want the app cookie only", got)
	}
}

func TestHandoffIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.enter(t, "good")
	if rec := f.get(protocol.HandoffPath + "?token=good"); rec.Code != http.StatusForbidden {
		t.Fatalf("second handoff status = %d, want 403", rec.Code)
	}
}

func TestHandoffForAnotherStageletIsRejected(t *testing.T) {
	f := newFixture(t)
	if rec := f.get(protocol.HandoffPath + "?token=other"); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRevokedSessionIsDroppedOnConfirm(t *testing.T) {
	f := newFixture(t)
	cookie := f.enter(t, "good")
	f.cp.valid = map[string]bool{}

	if err := f.gw.ConfirmSessions(context.Background()); err != nil {
		t.Fatalf("ConfirmSessions: %v", err)
	}
	if rec := f.get("/", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401", rec.Code)
	}
}

func TestSessionFailsClosedWhenControlPlaneIsUnreachable(t *testing.T) {
	f := newFixture(t)
	cookie := f.enter(t, "good")
	f.cp.confirmErr = errors.New("disconnected")

	f.clock.t = f.clock.t.Add(40 * time.Second)
	if err := f.gw.ConfirmSessions(context.Background()); err == nil {
		t.Fatal("ConfirmSessions succeeded while the control plane was unreachable")
	}
	if rec := f.get("/", cookie); rec.Code != http.StatusOK {
		t.Fatalf("status within the window = %d, want 200", rec.Code)
	}

	f.clock.t = f.clock.t.Add(11 * time.Second)
	if rec := f.get("/", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status after the window = %d, want 401", rec.Code)
	}
}

func TestFinishedRequestIsNoLongerTrackedUnderItsSession(t *testing.T) {
	f := newFixture(t)
	cookie := f.enter(t, "good")
	if rec := f.get("/", cookie); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if n := len(f.gw.sessions["sess1"].requests); n != 0 {
		t.Fatalf("session tracks %d finished requests, want 0", n)
	}
}

func TestRevokingASessionClosesItsOpenWebSocketOnly(t *testing.T) {
	f := newFixture(t)
	gatewayURL := f.serveWebSocketApp(t)
	f.addSession("second", "sess3")
	revoked := openWebSocket(t, gatewayURL, f.enter(t, "good"))
	kept := openWebSocket(t, gatewayURL, f.enter(t, "second"))
	requireEcho(t, revoked)

	delete(f.cp.valid, "sess1")
	if err := f.gw.ConfirmSessions(context.Background()); err != nil {
		t.Fatalf("ConfirmSessions: %v", err)
	}

	requireClosed(t, revoked)
	requireEcho(t, kept)
}

func TestUnconfirmedSessionLosesItsOpenWebSocketWhenTheWindowPasses(t *testing.T) {
	f := newFixture(t)
	gatewayURL := f.serveWebSocketApp(t)
	stale := openWebSocket(t, gatewayURL, f.enter(t, "good"))
	f.clock.t = f.clock.t.Add(40 * time.Second)
	f.addSession("second", "sess3")
	fresh := openWebSocket(t, gatewayURL, f.enter(t, "second"))
	requireEcho(t, stale)

	f.cp.confirmErr = errors.New("disconnected")
	f.clock.t = f.clock.t.Add(11 * time.Second)
	if err := f.gw.ConfirmSessions(context.Background()); err == nil {
		t.Fatal("ConfirmSessions succeeded while the control plane was unreachable")
	}

	requireClosed(t, stale)
	requireEcho(t, fresh)
}

// addSession lets the control plane hand off one more valid session for
// stagelet s1 with token.
func (f *fixture) addSession(token, id string) {
	f.cp.handoffs[token] = protocol.HandoffResult{OK: true, SessionID: id, StageletID: "s1", ExpiresAt: f.clock.t.Add(time.Hour)}
	f.cp.valid[id] = true
}

// serveWebSocketApp routes stagelet s1 to an app that echoes WebSocket
// messages, and serves the gateway over HTTP. It returns the gateway's URL.
func (f *fixture) serveWebSocketApp(t *testing.T) string {
	t.Helper()
	app := startApp(t, echoWebSocket)
	f.gw.SetRoutes([]protocol.Route{{StageletID: "s1", Host: host, Upstream: addr(app)}})
	return startApp(t, f.gw.ServeHTTP).URL
}

func echoWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		typ, data, err := conn.Read(r.Context())
		if err != nil || conn.Write(r.Context(), typ, data) != nil {
			return
		}
	}
}

// openWebSocket opens a WebSocket to stagelet s1 through the gateway at
// gatewayURL, with the visitor's session cookie.
func openWebSocket(t *testing.T, gatewayURL string, cookie *http.Cookie) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{Host: host, HTTPHeader: http.Header{"Cookie": {SessionCookie + "=" + cookie.Value}}}
	conn, _, err := websocket.Dial(ctx, gatewayURL, opts) //nolint:bodyclose // websocket.Dial closes the response body itself
	if err != nil {
		t.Fatalf("open WebSocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func requireEcho(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("write to WebSocket: %v", err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "ping" {
		t.Fatalf("WebSocket echo = %q, %v; want ping", data, err)
	}
}

func requireClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("WebSocket read = %v, want the gateway to close the connection", err)
	}
}

// unansweredControlPlane admits visitors, but never answers a confirm: each
// call waits until its context ends.
type unansweredControlPlane struct {
	fakeControlPlane
	confirms chan struct{}
}

func (u *unansweredControlPlane) ConfirmSessions(ctx context.Context, _ []string) ([]string, error) {
	u.confirms <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUnansweredConfirmDoesNotHoldUpTheNextOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := &unansweredControlPlane{confirms: make(chan struct{})}
		cp.handoffs = map[string]protocol.HandoffResult{"good": {OK: true, SessionID: "sess1", StageletID: "s1", ExpiresAt: time.Now().Add(time.Hour)}}
		gw := New(cp, 50*time.Second, time.Now, slog.New(slog.DiscardHandler))
		gw.SetRoutes([]protocol.Route{{StageletID: "s1", Host: host, Upstream: "127.0.0.1:9"}})
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+protocol.HandoffPath+"?token=good", nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("handoff status = %d, want 303", rec.Code)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go gw.RunConfirmLoop(ctx, 20*time.Second)

		<-cp.confirms
		<-cp.confirms
	})
}
