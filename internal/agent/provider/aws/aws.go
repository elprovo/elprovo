// Package aws runs preview machines as Amazon EC2 instances. The agent
// launches each instance through the EC2 API and hands it a host key that
// the agent generated, so it never has to trust an unknown host key. It then
// drives the instance over SSH as root with its own key. It also hands out
// Amazon ECR credentials for the machines' Docker.
package aws

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/elprovo/elprovo/internal/cloudinit"
	"github.com/elprovo/elprovo/internal/sshrun"
)

const (
	// groupTag is the tag key that marks the instances of one agent.
	groupTag = "elprovo-group"
	// rootLogin lets root sign in with the instance's key pair. Ubuntu AMIs
	// on AWS refuse root logins unless cloud-config turns disable_root off,
	// and the agent signs in as root.
	rootLogin = "disable_root: false\n"
)

// liveStates are the states of an instance that still exists; a terminated
// instance stays listed for about an hour.
var liveStates = []string{"pending", "running", "stopping", "stopped", "shutting-down"}

// registrySessionDuration is how long the federated session behind registry
// credentials lasts: the 12 hours of an ECR authorization token, so the
// session never ends a token sooner.
const registrySessionDuration = 12 * time.Hour

var (
	accountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	// registryPullActions are the ECR actions that a Docker pull needs.
	registryPullActions = []string{"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"}
	// registryPushActions are the ECR actions that a Docker push needs
	// beyond those of a pull. ecr:CreateRepository lets the first push to a
	// new repository under the prefix create it, should create on push check
	// the pusher's permission as well as its repository creation template's.
	registryPushActions = []string{"ecr:CompleteLayerUpload", "ecr:CreateRepository", "ecr:InitiateLayerUpload", "ecr:PutImage", "ecr:UploadLayerPart"}
)

// Config says where and how the provider launches instances. When SubnetID
// is set, instances launch into that subnet and the agent reaches them on
// their private address; otherwise they launch into the default VPC and the
// agent uses their public address.
type Config struct {
	// EC2URL is the EC2 API endpoint; empty means the region's.
	EC2URL string
	// ECRURL is the ECR API endpoint; empty means the region's.
	ECRURL string
	// STSURL is the STS API endpoint; empty means the region's.
	STSURL       string
	Region       string
	Credentials  Credentials
	InstanceType string
	// Image is a stock Ubuntu AMI; cloud-init installs Docker on it.
	Image string
	// GoldenImage, when set, is a Elprovo golden image AMI that already has
	// Docker, Compose and BuildKit. It replaces Image.
	GoldenImage string
	SubnetID    string
	// SecurityGroupID is the group of every instance. It must let in only
	// the agent and the gateway: a group that lets in its own members, such
	// as a VPC's default group, lets previews reach each other.
	SecurityGroupID string
	// VolumeSize is the root volume's size in GiB, for images, build cache
	// and preview data; 0 keeps the AMI's, which is 8 GiB for Ubuntu.
	VolumeSize int
	// Group is the value of the elprovo-group tag on every instance the
	// agent launches. The agent finds its instances by it.
	Group string
	// RepositoryPrefix is the ECR repository name prefix that the
	// environment's images live under, such as acme; empty means every
	// repository. Registry credentials reach only the repositories under it.
	RepositoryPrefix string
	HostKeyDir       string
	ReadyTimeout     time.Duration
}

// Provider launches, finds, drives and terminates instances.
type Provider struct {
	ec2       *client
	ecr       *client
	sts       *client
	cfg       Config
	machines  sshrun.Machines
	pollEvery time.Duration

	keyMu         sync.Mutex
	keyRegistered bool
}

