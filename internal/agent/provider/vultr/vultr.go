// Package vultr runs preview machines as Vultr instances. The agent creates
// each instance through the API and hands it a host key that the agent
// generated, so it never has to trust an unknown host key. Every instance
// joins the environment's VPC, and the agent drives it over SSH as root with
// its own key, on its private address.
package vultr

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/elprovo/elprovo/internal/cloudinit"
	"github.com/elprovo/elprovo/internal/sshrun"
)

// Config says where and how the provider creates instances. Instances join
// the VPC VPCID, and the agent and the gateway reach them there.
// FirewallGroupID puts every instance in that firewall group, which filters
// the public interface only.
type Config struct {
	APIURL string
	APIKey string
	Region string
	Plan   string
	// OSID is a stock operating system; cloud-init installs Docker on it.
	OSID int
	// GoldenImage, when set, is the snapshot id of an Elprovo golden image
	// that already has Docker, Compose and BuildKit. It replaces OSID.
	GoldenImage     string
	Tag             string
	FirewallGroupID string
	VPCID           string
	// Network is the VPC's range and the addresses in it, the agent's and
	// the gateway's, that may open connections to an instance. Vultr's
	// firewall does not filter VPC traffic, so each instance enforces this
	// with its own firewall rules.
	Network cloudinit.PrivateNetwork
	// RegistryID is the id of the account's container registry that
	// RegistryCredentials hands out credentials for.
	RegistryID   string
	HostKeyDir   string
	ReadyTimeout time.Duration
}

// Provider creates, finds, drives and deletes instances.
type Provider struct {
	api       *api
	cfg       Config
	machines  sshrun.Machines
	pollEvery time.Duration

	keyMu sync.Mutex
	keyID string
}

var (
	machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// registryCredentialsExpiry is how long registry credentials on a machine
// stay valid: long enough for a long build before its push.
const registryCredentialsExpiry = 2 * time.Hour

// New returns a provider that signs in to instances with key.
func New(cfg Config, key ed25519.PrivateKey) (*Provider, error) {
	for name, value := range map[string]string{"API key": cfg.APIKey, "region": cfg.Region, "plan": cfg.Plan, "tag": cfg.Tag, "host key directory": cfg.HostKeyDir} {
		if value == "" {
			return nil, fmt.Errorf("the Vultr provider needs a %s", name)
		}
	}
	if cfg.VPCID == "" || !cfg.Network.Range.IsValid() || len(cfg.Network.Sources) == 0 {
		return nil, errors.New("the Vultr provider needs a VPC, its range and the addresses that may connect to instances in it")
	}
	if cfg.OSID == 0 && cfg.GoldenImage == "" {
		return nil, errors.New("the Vultr provider needs an operating system id or a golden image")
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	p := &Provider{api: newAPI(cfg.APIURL, cfg.APIKey), cfg: cfg, machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second}
	p.machines.Address = p.Address
	return p, nil
}

// RegistryCredentials returns credentials for the container registry
// RegistryID at host. They expire after registryCredentialsExpiry, can push
// only when push is set, and reach only that registry, so neither the API key
// nor the registry's root user goes to a machine.
func (p *Provider) RegistryCredentials(ctx context.Context, host string, push bool) (string, string, error) {
	return p.api.dockerCredentials(ctx, p.cfg.RegistryID, host, push, registryCredentialsExpiry)
}

// Exists reports whether an instance with the name exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	instances, err := p.named(ctx, name)
	return len(instances) > 0, err
}

// Create creates the instance and waits until Docker Compose runs on it.
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
	req := createInstance{
		Region: p.cfg.Region, Plan: p.cfg.Plan, OSID: p.cfg.OSID, Label: name, Hostname: name, Tags: []string{p.cfg.Tag},
		SSHKeyIDs: []string{keyID}, FirewallGroupID: p.cfg.FirewallGroupID, AttachVPC: []string{p.cfg.VPCID},
	}
	packages := machinePackages
	if p.cfg.GoldenImage != "" {
		req.OSID, req.SnapshotID, packages = 0, p.cfg.GoldenImage, nil
	}
	userData, err := cloudinit.MachineConfigOnNetwork(hostKey, p.cfg.Network, packages...)
	if err != nil {
		return err
	}
	req.UserData = base64.StdEncoding.EncodeToString([]byte(userData))
	if _, err := p.api.createInstance(ctx, req); err != nil {
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
			return fmt.Errorf("instance %s is not ready after %v: %w", name, p.cfg.ReadyTimeout, err)
		case <-time.After(p.pollEvery):
		}
	}
}

// Address returns the instance's address in the VPC.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	instances, err := p.named(ctx, name)
	if err != nil {
		return "", err
	}
	if len(instances) == 0 {
		return "", fmt.Errorf("instance %s: %w", name, errNotFound)
	}
	i := instances[0]
	if i.Status == "active" && i.PowerStatus == "running" && i.InternalIP != "" {
		return i.InternalIP, nil
	}
	return "", fmt.Errorf("instance %s has no private address yet (status %s, power %s)", name, i.Status, i.PowerStatus)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the labels of the instances with the provider's tag.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	instances, err := p.api.instances(ctx, "")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, i := range instances {
		if slices.Contains(i.Tags, p.cfg.Tag) {
			names = append(names, i.Label)
		}
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete deletes every instance with the name and waits until Vultr no
// longer has them. Deleting a missing instance succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	instances, err := p.named(ctx, name)
	if err != nil {
		return err
	}
	for _, i := range instances {
		if err := p.api.deleteInstance(ctx, i.ID); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	for _, i := range instances {
		if err := p.waitGone(ctx, i.ID); err != nil {
			return fmt.Errorf("delete instance %s: %w", name, err)
		}
	}
	if err := p.machines.Forget(name); err != nil {
		return err
	}
	return nil
}

// waitGone waits until GET answers 404 for the instance. Vultr answers
// otherwise while it is still deleting the instance.
func (p *Provider) waitGone(ctx context.Context, id string) error {
	for {
		_, err := p.api.instance(ctx, id)
		if errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance %s still exists: %w", id, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

// named returns the instances that carry the name as their label and the
// provider's tag, so the agent never touches another instance.
func (p *Provider) named(ctx context.Context, name string) ([]instance, error) {
	labelled, err := p.api.instances(ctx, name)
	if err != nil {
		return nil, err
	}
	var named []instance
	for _, i := range labelled {
		if i.Label == name && slices.Contains(i.Tags, p.cfg.Tag) {
			named = append(named, i)
		}
	}
	return named, nil
}

// ensureSSHKey finds the agent's public key among the account's SSH keys,
// or adds it once, and returns its id.
func (p *Provider) ensureSSHKey(ctx context.Context) (string, error) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keyID != "" {
		return p.keyID, nil
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
	keys, err := p.api.sshKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if sameKey(k.SSHKey, public) {
			p.keyID = k.ID
			return k.ID, nil
		}
	}
	id, err := p.api.addSSHKey(ctx, "elprovo-agent "+ssh.FingerprintSHA256(p.machines.Signer.PublicKey()), public)
	if err != nil {
		return "", err
	}
	p.keyID = id
	return id, nil
}

// sameKey compares two authorized keys by type and key data, ignoring
// comments.
func sameKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}
