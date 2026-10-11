package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stagely-dev/stagely/internal/controlplane"
	"github.com/stagely-dev/stagely/protocol"
)

func fakeSessionSendingRoutes(t *testing.T, routes protocol.Routes, received chan<- protocol.Envelope) string {
	t.Helper()
	return fakeSession(t, routes, func(_ context.Context, _ *websocket.Conn, env protocol.Envelope) { received <- env })
}

// fakeSession serves gateway sessions like the control plane: it completes
// the handshake, sends routes and passes each message from the gateway to
// answer, which may reply on conn.
func fakeSession(t *testing.T, routes protocol.Routes, answer func(ctx context.Context, conn *websocket.Conn, env protocol.Envelope)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if writeMessage(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: []byte("nonce")}) != nil {
			return
		}
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		if writeMessage(ctx, conn, protocol.TypeWelcome, protocol.Welcome{ClientID: "gw1", EnvironmentID: "env1"}) != nil {
			return
		}
		if writeMessage(ctx, conn, protocol.TypeRoutes, routes) != nil {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env, err := protocol.Decode(data)
			if err != nil {
				return
			}
			answer(ctx, conn, env)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func writeMessage(ctx context.Context, conn *websocket.Conn, msgType string, payload any) error {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func nextEnvelope(t *testing.T, received <-chan protocol.Envelope) protocol.Envelope {
	t.Helper()
	select {
	case env := <-received:
		return env
	case <-time.After(5 * time.Second):
		t.Fatal("no message from the gateway")
		return protocol.Envelope{}
	}
}

func TestSessionReportsRouteHealthAfterApplyingRoutes(t *testing.T) {
	up := addr(startUpstream(t))
	received := make(chan protocol.Envelope, 10)
	url := fakeSessionSendingRoutes(t, protocol.Routes{Routes: []protocol.Route{{StageletID: "s1", Host: host, Upstream: up}}}, received)
	startLink(t, url)

	if env := nextEnvelope(t, received); env.Type != protocol.TypeRoutesApplied {
		t.Fatalf("first message = %s, want %s", env.Type, protocol.TypeRoutesApplied)
	}
	env := nextEnvelope(t, received)
	if env.Type != protocol.TypeRouteHealth {
		t.Fatalf("second message = %s, want %s", env.Type, protocol.TypeRouteHealth)
	}
	var health protocol.RouteHealth
	if err := env.Into(&health); err != nil {
		t.Fatalf("decode route health: %v", err)
	}
	if len(health.Routes) != 1 {
		t.Fatalf("route health = %+v, want one route", health.Routes)
	}
	if got := health.Routes[0]; got.StageletID != "s1" || got.Upstream != up || !got.Reachable || !got.CheckedAt.Equal(checkedAt) {
		t.Fatalf("route state = %+v, want s1 reachable on %s at %s", got, up, checkedAt)
	}
}

// startLink connects a gateway to the control plane at url and serves the
// session until the test ends.
func startLink(t *testing.T, url string) *Link {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	s, err := controlplane.Dial(ctx, controlplane.Config{URL: url, Role: protocol.RoleGateway, KeyPath: filepath.Join(t.TempDir(), "gateway.key")})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	link := &Link{}
	gw := New(link, 50*time.Second, func() time.Time { return checkedAt }, slog.New(slog.DiscardHandler))
	serveCtx, stopServe := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- link.Serve(gw)(serveCtx, s) }()
	t.Cleanup(func() {
		stopServe()
		<-served
		_ = s.Close()
	})
	return link
}

func TestConfirmFailsWhenTheControlPlaneCouldNotCheckSessions(t *testing.T) {
	received := make(chan protocol.Envelope, 10)
	url := fakeSession(t, protocol.Routes{}, func(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) {
		var confirm protocol.SessionConfirm
		if env.Type != protocol.TypeSessionConfirm || env.Into(&confirm) != nil {
			received <- env
			return
		}
		_ = writeMessage(ctx, conn, protocol.TypeSessionConfirmed, protocol.SessionConfirmed{RequestID: confirm.RequestID, Error: "internal error"})
	})
	link := startLink(t, url)
	if env := nextEnvelope(t, received); env.Type != protocol.TypeRoutesApplied {
		t.Fatalf("first message = %s, want %s", env.Type, protocol.TypeRoutesApplied)
	}

	valid, err := link.ConfirmSessions(t.Context(), []string{"sess1"})

	if err == nil {
		t.Fatalf("ConfirmSessions = %v, nil; want an error, so the gateway keeps the sessions unconfirmed instead of revoking them", valid)
	}
}
