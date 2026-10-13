// Package gcp runs preview machines as Compute Engine instances. The agent
// signs in with a service account key, creates each instance through the
// Compute Engine API and hands it a host key that the agent generated, so it
// never has to trust an unknown host key. It then drives the instance over
// SSH as root with its own key, on the instance's internal address.
package gcp

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/elprovo/elprovo/internal/cloudinit"
	"github.com/elprovo/elprovo/internal/sshrun"
)

// ownerLabel is the instance label that holds the provider's tag. The agent
// lists, and deletes, only instances that carry it.
const ownerLabel = "elprovo"

// metadataBlock rejects new connections from containers to the metadata
// server, which serves the user data and so the host key, on every boot
// before Docker starts. Compute Engine's DNS server has the same address,
// so DNS stays open: containers on Docker's default bridge, such as build
// steps, resolve names through it.
const metadataBlock = "bootcmd:\n" +
	"  - iptables -N DOCKER-USER 2>/dev/null || true\n" +
	"  - iptables -C " + metadataReject + " 2>/dev/null || iptables -I " + metadataReject + "\n" +
	"  - iptables -C " + dnsReturnUDP + " 2>/dev/null || iptables -I " + dnsReturnUDP + "\n" +
	"  - iptables -C " + dnsReturnTCP + " 2>/dev/null || iptables -I " + dnsReturnTCP + "\n"

const (
	metadataReject = "DOCKER-USER -d 169.254.169.254 -m conntrack --ctstate NEW -j REJECT"
	dnsReturnUDP   = "DOCKER-USER -d 169.254.169.254 -p udp --dport 53 -j RETURN"
	dnsReturnTCP   = "DOCKER-USER -d 169.254.169.254 -p tcp --dport 53 -j RETURN"
)

// guestAgentConfig keeps the Compute Engine guest agent from replacing the
// host key from the user data with keys of its own on the first boot.
// Cloud-init writes the file before the guest agent starts.
const guestAgentConfig = "write_files:\n" +
	"  - path: /etc/default/instance_configs.cfg\n" +
	"    content: |\n" +
	"      [InstanceSetup]\n" +
	"      set_host_keys = false\n"

// Config says where and how the provider creates instances.
type Config struct {
	// ComputeURL is the Compute Engine API base URL, ending in /compute/v1/.
	ComputeURL string
	// Credentials is a service account key file, as JSON or base64-encoded
	// JSON. Its token_uri is where the provider gets access tokens.
	Credentials []byte
	// Project defaults to the project of the service account key.
	Project     string
	Zone        string
	MachineType string
	// Image is a stock image; cloud-init installs Docker on it.
	Image string
	// GoldenImage, when set, is a Elprovo golden image that already has
	// Docker, Compose and BuildKit. It replaces Image. A bare name means an
	// image of the project.
	GoldenImage string
	// Subnetwork is the subnetwork the instances join, such as
	// projects/acme/regions/us-central1/subnetworks/elprovo. Empty means the
	// project's default network.
	Subnetwork string
	// Tag is the network tag that firewall rules target, and the value of
	// the elprovo label that marks the provider's instances.
	Tag string
	// IAMCredentialsURL is the IAM Service Account Credentials API base URL,
	// ending in /v1/.
	IAMCredentialsURL string
	// RegistryPushAccount and RegistryPullAccount are service accounts that
	// may only push to and pull from the image repository. The provider's
	// service account impersonates them to sign machines in to the registry.
	RegistryPushAccount string
	RegistryPullAccount string
	HostKeyDir          string
	ReadyTimeout        time.Duration
}

// Provider creates, finds, drives and deletes Compute Engine instances.
type Provider struct {
	api       *api
	cfg       Config
	machines  sshrun.Machines
	pollEvery time.Duration
}

