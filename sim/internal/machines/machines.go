// Package machines runs simulated cloud VMs as Docker containers and drives
// them through the docker CLI. Every simulated cloud uses it. A machine runs
// the image built from sim/vm: Ubuntu with OpenSSH, Docker and the Compose
// plugin, booted by stagely-vm-init, which applies NoCloud seed data the way
// cloud-init does.
//
// Each machine has a private address on one Docker network and a public
// address on a second network named after the first with a "-public" suffix.
// Both addresses are static, so they survive stop and start.
package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	labelCloud    = "stagely-sim.cloud"
	labelAccount  = "stagely-sim.account"
	labelResource = "stagely-sim.resource"
	stopSeconds   = "30"
)

// ErrNotFound reports that no container exists for a machine.
var ErrNotFound = errors.New("machine not found")

// Backend creates and drives machines. It is safe for concurrent use.
type Backend struct {
	network       string
	publicNetwork string
	options       Options
	log           *slog.Logger

	imageMu sync.Mutex
	image   string

	createMu sync.Mutex
}

// New returns a backend that attaches machines to network for private
// addresses and to network+"-public" for public ones. Both networks are
// created when missing.
func New(network string, options Options, log *slog.Logger) *Backend {
	return &Backend{network: network, publicNetwork: network + "-public", options: options, log: log}
}

// Spec describes a machine to create.
type Spec struct {
	// Name is the container name. It must be unique on the Docker host.
	Name     string
	Hostname string
	// Cloud, Account and Resource label the container, so List finds it.
	Cloud, Account, Resource string
	// Image is the image to boot; empty means the base image.
	Image string
	// PrivateIP is the machine's address on the private network, for a
	// cloud that hands out the address before the machine exists. It must
	// be in the private subnet and free. Empty takes the highest free
	// address.
	PrivateIP string
	// PublicIP is the machine's address on the public network, for a cloud
	// that hands out the address before the machine exists. It must be in
	// the public subnet and free. Empty takes the highest free address.
	PublicIP string
	Seed     Seed
	Firewall *Firewall
	Ports    []Port
	// Hosts maps host names to the addresses they resolve to on the
	// machine, such as the name of a cloud's metadata server.
	Hosts map[string]string
}

// Seed is the NoCloud data a machine boots with, and the instance metadata it
// serves at 169.254.169.254.
type Seed struct {
	InstanceID string
	PublicKeys []string
	UserData   string
	// Metadata returns the metadata documents by path, such as
	// /metadata/v1/hostname, given the machine's addresses. Nil serves none.
	Metadata func(privateIPv4, publicIPv4 string) map[string]string
}

// Port publishes a machine's TCP port on the Docker host's 127.0.0.1.
type Port struct {
	Host    int
	Machine int
}

// Machine is a machine as Docker reports it.
type Machine struct {
	ID                       string
	Name                     string
	Cloud, Account, Resource string
	Running                  bool
	PrivateIP, PublicIP      string
}

// Firewall is the traffic a machine accepts on its private and public
// addresses. A nil *Firewall lets all traffic through; a Firewall drops every
// new connection that no rule allows. Established connections and loopback
// traffic always pass.
type Firewall struct {
	Inbound  []Rule `json:"inbound"`
	Outbound []Rule `json:"outbound"`
}

// Rule allows one protocol from (Inbound) or to (Outbound) Peers, which are
// IPv4 addresses or CIDR blocks. Protocol is tcp, udp or icmp. Ports is a
// port or an inclusive range such as "8000-9000"; empty means every port.
type Rule struct {
	Protocol string   `json:"protocol"`
	Ports    string   `json:"ports"`
	Peers    []string `json:"peers"`
}

