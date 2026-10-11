package sshrun

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// startServer runs an SSH server that accepts client and answers every
// command with "ran <command>: <stdin>".
func startServer(t *testing.T, client ssh.PublicKey) (string, ssh.PublicKey) {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(client.Marshal()) {
			return nil, io.EOF
		}
		return nil, nil
	}}
	cfg.AddHostKey(hostSigner)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serve(conn, cfg)
		}
	}()
	return l.Addr().String(), hostSigner.PublicKey()
}

func serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				command := string(req.Payload[4:])
				input, _ := io.ReadAll(ch)
				_, _ = io.WriteString(ch, "ran "+command+": "+string(input))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
			}
		}()
	}
}

func TestOutputRunsTheCommandWithStdin(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	addr, hostKey := startServer(t, signer.PublicKey())

	out, err := Target{Addr: addr, HostKey: hostKey, Signer: signer}.Output(context.Background(), strings.NewReader("input"), Quote("echo", "it's"))

	if err != nil || string(out) != `ran 'echo' 'it'\''s': input` {
		t.Fatalf("Output = %q, %v", out, err)
	}
}

func TestAnotherHostKeyIsRefused(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	addr, _ := startServer(t, signer.PublicKey())
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)

	_, err := Target{Addr: addr, HostKey: otherSigner.PublicKey(), Signer: signer}.Output(context.Background(), nil, "true")

	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err = %v, want a failed handshake", err)
	}
}
