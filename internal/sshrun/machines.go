package sshrun

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrNoHostKey means the agent pinned no host key for the machine, so it
// did not create it or forgot it.
var ErrNoHostKey = errors.New("no pinned host key for machine")

// namePattern keeps machine names safe as file names in HostKeyDir.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// Machines drives the machines a cloud provider creates. It makes each
// machine's host key for its user data and pins the public half before the
// machine boots, then runs commands on the machine as Signer only when it
// presents that key. Address returns the address a machine is reached at.
type Machines struct {
	Signer     ssh.Signer
	Port       string
	HostKeyDir string
	Address    func(ctx context.Context, name string) (string, error)
}

// NewHostKey makes an Ed25519 host key for the machine, pins its public half
// and returns the private key in OpenSSH PEM form, for the machine's user
// data.
func (m Machines) NewHostKey(name string) (string, error) {
	path, err := m.hostKeyPath(name)
	if err != nil {
		return "", err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return "", fmt.Errorf("encode host key: %w", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return "", fmt.Errorf("encode host key: %w", err)
	}
	if err := os.MkdirAll(m.HostKeyDir, 0o700); err != nil {
		return "", fmt.Errorf("create host key directory: %w", err)
	}
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(sshPublic), 0o600); err != nil {
		return "", fmt.Errorf("pin host key of %s: %w", name, err)
	}
	return string(pem.EncodeToMemory(block)), nil
}

// Forget removes the machine's pinned host key. Forgetting a machine with
// no pinned key succeeds.
func (m Machines) Forget(name string) error {
	path, err := m.hostKeyPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("forget host key of %s: %w", name, err)
	}
	return nil
}

// Run runs a command on the machine and writes its output to stdout and
// stderr; nil discards it.
func (m Machines) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	addr, err := m.Address(ctx, name)
	if err != nil {
		return err
	}
	hostKey, err := m.pinnedHostKey(name)
	if err != nil {
		return err
	}
	target := Target{Addr: net.JoinHostPort(addr, m.Port), HostKey: hostKey, Signer: m.Signer}
	if err := target.Run(ctx, stdin, stdout, stderr, Quote(args...)); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	return nil
}

// Output runs a command on the machine and returns its standard output,
// with the end of its standard error in the error.
func (m Machines) Output(ctx context.Context, name string, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := m.Run(ctx, name, stdin, &stdout, &stderr, args...); err != nil {
		text := strings.TrimSpace(stderr.String())
		if len(text) > 2000 {
			text = text[len(text)-2000:]
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, text)
	}
	return stdout.Bytes(), nil
}

// CopyIn copies a file from the agent host to remotePath on the machine.
func (m Machines) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()
	_, err = m.Output(ctx, name, f, "sh", "-c", `cat > "$1"`, "sh", remotePath)
	return err
}

func (m Machines) hostKeyPath(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid machine name %q", name)
	}
	return filepath.Join(m.HostKeyDir, name+".pub"), nil
}

func (m Machines) pinnedHostKey(name string) (ssh.PublicKey, error) {
	path, err := m.hostKeyPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w %s", ErrNoHostKey, name)
	}
	if err != nil {
		return nil, fmt.Errorf("read host key of %s: %w", name, err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse host key of %s: %w", name, err)
	}
	return key, nil
}
