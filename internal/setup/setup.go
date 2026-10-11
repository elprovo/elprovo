// Package setup bootstraps a cloud account for one Stagely environment: it
// makes sure the environment exists on the control plane, applies the
// cloud's bootstrap module, then installs the agent and the gateway on their
// hosts over SSH and enrolls them. Running it again converges on the same
// result: one agent and one gateway, both enrolled.
//
// Every cloud's module takes the variables name, ssh_public_key,
// operator_cidrs, agent_user_data and gateway_user_data, and outputs
// agent_address, gateway_address and gateway_ip. A Cloud with a
// PrivateNetwork also passes network_range. A Cloud adds the rest.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/stagely-dev/stagely/internal/cloudinit"
	"github.com/stagely-dev/stagely/internal/sshrun"
)

// Roles of the two daemons.
const (
	roleAgent   = "agent"
	roleGateway = "gateway"
)

// Cloud is what one cloud adds to a setup run.
type Cloud struct {
	// Provider is the environment's provider in Stagely, such as
	// digitalocean.
	Provider string
	Module   fs.FS
	// TofuEnv holds the cloud credentials the module's provider reads.
	TofuEnv []string
	// Vars are the module's cloud-specific variables.
	Vars map[string]any
	// PrivateNetwork, when valid, is the address range of the private
	// network that the module puts the hosts on, which it gets as
	// network_range. The cloud's firewall does not filter that network, so
	// each host's user data makes it reject new connections from the
	// network.
	PrivateNetwork netip.Prefix
	// Agent returns the agent's cloud flags and credentials, from the
	// module's outputs.
	Agent func(outputs map[string]any) (args []string, env map[string]string, err error)
}

// Run is one `stagely setup <cloud>` run.
type Run struct {
	API            API
	Server         string
	Environment    string
	StageletDomain string
	OperatorCIDRs  []string
	// Binaries holds stagely-agent and stagely-gateway built for the hosts.
	Binaries string
	ACME     ACME
	Tofu     string
	WorkDir  string
	// SSHPort is the hosts' SSH port; tests against a simulation change it.
	SSHPort      string
	ReadyTimeout time.Duration
	Out          io.Writer
	Cloud        Cloud
}

// ACME says where the gateway gets its certificates.
type ACME struct {
	Email        string
	DirectoryURL string
	// DirectoryCA is a PEM file the gateway trusts for the ACME server's own
	// HTTPS, for test servers such as Pebble.
	DirectoryCA string
}

// Result is what a run set up.
type Result struct {
	Environment Environment
	GatewayIP   string
}

