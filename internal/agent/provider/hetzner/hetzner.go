// Package hetzner runs preview machines as Hetzner Cloud servers. The agent
// creates each server through the API with a label it owns and hands it a
// host key that the agent generated, so it never has to trust an unknown
// host key. Every server joins the environment's private network, and the
// agent drives it over SSH as root with its own key, on its private address.
package hetzner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/elprovo/elprovo/internal/cloudinit"
	"github.com/elprovo/elprovo/internal/sshrun"
)

// Config says where and how the provider creates servers.
type Config struct {
	// APIURL is the API base URL with its /v1 path.
	APIURL     string
	Token      string
	Location   string
	ServerType string
	// Image is a stock image; cloud-init installs Docker on it.
	Image string
	// GoldenImage, when set, is the id of an Elprovo golden image snapshot
	// that already has Docker, Compose and BuildKit. It replaces Image.
	GoldenImage string
	// Label is key=value. Every server the provider creates has it, and the
	// provider sees only the servers that have it.
	Label string
	// NetworkID is the id of the private network that every server joins.
	NetworkID int64
	// Network is that network's range and the addresses on it, the agent's
	// and the gateway's, that may open connections to a server. Hetzner's
	// firewalls do not filter private networks, so each server enforces
	// this with its own firewall rules.
	Network      cloudinit.PrivateNetwork
	HostKeyDir   string
	ReadyTimeout time.Duration
}

// Provider creates, finds, drives and deletes servers.
type Provider struct {
	api       *api
	cfg       Config
	labels    map[string]string
	selector  string
	machines  sshrun.Machines
	pollEvery time.Duration

	keyMu sync.Mutex
	keyID int64
}

var (
	machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	// labelPattern is a Hetzner label as key=value: an optional DNS
	// subdomain prefix and a slash, then the key's name, and a value.
	labelPattern = regexp.MustCompile(`^((?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?/)?` +
		`([a-zA-Z0-9](?:[-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9])?)=([a-zA-Z0-9](?:[-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9])?)$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// New returns a provider that signs in to servers with sshKey.
func New(cfg Config, sshKey ed25519.PrivateKey) (*Provider, error) {
	for name, value := range map[string]string{"token": cfg.Token, "location": cfg.Location, "server type": cfg.ServerType, "image": cfg.Image, "label": cfg.Label, "host key directory": cfg.HostKeyDir} {
		if value == "" {
			return nil, fmt.Errorf("the Hetzner provider needs a %s", name)
		}
	}
	if cfg.NetworkID == 0 || !cfg.Network.Range.IsValid() || len(cfg.Network.Sources) == 0 {
		return nil, errors.New("the Hetzner provider needs a private network, its range and the addresses that may connect to servers on it")
	}
	if !labelPattern.MatchString(cfg.Label) {
		return nil, fmt.Errorf("the Hetzner label %q is not key=value with a valid label key and value", cfg.Label)
	}
	key, value, _ := strings.Cut(cfg.Label, "=")
	signer, err := ssh.NewSignerFromKey(sshKey)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	p := &Provider{
		api: newAPI(cfg.APIURL, cfg.Token), cfg: cfg, labels: map[string]string{key: value}, selector: key + "==" + value,
		machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second,
	}
	p.machines.Address = p.Address
	return p, nil
}

// Exists reports whether a server with the name and the provider's label
// exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	servers, err := p.api.servers(ctx, p.selector, name)
	return len(servers) > 0, err
}

// Create creates the server and waits until Docker Compose runs on it.
func (p *Provider) Create(ctx context.Context, name string) error {
	if !machineNamePattern.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	keyID, err := p.ensureSSHKey(ctx)
	if err != nil {
		return err
	}
	hostKey, err := p.machines.NewHostKey(name)
	if err != nil {
		return err
	}
	image, packages := p.cfg.Image, machinePackages
	if p.cfg.GoldenImage != "" {
		image, packages = p.cfg.GoldenImage, nil
	}
	userData, err := cloudinit.MachineConfigOnNetwork(hostKey, p.cfg.Network, packages...)
	if err != nil {
		return err
	}
	if err := p.api.createServer(ctx, createServer{
		Name: name, ServerType: p.cfg.ServerType, Image: image, Location: p.cfg.Location, SSHKeys: []int64{keyID},
		UserData: userData, Labels: p.labels, Networks: []int64{p.cfg.NetworkID}, PublicNet: publicNet{EnableIPv4: true},
	}); err != nil {
		return err
	}
	return p.waitReady(ctx, name)
}

func (p *Provider) waitReady(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	for {
		_, err := p.machines.Output(ctx, name, nil, "sh", "-c", "cloud-init status --wait >/dev/null; docker compose version")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server %s is not ready after %v: %w", name, p.cfg.ReadyTimeout, err)
		case <-time.After(p.pollEvery):
		}
	}
}

// Address returns the server's address on the private network once it runs.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	servers, err := p.api.servers(ctx, p.selector, name)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "", fmt.Errorf("server %s: %w", name, errNotFound)
	}
	s := servers[0]
	if addr := s.privateIP(p.cfg.NetworkID); s.Status == "running" && addr != "" {
		return addr, nil
	}
	return "", fmt.Errorf("server %s has no private address yet (status %s)", name, s.Status)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the names of the servers with the provider's label.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	servers, err := p.api.servers(ctx, p.selector, "")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete deletes the server with the name and the provider's label, and
// waits until Hetzner no longer has it. Deleting a missing server succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	servers, err := p.api.servers(ctx, p.selector, name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	for _, s := range servers {
		if err := p.deleteWhenUnlocked(ctx, s.ID); err != nil {
			return err
		}
	}
	for _, s := range servers {
		if err := p.waitGone(ctx, s.ID); err != nil {
			return fmt.Errorf("delete server %s: %w", name, err)
		}
	}
	if err := p.machines.Forget(name); err != nil {
		return err
	}
	return nil
}

// deleteWhenUnlocked deletes the server, waiting while an action of the
// server, such as its creation, still locks it.
func (p *Provider) deleteWhenUnlocked(ctx context.Context, id int64) error {
	for {
		err := p.api.deleteServer(ctx, id)
		if !errors.Is(err, errLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server %d stays locked: %w", id, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

func (p *Provider) waitGone(ctx context.Context, id int64) error {
	for {
		_, err := p.api.server(ctx, id)
		if errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server %d still exists: %w", id, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

// ensureSSHKey registers the agent's public key with Hetzner once and
// returns its id.
func (p *Provider) ensureSSHKey(ctx context.Context) (int64, error) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keyID != 0 {
		return p.keyID, nil
	}
	fingerprint := ssh.FingerprintLegacyMD5(p.machines.Signer.PublicKey())
	id, err := p.api.sshKeyID(ctx, fingerprint)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
		if id, err = p.api.addSSHKey(ctx, "elprovo-agent-"+strings.ReplaceAll(fingerprint, ":", ""), public); err != nil {
			return 0, err
		}
	}
	p.keyID = id
	return id, nil
}
