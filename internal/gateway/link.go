package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/internal/controlplane"
	"github.com/stagely-dev/stagely/protocol"
)

// ErrDisconnected means the gateway has no session to the control plane.
var ErrDisconnected = errors.New("not connected to the control plane")

// Link carries the gateway's requests over the current control plane session
// and applies the route tables the control plane sends.
type Link struct {
	mu      sync.Mutex
	session *controlplane.Session
	pending map[string]chan protocol.Envelope
}

// Serve returns the function that controlplane.Run calls for each session.
// While the session lasts, it also reports route health to the control
// plane: right after each route table and then periodically.
func (l *Link) Serve(g *Gateway) func(context.Context, *controlplane.Session) error {
	return func(ctx context.Context, s *controlplane.Session) error {
		l.attach(s)
		defer l.detach()

		ctx, cancel := context.WithCancel(ctx)
		checkNow := make(chan struct{}, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := sendRouteHealth(ctx, s, g, checkNow); err != nil {
				g.log.Warn("report route health", "error", err)
				cancel()
			}
		})
		defer func() {
			cancel()
			wg.Wait()
		}()

		for {
			env, err := s.Receive(ctx)
			if err != nil {
				return err
			}
			if err := l.handle(ctx, s, g, env, checkNow); err != nil {
				return err
			}
		}
	}
}

func sendRouteHealth(ctx context.Context, s *controlplane.Session, g *Gateway, checkNow <-chan struct{}) error {
	ticker := time.NewTicker(routeHealthInterval)
	defer ticker.Stop()
	return g.reportRouteHealth(ctx, ticker.C, checkNow, func(ctx context.Context, h protocol.RouteHealth) error {
		return s.Send(ctx, protocol.TypeRouteHealth, h)
	})
}

func (l *Link) handle(ctx context.Context, s *controlplane.Session, g *Gateway, env protocol.Envelope, checkNow chan<- struct{}) error {
	switch env.Type {
	case protocol.TypeRoutes:
		var routes protocol.Routes
		if err := env.Into(&routes); err != nil {
			return err
		}
		applied := g.SetRoutes(routes.Routes)
		if err := s.Send(ctx, protocol.TypeRoutesApplied, protocol.RoutesApplied{StageletIDs: applied}); err != nil {
			return err
		}
		select {
		case checkNow <- struct{}{}:
		default:
		}
	case protocol.TypeHandoffResult:
		var res protocol.HandoffResult
		if err := env.Into(&res); err != nil {
			return err
		}
		l.deliver(res.RequestID, env)
	case protocol.TypeSessionConfirmed:
		var res protocol.SessionConfirmed
		if err := env.Into(&res); err != nil {
			return err
		}
		l.deliver(res.RequestID, env)
	default:
		g.log.Warn("ignoring unexpected message", "type", env.Type)
	}
	return nil
}

// ExchangeHandoff redeems a handoff token.
func (l *Link) ExchangeHandoff(ctx context.Context, token string) (protocol.HandoffResult, error) {
	var res protocol.HandoffResult
	err := l.request(ctx, protocol.TypeHandoffExchange, func(id string) any {
		return protocol.HandoffExchange{RequestID: id, Token: token}
	}, &res)
	return res, err
}

// ConfirmSessions returns the ids that are still valid. It fails when the
// control plane could not check them.
func (l *Link) ConfirmSessions(ctx context.Context, ids []string) ([]string, error) {
	var res protocol.SessionConfirmed
	err := l.request(ctx, protocol.TypeSessionConfirm, func(id string) any {
		return protocol.SessionConfirm{RequestID: id, SessionIDs: ids}
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("control plane could not confirm sessions: %s", res.Error)
	}
	return res.Valid, nil
}

func (l *Link) request(ctx context.Context, msgType string, build func(id string) any, reply any) error {
	id, err := newRequestID()
	if err != nil {
		return err
	}
	ch := make(chan protocol.Envelope, 1)
	l.mu.Lock()
	s := l.session
	if s != nil {
		l.pending[id] = ch
	}
	l.mu.Unlock()
	if s == nil {
		return ErrDisconnected
	}
	defer l.forget(id)

	if err := s.Send(ctx, msgType, build(id)); err != nil {
		return err
	}
	select {
	case env, ok := <-ch:
		if !ok {
			return ErrDisconnected
		}
		return env.Into(reply)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Link) attach(s *controlplane.Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.session = s
	l.pending = map[string]chan protocol.Envelope{}
}

func (l *Link) detach() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.session = nil
	for id, ch := range l.pending {
		close(ch)
		delete(l.pending, id)
	}
}

func (l *Link) deliver(id string, env protocol.Envelope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.pending[id]; ok {
		ch <- env
		delete(l.pending, id)
	}
}

func (l *Link) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, id)
}

func newRequestID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