// Run bootstraps the environment.
func (r Run) Run(ctx context.Context) (Result, error) {
	if err := r.check(); err != nil {
		return Result{}, err
	}
	accountID, err := account(ctx, r.API)
	if err != nil {
		return Result{}, err
	}
	r.printf("Environment %s\n", r.Environment)
	env, err := ensureEnvironment(ctx, r.API, accountID, Environment{Name: r.Environment, Provider: r.Cloud.Provider, StageletDomain: r.StageletDomain, StageletPort: 443})
	if err != nil {
		return Result{}, err
	}
	keys, err := r.keys()
	if err != nil {
		return Result{}, err
	}

	r.printf("Applying the bootstrap module in %s\n", filepath.Join(r.WorkDir, "terraform"))
	vars := map[string]any{
		"name": moduleName(env.Name), "ssh_public_key": keys.operatorPub, "operator_cidrs": r.OperatorCIDRs,
		"agent_user_data": keys.agentData, "gateway_user_data": keys.gatewayData,
	}
	if r.Cloud.PrivateNetwork.IsValid() {
		vars["network_range"] = r.Cloud.PrivateNetwork.String()
	}
	maps.Copy(vars, r.Cloud.Vars)
	outputs, err := Tofu{Binary: r.Tofu, Dir: filepath.Join(r.WorkDir, "terraform"), Env: r.Cloud.TofuEnv, Out: r.Out}.Apply(ctx, r.Cloud.Module, vars)
	if err != nil {
		return Result{}, err
	}
	agentAddr, gatewayAddr, gatewayIP, err := hostOutputs(outputs)
	if err != nil {
		return Result{}, err
	}
	cloudArgs, cloudEnv, err := r.Cloud.Agent(outputs)
	if err != nil {
		return Result{}, err
	}

	agent := service{
		name: "stagely-agent", binary: filepath.Join(r.Binaries, "stagely-agent"), keyFile: "agent.key",
		args: append([]string{"-control-plane", sessionURL(r.Server), "-data-dir", "/var/lib/stagely-agent", "-provider", r.Cloud.Provider}, cloudArgs...),
		env:  cloudEnv,
	}
	hosts := []struct {
		role string
		addr string
		key  ssh.PublicKey
		svc  service
	}{
		{roleAgent, agentAddr, keys.agentHost, agent},
		{roleGateway, gatewayAddr, keys.gatewayHost, r.gateway()},
	}
	for _, h := range hosts {
		target := sshrun.Target{Addr: net.JoinHostPort(h.addr, r.SSHPort), HostKey: h.key, Signer: keys.operator}
		if err := r.installAndEnroll(ctx, target, accountID, env.ID, h.role, h.svc); err != nil {
			return Result{}, err
		}
	}
	return Result{Environment: env, GatewayIP: gatewayIP}, nil
}

func (r Run) gateway() service {
	args := []string{"-control-plane", sessionURL(r.Server), "-data-dir", "/var/lib/stagely-gateway", "-listen", ":443", "-http-listen", ":80", "-acme"}
	if r.ACME.Email != "" {
		args = append(args, "-acme-email", r.ACME.Email)
	}
	if r.ACME.DirectoryURL != "" {
		args = append(args, "-acme-directory", r.ACME.DirectoryURL)
	}
	return service{name: "stagely-gateway", binary: filepath.Join(r.Binaries, "stagely-gateway"), keyFile: "gateway.key", args: args, env: map[string]string{}, bindLowPorts: true}
}

func (r Run) installAndEnroll(ctx context.Context, target sshrun.Target, accountID, envID, role string, svc service) error {
	r.printf("Installing %s on %s\n", svc.name, target.Addr)
	if err := waitReady(ctx, target, target.Addr, r.ReadyTimeout); err != nil {
		return err
	}
	keyed, err := hasKey(ctx, target, svc)
	if err != nil {
		return err
	}
	token, err := enrollmentFor(ctx, r.API, accountID, envID, role, keyed)
	if err != nil {
		return err
	}
	if svc.env == nil {
		svc.env = map[string]string{}
	}
	if token != "" {
		svc.env["STAGELY_ENROLLMENT_TOKEN"] = token
	}
	if role == roleGateway && r.ACME.DirectoryCA != "" {
		ca, err := os.ReadFile(r.ACME.DirectoryCA)
		if err != nil {
			return fmt.Errorf("read ACME directory CA: %w", err)
		}
		svc.files = append(svc.files, serviceFile{path: "/etc/stagely/acme-ca.pem", data: ca, mode: "0644"})
		svc.args = append(svc.args, "-acme-directory-ca", "/etc/stagely/acme-ca.pem")
	}
	before, err := clients(ctx, r.API, accountID, envID)
	if err != nil {
		return err
	}
	if err := install(ctx, target, target.Addr, svc); err != nil {
		return err
	}
	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := waitConnected(connectCtx, r.API, accountID, envID, role, before, 2*time.Second)
	if err != nil {
		return fmt.Errorf("%s: %w; see `journalctl -u %s` on %s", svc.name, err, svc.name, target.Addr)
	}
	if err := keepOne(ctx, r.API, accountID, envID, role, id); err != nil {
		return err
	}
	r.printf("%s is connected\n", svc.name)
	return nil
}