var (
	machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// New returns a provider that signs in to instances with key.
func New(cfg Config, key ed25519.PrivateKey) (*Provider, error) {
	for name, value := range map[string]string{
		"region": cfg.Region, "instance type": cfg.InstanceType, "group": cfg.Group, "host key directory": cfg.HostKeyDir,
		"security group": cfg.SecurityGroupID,
		"access key id":  cfg.Credentials.AccessKeyID, "secret access key": cfg.Credentials.SecretAccessKey,
	} {
		if value == "" {
			return nil, fmt.Errorf("AWS provider needs a %s", name)
		}
	}
	if cfg.Image == "" && cfg.GoldenImage == "" {
		return nil, errors.New("AWS provider needs an image or a golden image")
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	ec2URL, ecrURL, stsURL := cfg.EC2URL, cfg.ECRURL, cfg.STSURL
	if ec2URL == "" {
		ec2URL = "https://ec2." + cfg.Region + ".amazonaws.com"
	}
	if ecrURL == "" {
		ecrURL = "https://api.ecr." + cfg.Region + ".amazonaws.com"
	}
	if stsURL == "" {
		stsURL = "https://sts." + cfg.Region + ".amazonaws.com"
	}
	p := &Provider{
		ec2: newClient(ec2URL, cfg.Region, "ec2", cfg.Credentials), ecr: newClient(ecrURL, cfg.Region, "ecr", cfg.Credentials),
		sts: newClient(stsURL, cfg.Region, "sts", cfg.Credentials),
		cfg: cfg, machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second,
	}
	p.machines.Address = p.Address
	return p, nil
}

// RegistryCredentials returns Docker credentials for the account's ECR
// registry at host: the user AWS and a fresh authorization token, which
// reaches only the registry, never the agent's AWS credentials. ECR gives a
// token the permissions of the credentials that asked for it, so the agent
// asks with the temporary credentials of a federated session whose policy
// allows only pulls, and pushes too when push is set, on the repositories
// under the repository prefix. A host other than the account's registry in
// the region gets no token.
func (p *Provider) RegistryCredentials(ctx context.Context, host string, push bool) (string, string, error) {
	account, ok := registryAccount(host, p.cfg.Region)
	if !ok {
		return "", "", fmt.Errorf("%s is not an Amazon ECR registry in %s", host, p.cfg.Region)
	}
	name, policy := registrySession(p.cfg.Region, account, p.cfg.RepositoryPrefix, push)
	creds, err := p.sts.federationToken(ctx, name, policy, registrySessionDuration)
	if err != nil {
		return "", "", err
	}
	auth, err := p.ecr.signedAs(creds).authorizationToken(ctx)
	if err != nil {
		return "", "", err
	}
	if endpoint, err := url.Parse(auth.ProxyEndpoint); err != nil || endpoint.Host != host {
		return "", "", fmt.Errorf("ECR gives tokens for %s, not for the registry %s", auth.ProxyEndpoint, host)
	}
	decoded, err := base64.StdEncoding.DecodeString(auth.Token)
	if err != nil {
		return "", "", fmt.Errorf("decode ECR authorization token: %w", err)
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok || username == "" || password == "" {
		return "", "", errors.New("ECR authorization token is not user:password")
	}
	return username, password, nil
}

// registryAccount returns the account id of host when it is the host of an
// ECR registry in the region, <account>.dkr.ecr.<region>.amazonaws.com.
func registryAccount(host, region string) (string, bool) {
	account, ok := strings.CutSuffix(host, ".dkr.ecr."+region+".amazonaws.com")
	return account, ok && accountPattern.MatchString(account)
}

// registrySession returns the federated user name and the session policy
// of registry credentials: ECR authorization tokens, and pulls, plus pushes
// when push is set, on the repositories under prefix in the account's
// registry.
func registrySession(region, account, prefix string, push bool) (string, string) {
	name, actions := "elprovo-registry-pull", registryPullActions
	if push {
		name, actions = "elprovo-registry-push", slices.Concat(registryPullActions, registryPushActions)
	}
	repositories := "arn:aws:ecr:" + region + ":" + account + ":repository/*"
	if prefix != "" {
		repositories = "arn:aws:ecr:" + region + ":" + account + ":repository/" + prefix + "/*"
	}
	type statement struct {
		Effect   string `json:"Effect"`
		Action   any    `json:"Action"`
		Resource string `json:"Resource"`
	}
	doc, _ := json.Marshal(struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{"2012-10-17", []statement{{"Allow", "ecr:GetAuthorizationToken", "*"}, {"Allow", actions, repositories}}})
	return name, string(doc)
}

// Exists reports whether an instance with the name exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	instances, err := p.named(ctx, name)
	return len(instances) > 0, err
}

// Create launches the instance and waits until Docker Compose runs on it.
func (p *Provider) Create(ctx context.Context, name string) error {
	if !machineNamePattern.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	keyName, err := p.ensureKeyPair(ctx)
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
	if _, err := p.ec2.runInstance(ctx, runInstance{
		ImageID: image, InstanceType: p.cfg.InstanceType, KeyName: keyName, UserData: userData + rootLogin,
		SubnetID: p.cfg.SubnetID, SecurityGroupID: p.cfg.SecurityGroupID, VolumeSize: p.cfg.VolumeSize,
		Tags: []tag{{"Name", name}, {groupTag, p.cfg.Group}},
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
			return fmt.Errorf("instance %s is not ready after %v: %w", name, p.cfg.ReadyTimeout, err)
		case <-time.After(p.pollEvery):
		}
	}
}

// Address returns the running instance's private address inside a subnet,
// or its public address otherwise.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	instances, err := p.named(ctx, name)
	if err != nil {
		return "", err
	}
	if len(instances) == 0 {
		return "", fmt.Errorf("instance %s: %w", name, errNotFound)
	}
	kind, addr := "public", func(i instance) string { return i.PublicIP }
	if p.cfg.SubnetID != "" {
		kind, addr = "private", func(i instance) string { return i.PrivateIP }
	}
	for _, i := range instances {
		if i.State.Name == "running" && addr(i) != "" {
			return addr(i), nil
		}
	}
	return "", fmt.Errorf("instance %s has no %s address yet (state %s)", name, kind, instances[0].State.Name)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the names of the instances with the provider's group tag.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	instances, err := p.ec2.describeInstances(ctx, p.filters())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(instances))
	for _, i := range instances {
		names = append(names, i.tag("Name"))
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete terminates every instance with the name and waits until each is
// terminated. Deleting a missing instance succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	instances, err := p.named(ctx, name)
	if err != nil {
		return err
	}
	for _, i := range instances {
		if err := p.ec2.terminateInstance(ctx, i.ID); err != nil {
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

// waitGone waits until AWS reports the instance terminated or no longer
// knows it.
func (p *Provider) waitGone(ctx context.Context, id string) error {
	for {
		instances, err := p.ec2.describeInstances(ctx, nil, id)
		if errors.Is(err, errNotFound) || (err == nil && (len(instances) == 0 || instances[0].State.Name == "terminated")) {
			return nil
		}
		state := ""
		if len(instances) > 0 {
			state = instances[0].State.Name
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance %s still exists (state %s): %w", id, state, errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}

func (p *Provider) named(ctx context.Context, name string) ([]instance, error) {
	return p.ec2.describeInstances(ctx, append(p.filters(), filter{"tag:Name", []string{name}}))
}

// filters select the live instances with the provider's group tag.
func (p *Provider) filters() []filter {
	return []filter{{"tag:" + groupTag, []string{p.cfg.Group}}, {"instance-state-name", liveStates}}
}

// ensureKeyPair registers the agent's public key with EC2 once and returns
// the key pair's name.
func (p *Provider) ensureKeyPair(ctx context.Context) (string, error) {
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
	name := "elprovo-agent-" + strings.ReplaceAll(ssh.FingerprintLegacyMD5(p.machines.Signer.PublicKey()), ":", "")
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keyRegistered {
		return name, nil
	}
	known, err := p.ec2.hasKeyPair(ctx, name)
	if err != nil {
		return "", err
	}
	if !known {
		if err := p.ec2.importKeyPair(ctx, name, public); err != nil {
			return "", err
		}
	}
	p.keyRegistered = true
	return name, nil
}