var (
	instanceNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	tagPattern          = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// New returns a provider that signs in to instances with key.
func New(cfg Config, key ed25519.PrivateKey) (*Provider, error) {
	sa, err := parseKey(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	if cfg.Project == "" {
		cfg.Project = sa.ProjectID
	}
	for name, value := range map[string]string{"project": cfg.Project, "zone": cfg.Zone, "machine type": cfg.MachineType, "image": cfg.Image, "host key directory": cfg.HostKeyDir} {
		if value == "" {
			return nil, fmt.Errorf("google cloud provider needs a %s", name)
		}
	}
	if !tagPattern.MatchString(cfg.Tag) {
		return nil, fmt.Errorf("tag %q must be lowercase letters, digits and dashes, starting with a letter", cfg.Tag)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	a := &api{
		baseURL: strings.TrimRight(cfg.ComputeURL, "/") + "/", iamCredentialsURL: strings.TrimRight(cfg.IAMCredentialsURL, "/") + "/",
		project: cfg.Project, zone: cfg.Zone, http: client, tokens: &tokenSource{key: sa, http: client, now: time.Now},
	}
	p := &Provider{api: a, cfg: cfg, machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second}
	p.machines.Address = p.Address
	return p, nil
}

// registryTokenLifetime is how long a machine's registry sign-in lasts: the
// longest an access token from impersonation may live by default.
const registryTokenLifetime = time.Hour

// RegistryCredentials signs a machine in to the Artifact Registry host with
// an access token of the registry push or pull service account, so the
// provider's key never reaches a machine and the token can't do more than
// push or pull images for an hour.
func (p *Provider) RegistryCredentials(ctx context.Context, _ string, push bool) (string, string, error) {
	account := p.cfg.RegistryPullAccount
	if push {
		account = p.cfg.RegistryPushAccount
	}
	if account == "" {
		return "", "", errors.New("no registry service account to sign machines in with; set the registry push and pull accounts")
	}
	token, err := p.api.accessToken(ctx, account, registryTokenLifetime)
	if err != nil {
		return "", "", err
	}
	return "oauth2accesstoken", token, nil
}

// Exists reports whether the provider's instance with the name exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	_, found, err := p.owned(ctx, name)
	return found, err
}

// Create creates the instance and waits until Docker Compose runs on it.
func (p *Provider) Create(ctx context.Context, name string) error {
	if !instanceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	hostKey, err := p.machines.NewHostKey(name)
	if err != nil {
		return err
	}
	image, packages := p.cfg.Image, machinePackages
	if p.cfg.GoldenImage != "" {
		image, packages = p.cfg.GoldenImage, nil
		if !strings.Contains(image, "/") {
			image = "projects/" + p.cfg.Project + "/global/images/" + image
		}
	}
	userData, _, err := cloudinit.HostKeyConfig(hostKey, packages...)
	if err != nil {
		return err
	}
	if err := p.insert(ctx, p.instanceRequest(name, image, userData+metadataBlock+guestAgentConfig)); err != nil {
		return fmt.Errorf("create instance %s: %w", name, err)
	}
	return p.waitReady(ctx, name)
}

// insert creates the instance and waits until its operation is done.
func (p *Provider) insert(ctx context.Context, req insertInstance) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	op, err := p.api.insertInstance(ctx, req)
	if err != nil {
		return err
	}
	return p.api.wait(ctx, op, p.pollEvery)
}

func (p *Provider) instanceRequest(name, image, userData string) insertInstance {
	nic := insertInterface{Subnetwork: p.cfg.Subnetwork, AccessConfigs: []map[string]string{{"type": "ONE_TO_ONE_NAT", "name": "External NAT"}}}
	if nic.Subnetwork == "" {
		nic.Network = "global/networks/default"
	}
	login := "root:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey()))) + " elprovo-agent"
	return insertInstance{
		Name:              name,
		MachineType:       "zones/" + p.cfg.Zone + "/machineTypes/" + p.cfg.MachineType,
		Disks:             []insertDisk{{Boot: true, AutoDelete: true, InitializeParams: map[string]string{"sourceImage": image}}},
		NetworkInterfaces: []insertInterface{nic},
		Metadata: metadata{Items: []metadataItem{
			{Key: "user-data", Value: userData}, {Key: "ssh-keys", Value: login}, {Key: "block-project-ssh-keys", Value: "TRUE"},
		}},
		Tags:   tags{Items: []string{p.cfg.Tag}},
		Labels: map[string]string{ownerLabel: p.cfg.Tag},
	}
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

// Address returns the instance's internal address.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	inst, found, err := p.owned(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("instance %s: %w", name, errNotFound)
	}
	if addr := inst.internalAddress(); inst.Status == "RUNNING" && addr != "" {
		return addr, nil
	}
	return "", fmt.Errorf("instance %s has no internal address yet (status %s)", name, inst.Status)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the names of the instances with the provider's label.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	instances, err := p.api.instancesLabeled(ctx, ownerLabel, p.cfg.Tag)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(instances))
	for _, inst := range instances {
		names = append(names, inst.Name)
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete deletes the provider's instance with the name and waits until
// Compute Engine no longer has it. Deleting a missing instance succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	_, found, err := p.owned(ctx, name)
	if err != nil {
		return err
	}
	if found {
		op, err := p.api.deleteInstance(ctx, name)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
		defer cancel()
		if op != nil {
			if err := p.api.wait(ctx, *op, p.pollEvery); err != nil {
				return fmt.Errorf("delete instance %s: %w", name, err)
			}
		}
		if err := p.waitGone(ctx, name); err != nil {
			return fmt.Errorf("delete instance %s: %w", name, err)
		}
	}
	if err := p.machines.Forget(name); err != nil {
		return err
	}
	return nil
}

func (p *Provider) waitGone(ctx context.Context, name string) error {
	for {
		_, err := p.api.instance(ctx, name)
		if errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance %s still exists: %w", name, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

// owned returns the instance with the name when it carries the provider's
// label. Instances of the same name that the provider did not create count
// as missing, so the agent never drives or deletes them.
func (p *Provider) owned(ctx context.Context, name string) (instance, bool, error) {
	if !instanceNamePattern.MatchString(name) {
		return instance{}, false, fmt.Errorf("invalid machine name %q", name)
	}
	inst, err := p.api.instance(ctx, name)
	if errors.Is(err, errNotFound) {
		return instance{}, false, nil
	}
	if err != nil {
		return instance{}, false, fmt.Errorf("get instance %s: %w", name, err)
	}
	return inst, inst.Labels[ownerLabel] == p.cfg.Tag, nil
}