// Create creates the machine and starts it.
func (b *Backend) Create(ctx context.Context, spec Spec) (Machine, error) {
	image := spec.Image
	if image == "" {
		var err error
		if image, err = b.BaseImage(ctx); err != nil {
			return Machine{}, err
		}
	}
	if err := b.EnsureNetworks(ctx); err != nil {
		return Machine{}, err
	}
	id, addrs, err := b.createContainer(ctx, spec, image)
	if err != nil {
		return Machine{}, err
	}
	seed, err := seedArchive(spec, addrs, b.options)
	if err == nil {
		_, err = b.docker(ctx, bytes.NewReader(seed), "cp", "-", id+":/")
	}
	if err != nil {
		return Machine{}, errors.Join(err, b.cleanup(ctx, id))
	}
	if _, err := b.docker(ctx, nil, "start", id); err != nil {
		return Machine{}, errors.Join(err, b.cleanup(ctx, id))
	}
	m, err := b.Inspect(ctx, id)
	if err != nil {
		return Machine{}, errors.Join(err, b.cleanup(ctx, id))
	}
	return m, nil
}

func (b *Backend) cleanup(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	return b.Delete(ctx, id)
}

type addresses struct{ private, public string }

func (b *Backend) createContainer(ctx context.Context, spec Spec, image string) (string, addresses, error) {
	b.createMu.Lock()
	defer b.createMu.Unlock()
	var addrs addresses
	used, err := b.usedAddresses(ctx)
	if err != nil {
		return "", addrs, err
	}
	args := []string{
		"create", "--name", spec.Name, "--hostname", spec.Hostname,
		"--init", "--privileged", "--stop-timeout", stopSeconds,
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "--volume", "/var/lib/docker",
		"--label", labelCloud + "=" + spec.Cloud,
		"--label", labelAccount + "=" + spec.Account,
		"--label", labelResource + "=" + spec.Resource,
	}
	if addrs.private, err = b.takeAddress(ctx, b.network, spec.PrivateIP, used); err != nil {
		return "", addrs, err
	}
	if addrs.public, err = b.takeAddress(ctx, b.publicNetwork, spec.PublicIP, used); err != nil {
		return "", addrs, err
	}
	args = append(args, "--network", "name="+b.network+",ip="+addrs.private, "--network", "name="+b.publicNetwork+",ip="+addrs.public)
	for _, p := range spec.Ports {
		args = append(args, "--publish", "127.0.0.1:"+strconv.Itoa(p.Host)+":"+strconv.Itoa(p.Machine))
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Hosts)) {
		args = append(args, "--add-host", name+":"+spec.Hosts[name])
	}
	out, err := b.docker(ctx, nil, append(args, image)...)
	if err != nil {
		return "", addrs, err
	}
	return strings.TrimSpace(string(out)), addrs, nil
}

// Inspect returns the machine with the container id or name.
func (b *Backend) Inspect(ctx context.Context, id string) (Machine, error) {
	machines, err := b.inspect(ctx, id)
	if err != nil {
		return Machine{}, err
	}
	return machines[0], nil
}

// List returns the machines of a cloud account, running or not.
func (b *Backend) List(ctx context.Context, cloud, account string) ([]Machine, error) {
	out, err := b.docker(ctx, nil, "ps", "--all", "--quiet", "--no-trunc",
		"--filter", "label="+labelCloud+"="+cloud, "--filter", "label="+labelAccount+"="+account)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	return b.inspect(ctx, ids...)
}

// Start starts a stopped machine. Starting a running machine does nothing.
func (b *Backend) Start(ctx context.Context, id string) (Machine, error) {
	if _, err := b.docker(ctx, nil, "start", id); err != nil {
		return Machine{}, err
	}
	return b.Inspect(ctx, id)
}

// Stop stops the machine: gracefully, so stagely-vm-init stops its units
// first, or at once, like pulling the power cable.
func (b *Backend) Stop(ctx context.Context, id string, graceful bool) error {
	if graceful {
		_, err := b.docker(ctx, nil, "stop", "--time", stopSeconds, id)
		return err
	}
	_, err := b.docker(ctx, nil, "kill", id)
	if err != nil && strings.Contains(err.Error(), "is not running") {
		return nil
	}
	return err
}

// Restart stops the machine as Stop does and starts it again.
func (b *Backend) Restart(ctx context.Context, id string, graceful bool) (Machine, error) {
	if err := b.Stop(ctx, id, graceful); err != nil {
		return Machine{}, err
	}
	return b.Start(ctx, id)
}

