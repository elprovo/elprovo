// Package lima runs stagelet machines as local Lima VMs through limactl.
//
// Each VM uses the vz backend, Lima's user-v2 network, where VMs reach each
// other and reach the Mac as host.lima.internal, and plain mode, so it has no
// host mounts and no port forwarding to the Mac. Containers in a VM can't
// open connections to other VMs or to the Mac's loopback services.
package lima

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Provider creates, inspects and deletes Lima VMs.
type Provider struct {
	Limactl   string
	Template  string
	CPUs      int
	MemoryGiB int
	DiskGiB   int
	// InsecureRegistries are registries, as host:port, that Docker in each
	// VM may reach over plain HTTP, such as a registry on the Mac.
	InsecureRegistries []string
}

// New returns a provider with the default VM size.
func New() *Provider {
	return &Provider{Limactl: "limactl", Template: "template:docker-rootful", CPUs: 2, MemoryGiB: 4, DiskGiB: 20}
}

// Create creates and starts the VM.
func (p *Provider) Create(ctx context.Context, name string) error {
	if _, err := p.limactl(ctx, nil, "create", "--tty=false", "--name="+name,
		"--vm-type=vz", "--network=lima:user-v2", "--plain",
		"--cpus="+strconv.Itoa(p.CPUs), "--memory="+strconv.Itoa(p.MemoryGiB), "--disk="+strconv.Itoa(p.DiskGiB),
		p.Template); err != nil {
		return err
	}
	if _, err := p.limactl(ctx, nil, "start", "--tty=false", name); err != nil {
		return err
	}
	return p.configureDocker(ctx, name)
}

// containerIsolation rejects connections that containers open to the VM's
// own network: the other Lima VMs, and the gateway, which on user-v2 leads to
// the Mac's loopback services. DNS to the gateway stays open, because
// containers on Docker's default bridge resolve names there. Replies and the
// VM's own connections, such as image pulls and health checks, are not
// affected.
const containerIsolation = `#!/bin/sh
set -eu
net=$(ip -4 -o addr show eth0 | awk '{print $4}')
gw=$(ip -4 route show default dev eth0 | awk '{print $3; exit}')
iptables -F DOCKER-USER
iptables -A DOCKER-USER -d "$gw" -p udp --dport 53 -j RETURN
iptables -A DOCKER-USER -d "$gw" -p tcp --dport 53 -j RETURN
iptables -A DOCKER-USER -d "$net" -m conntrack --ctstate NEW -j REJECT
iptables -A DOCKER-USER -j RETURN
`

// configureDocker makes Docker apply containerIsolation each time it starts
// and trust the insecure registries, then restarts it.
func (p *Provider) configureDocker(ctx context.Context, name string) error {
	files := []vmFile{
		{"/usr/local/sbin/stagely-container-isolation", "0755", []byte(containerIsolation)},
		{"/etc/systemd/system/docker.service.d/stagely-container-isolation.conf", "0644", []byte("[Service]\nExecStartPost=/usr/local/sbin/stagely-container-isolation\n")},
	}
	if len(p.InsecureRegistries) > 0 {
		config, err := json.Marshal(map[string][]string{"insecure-registries": p.InsecureRegistries})
		if err != nil {
			return err
		}
		files = append(files, vmFile{"/etc/docker/daemon.json", "0644", config})
	}
	for _, f := range files {
		if err := p.runQuiet(ctx, name, bytes.NewReader(f.data), "sudo", "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1" && chmod "$2" "$1"`, "sh", f.path, f.mode); err != nil {
			return fmt.Errorf("configure Docker in %s: %w", name, err)
		}
	}
	if err := p.runQuiet(ctx, name, nil, "sudo", "sh", "-c", "systemctl daemon-reload && systemctl restart docker"); err != nil {
		return fmt.Errorf("restart Docker in %s: %w", name, err)
	}
	return nil
}

// vmFile is a file that configureDocker writes in the VM.
type vmFile struct {
	path, mode string
	data       []byte
}

// runQuiet runs a command in the VM and adds the end of its standard error
// to the error.
func (p *Provider) runQuiet(ctx context.Context, name string, stdin io.Reader, args ...string) error {
	var stderr bytes.Buffer
	if err := p.Run(ctx, name, stdin, nil, &stderr, args...); err != nil {
		return fmt.Errorf("%w: %s", err, tail(stderr.String(), 2000))
	}
	return nil
}

// Exists reports whether a VM with the name exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	names, err := p.List(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

// List returns the names of all Lima VMs. Lima has no tags, so the agent
// tells its own VMs apart by their name prefix.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	out, err := p.limactl(ctx, nil, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseInstanceNames(out)
}

// Address returns the VM's IPv4 address on the user-v2 network.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	var out bytes.Buffer
	if err := p.Run(ctx, name, nil, &out, nil, "ip", "-4", "-o", "addr", "show", "eth0"); err != nil {
		return "", err
	}
	return parseIPv4(out.Bytes())
}

// Run runs a command in the VM and writes its output to stdout and stderr;
// nil discards it.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, p.Limactl, append([]string{"shell", "--workdir", "/", name, "--"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s in %s: %w", args[0], name, err)
	}
	return nil
}

// CopyIn copies a file from the Mac into the VM.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	_, err := p.limactl(ctx, nil, "copy", localPath, name+":"+remotePath)
	return err
}

// Delete removes the VM and confirms it is gone. Deleting a missing VM succeeds.
func (p *Provider) Delete(ctx context.Context, name string) error {
	exists, err := p.Exists(ctx, name)
	if err != nil || !exists {
		return err
	}
	if _, err := p.limactl(ctx, nil, "delete", "--force", "--tty=false", name); err != nil {
		return err
	}
	exists, err = p.Exists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("delete VM %s: still listed after delete", name)
	}
	return nil
}

func (p *Provider) limactl(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.Limactl, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("limactl %s: %w: %s", args[0], err, tail(stderr.String(), 2000))
	}
	return stdout.Bytes(), nil
}

// parseInstanceNames reads the NDJSON that `limactl list --format json` prints.
func parseInstanceNames(out []byte) ([]string, error) {
	var names []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var inst struct {
			Name string `json:"name"`
		}
		if err := dec.Decode(&inst); err != nil {
			return nil, fmt.Errorf("parse limactl list: %w", err)
		}
		names = append(names, inst.Name)
	}
	return names, nil
}

// parseIPv4 reads the address from `ip -4 -o addr show <iface>` output.
func parseIPv4(out []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		for i, f := range fields {
			if f == "inet" && i+1 < len(fields) {
				addr, _, _ := strings.Cut(fields[i+1], "/")
				return addr, nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 address in %q", tail(string(out), 200))
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
