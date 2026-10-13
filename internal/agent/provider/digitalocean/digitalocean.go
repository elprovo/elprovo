// Package digitalocean runs preview machines as DigitalOcean droplets. The
// agent creates each droplet through the API and hands it a host key that the
// agent generated, so it never has to trust an unknown host key. It then
// drives the droplet over SSH as root with its own key.
package digitalocean

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

// Config says where and how the provider creates droplets. When VPCUUID is
// set, droplets join that VPC and the agent reaches them on their private
// address; otherwise it uses their public address.
type Config struct {
	APIURL string
	Token  string
	Region string
	Size   string
	// Image is a stock image; cloud-init installs Docker on it.
	Image string
	// GoldenImage, when set, is a Elprovo golden image that already has
	// Docker, Compose and BuildKit. It replaces Image.
	GoldenImage  string
	Tag          string
	VPCUUID      string
	HostKeyDir   string
	ReadyTimeout time.Duration
}

// Provider creates, finds, drives and deletes droplets.
type Provider struct {
	api       *api
	cfg       Config
	machines  sshrun.Machines
	pollEvery time.Duration

	keyMu         sync.Mutex
	keyRegistered bool
}

// registryCredentialsExpiry is how long registry credentials on a machine
// stay valid: long enough for a long build before its push.
const registryCredentialsExpiry = 2 * time.Hour

var (
	machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// New returns a provider that signs in to droplets with key.
func New(cfg Config, key ed25519.PrivateKey) (*Provider, error) {
	for name, value := range map[string]string{"token": cfg.Token, "region": cfg.Region, "size": cfg.Size, "image": cfg.Image, "tag": cfg.Tag, "host key directory": cfg.HostKeyDir} {
		if value == "" {
			return nil, fmt.Errorf("DigitalOcean provider needs a %s", name)
		}
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	p := &Provider{api: newAPI(cfg.APIURL, cfg.Token), cfg: cfg, machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second}
	p.machines.Address = p.Address
	return p, nil
}

// RegistryCredentials returns credentials for the account's container
// registry at host. They expire after registryCredentialsExpiry, can push
// only when push is set, and never carry the API token to a machine.
func (p *Provider) RegistryCredentials(ctx context.Context, host string, push bool) (string, string, error) {
	return p.api.dockerCredentials(ctx, host, push, registryCredentialsExpiry)
}

// Exists reports whether a droplet with the name exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	droplets, err := p.named(ctx, name)
	return len(droplets) > 0, err
}

// Create creates the droplet and waits until Docker Compose runs on it.
func (p *Provider) Create(ctx context.Context, name string) error {
	if !machineNamePattern.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	fingerprint, err := p.ensureSSHKey(ctx)
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
	userData, err := cloudinit.MachineConfig(hostKey, packages...)
	if err != nil {
		return err
	}
	if _, err := p.api.createDroplet(ctx, createDroplet{
		Name: name, Region: p.cfg.Region, Size: p.cfg.Size, Image: image,
		SSHKeys: []string{fingerprint}, Tags: []string{p.cfg.Tag}, UserData: userData, VPCUUID: p.cfg.VPCUUID,
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
			return fmt.Errorf("droplet %s is not ready after %v: %w", name, p.cfg.ReadyTimeout, err)
		case <-time.After(p.pollEvery):
		}
	}
}

// Address returns the droplet's private address inside a VPC, or its public
// address otherwise.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	droplets, err := p.named(ctx, name)
	if err != nil {
		return "", err
	}
	if len(droplets) == 0 {
		return "", fmt.Errorf("droplet %s: %w", name, errNotFound)
	}
	kind := "public"
	if p.cfg.VPCUUID != "" {
		kind = "private"
	}
	d := droplets[0]
	if addr := d.address(kind); d.Status == "active" && addr != "" {
		return addr, nil
	}
	return "", fmt.Errorf("droplet %s has no %s address yet (status %s)", name, kind, d.Status)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the names of the droplets with the provider's tag.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	droplets, err := p.api.dropletsByTag(ctx, p.cfg.Tag)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(droplets))
	for _, d := range droplets {
		names = append(names, d.Name)
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete deletes every droplet with the name and waits until DigitalOcean no
// longer has them. Deleting a missing droplet succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	droplets, err := p.named(ctx, name)
	if err != nil {
		return err
	}
	for _, d := range droplets {
		if err := p.api.deleteDroplet(ctx, d.ID); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	for _, d := range droplets {
		if err := p.waitGone(ctx, d.ID); err != nil {
			return fmt.Errorf("delete droplet %s: %w", name, err)
		}
	}
	if err := p.machines.Forget(name); err != nil {
		return err
	}
	return nil
}

func (p *Provider) waitGone(ctx context.Context, id int64) error {
	for {
		_, err := p.api.droplet(ctx, id)
		if errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("droplet %d still exists: %w", id, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

func (p *Provider) named(ctx context.Context, name string) ([]droplet, error) {
	tagged, err := p.api.dropletsByTag(ctx, p.cfg.Tag)
	if err != nil {
		return nil, err
	}
	var named []droplet
	for _, d := range tagged {
		if d.Name == name {
			named = append(named, d)
		}
	}
	return named, nil
}

// ensureSSHKey registers the agent's public key with DigitalOcean once and
// returns its fingerprint.
func (p *Provider) ensureSSHKey(ctx context.Context) (string, error) {
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
	fingerprint := ssh.FingerprintLegacyMD5(p.machines.Signer.PublicKey())
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keyRegistered {
		return fingerprint, nil
	}
	known, err := p.api.hasSSHKey(ctx, fingerprint)
	if err != nil {
		return "", err
	}
	if !known {
		if err := p.api.addSSHKey(ctx, "elprovo-agent "+fingerprint, public); err != nil {
			return "", err
		}
	}
	p.keyRegistered = true
	return fingerprint, nil
}