func (r Run) check() error {
	missing := map[string]string{"environment": r.Environment, "stagelet domain": r.StageletDomain, "binaries directory": r.Binaries}
	for what, v := range missing {
		if v == "" {
			return fmt.Errorf("setup needs a %s", what)
		}
	}
	if len(r.OperatorCIDRs) == 0 {
		return errors.New("setup needs the operator addresses that may reach the hosts over SSH")
	}
	for _, bin := range []string{"stagely-agent", "stagely-gateway"} {
		if _, err := os.Stat(filepath.Join(r.Binaries, bin)); err != nil {
			return fmt.Errorf("no %s for the hosts in %s: %w", bin, r.Binaries, err)
		}
	}
	return nil
}

type setupKeys struct {
	operator    ssh.Signer
	operatorPub string
	agentHost   ssh.PublicKey
	gatewayHost ssh.PublicKey
	agentData   string
	gatewayData string
}

// keys loads or creates the operator's SSH key and the hosts' host keys in
// the work directory, so a later run reaches the same hosts.
func (r Run) keys() (setupKeys, error) {
	operator, err := loadOrCreateKey(filepath.Join(r.WorkDir, "operator_ed25519"))
	if err != nil {
		return setupKeys{}, err
	}
	signer, err := ssh.NewSignerFromKey(operator)
	if err != nil {
		return setupKeys{}, err
	}
	k := setupKeys{operator: signer, operatorPub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
	for _, h := range []struct {
		file string
		data *string
		pub  *ssh.PublicKey
	}{
		{"agent_host_ed25519", &k.agentData, &k.agentHost},
		{"gateway_host_ed25519", &k.gatewayData, &k.gatewayHost},
	} {
		hostKey, err := loadOrCreateKeyPEM(filepath.Join(r.WorkDir, h.file))
		if err != nil {
			return setupKeys{}, err
		}
		if *h.data, *h.pub, err = r.hostConfig(string(hostKey)); err != nil {
			return setupKeys{}, err
		}
	}
	return k, nil
}

// hostConfig returns the user data and the public host key of a host with
// hostKeyPEM. On a private network, the host accepts no new connections from
// the network: the agent and the gateway serve nothing there.
func (r Run) hostConfig(hostKeyPEM string) (string, ssh.PublicKey, error) {
	if !r.Cloud.PrivateNetwork.IsValid() {
		return cloudinit.HostKeyConfig(hostKeyPEM)
	}
	return cloudinit.HostKeyConfigOnNetwork(hostKeyPEM, cloudinit.PrivateNetwork{Range: r.Cloud.PrivateNetwork})
}

// privateNetworkRange is the address range of the private network that
// setup gives an environment on a cloud whose firewall does not filter
// private networks.
const privateNetworkRange = "10.42.0.0/20"

func hostOutputs(o map[string]any) (agent, gateway, gatewayIP string, err error) {
	if agent, err = outputString(o, "agent_address"); err != nil {
		return "", "", "", err
	}
	if gateway, err = outputString(o, "gateway_address"); err != nil {
		return "", "", "", err
	}
	if gatewayIP, err = outputString(o, "gateway_ip"); err != nil {
		return "", "", "", err
	}
	return agent, gateway, gatewayIP, nil
}

func outputString(o map[string]any, name string) (string, error) {
	v, ok := o[name].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("the bootstrap module did not output %s", name)
	}
	return v, nil
}

// moduleName turns an environment name into a resource prefix the modules
// accept.
func moduleName(env string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(env) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "env-" + name
	}
	if len(name) > 31 {
		name = strings.TrimRight(name[:31], "-")
	}
	return name
}

// sessionURL is the WebSocket URL of the control plane's agent hub.
func sessionURL(server string) string {
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return server
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path += "/v1/connect"
	return u.String()
}

func (r Run) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.Out, format, args...)
}