// Delete removes the machine's container and volumes, then confirms that
// Docker no longer has the container. Deleting a missing machine succeeds.
func (b *Backend) Delete(ctx context.Context, id string) error {
	if _, err := b.docker(ctx, nil, "rm", "--force", "--volumes", id); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := b.inspect(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("container %s still exists after removal", id)
}

// SetFirewall replaces the firewall of a running machine. The machine keeps
// it across reboots.
func (b *Backend) SetFirewall(ctx context.Context, id string, fw *Firewall) error {
	data, err := json.Marshal(fw)
	if err != nil {
		return err
	}
	_, err = b.docker(ctx, bytes.NewReader(data), "exec", "--interactive", id, "stagely-vm-init", "firewall")
	return err
}

// SetAddresses gives the running machine exactly addrs as extra IPv4
// addresses on its public interface, as a cloud routes a reserved IP to a
// server. The machine keeps them across restarts.
func (b *Backend) SetAddresses(ctx context.Context, id string, addrs []string) error {
	data, err := json.Marshal(map[string][]string{"addresses": append([]string{}, addrs...)})
	if err != nil {
		return err
	}
	_, err = b.docker(ctx, bytes.NewReader(data), "exec", "--interactive", id, "stagely-vm-init", "addresses")
	return err
}

// Exec runs a command as root in the running machine, with stdin, and
// returns its standard output. A cloud uses it for what its provisioning
// agent does inside a new VM, such as creating the admin user.
func (b *Backend) Exec(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error) {
	return b.docker(ctx, stdin, append([]string{"exec", "--interactive", id}, args...)...)
}

// Commit saves the machine's filesystem as the image ref, which Create can
// boot later, and returns the image's size in bytes. The image does not
// include /var/lib/docker, which is a volume.
func (b *Backend) Commit(ctx context.Context, id, ref string) (int64, error) {
	if _, err := b.docker(ctx, nil, "commit", id, ref); err != nil {
		return 0, err
	}
	out, err := b.docker(ctx, nil, "image", "inspect", "--format", "{{.Size}}", ref)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// Networks describes the private and public networks, creating them when
// missing.
func (b *Backend) Networks(ctx context.Context) (Networks, error) {
	if err := b.EnsureNetworks(ctx); err != nil {
		return Networks{}, err
	}
	private, err := b.subnet(ctx, b.network)
	if err != nil {
		return Networks{}, err
	}
	public, err := b.subnet(ctx, b.publicNetwork)
	if err != nil {
		return Networks{}, err
	}
	return Networks{Private: private, Public: public}, nil
}

// Networks are the subnets that machine addresses come from.
type Networks struct {
	Private, Public Subnet
}

// Subnet is a network's IPv4 prefix and gateway.
type Subnet struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
}

// RemoveImage removes an image made by Commit. Removing a missing image
// succeeds.
func (b *Backend) RemoveImage(ctx context.Context, ref string) error {
	_, err := b.docker(ctx, nil, "image", "rm", "--force", ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type containerJSON struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress  string `json:"IPAddress"`
			IPAMConfig *struct {
				IPv4Address string `json:"IPv4Address"`
			} `json:"IPAMConfig"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (b *Backend) inspect(ctx context.Context, ids ...string) ([]Machine, error) {
	out, err := b.docker(ctx, nil, append([]string{"container", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var containers []containerJSON
	if err := json.Unmarshal(out, &containers); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}
	machines := make([]Machine, 0, len(containers))
	for _, c := range containers {
		machines = append(machines, Machine{
			ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Running: c.State.Running,
			Cloud: c.Config.Labels[labelCloud], Account: c.Config.Labels[labelAccount], Resource: c.Config.Labels[labelResource],
			PrivateIP: staticAddress(c, b.network), PublicIP: staticAddress(c, b.publicNetwork),
		})
	}
	return machines, nil
}

func staticAddress(c containerJSON, network string) string {
	n, ok := c.NetworkSettings.Networks[network]
	if !ok {
		return ""
	}
	if n.IPAMConfig != nil && n.IPAMConfig.IPv4Address != "" {
		return n.IPAMConfig.IPv4Address
	}
	return n.IPAddress
}
