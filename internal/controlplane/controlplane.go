// Package controlplane holds the session that agents and gateways open to the
// Elprovo control plane: key management, enrollment, the signed handshake,
// and reconnects.
package controlplane

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/coder/websocket"
	"github.com/elprovo/elprovo/protocol"
)

// Config says how to reach the control plane and who the client is.
type Config struct {
	URL             string
	Role            protocol.Role
	KeyPath         string
	EnrollmentToken string
}

// Session is one authenticated connection. Send may be called concurrently;
// Receive must be called from one goroutine.
type Session struct {
	conn    *websocket.Conn
	Welcome protocol.Welcome
}

// ErrRejected means the control plane refused the client. Retrying with the
// same key and token will not help.
var ErrRejected = errors.New("control plane rejected the client")

// Dial opens and authenticates a session.
func Dial(ctx context.Context, cfg Config) (*Session, error) {
	key, err := LoadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial control plane: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	s := &Session{conn: conn}
	if err := s.handshake(ctx, cfg, key); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return nil, err
	}
	return s, nil
}

func (s *Session) handshake(ctx context.Context, cfg Config, key ed25519.PrivateKey) error {
	env, err := s.Receive(ctx)
	if err != nil {
		return err
	}
	var challenge protocol.Challenge
	if env.Type != protocol.TypeChallenge {
		return fmt.Errorf("handshake: got %q, want challenge", env.Type)
	}
	if err := env.Into(&challenge); err != nil {
		return err
	}
	auth := protocol.Auth{
		Role:            cfg.Role,
		ProtocolVersion: protocol.Version,
		PublicKey:       key.Public().(ed25519.PublicKey),
		Signature:       ed25519.Sign(key, append([]byte(protocol.SignaturePrefix), challenge.Nonce...)),
		EnrollmentToken: cfg.EnrollmentToken,
	}
	if err := s.Send(ctx, protocol.TypeAuth, auth); err != nil {
		return err
	}
	env, err = s.Receive(ctx)
	if err != nil {
		return err
	}
	switch env.Type {
	case protocol.TypeWelcome:
		return env.Into(&s.Welcome)
	case protocol.TypeError:
		var e protocol.Error
		if err := env.Into(&e); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s", ErrRejected, e.Message)
	default:
		return fmt.Errorf("handshake: got %q, want welcome", env.Type)
	}
}

// Send writes one message.
func (s *Session) Send(ctx context.Context, msgType string, payload any) error {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		return err
	}
	if err := s.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("send %s: %w", msgType, err)
	}
	return nil
}

// Receive reads the next message.
func (s *Session) Receive(ctx context.Context) (protocol.Envelope, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("receive: %w", err)
	}
	return protocol.Decode(data)
}

// Close ends the session.
func (s *Session) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// Run keeps a session open until ctx ends. It calls serve with each new
// session and reconnects with backoff when serve returns or the dial fails.
// It returns early only when the control plane rejects the client.
func Run(ctx context.Context, cfg Config, log *slog.Logger, serve func(context.Context, *Session) error) error {
	backoff := time.Second
	for {
		s, err := Dial(ctx, cfg)
		if errors.Is(err, ErrRejected) {
			return err
		}
		if err == nil {
			log.Info("connected to control plane", "client_id", s.Welcome.ClientID, "environment_id", s.Welcome.EnvironmentID)
			backoff = time.Second
			err = serve(ctx, s)
			_ = s.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		wait := backoff + rand.N(backoff/2)
		log.Warn("control plane session ended, reconnecting", "error", err, "wait", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}
