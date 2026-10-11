// Package sshrun runs commands on a machine over SSH as root, trusting only
// the machine's pinned host key.
package sshrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target is a machine and the credentials to reach it.
type Target struct {
	Addr    string
	HostKey ssh.PublicKey
	Signer  ssh.Signer
}

// Run runs command and writes its output to stdout and stderr; nil discards
// it. Ending ctx closes the connection, which stops the command.
func (t Target) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, command string) error {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", t.Addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyCallback: ssh.FixedHostKey(t.HostKey),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SSH handshake with %s: %w", t.Addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session: %w", err)
	}
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Output runs command and returns its standard output, with the end of its
// standard error in the error.
func (t Target) Output(ctx context.Context, stdin io.Reader, command string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := t.Run(ctx, stdin, &stdout, &stderr, command); err != nil {
		text := strings.TrimSpace(stderr.String())
		if len(text) > 2000 {
			text = text[len(text)-2000:]
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, text)
	}
	return stdout.Bytes(), nil
}

// Quote quotes args for a POSIX shell, which is how SSH runs a command.
func Quote(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
