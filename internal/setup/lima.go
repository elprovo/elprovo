package setup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/internal/controlplane"
)

// Lima is one `stagely setup lima` run. The agent runs on this Mac as a
// launchd job, because it drives Lima. The gateway runs in its own Lima VM,
// as it runs on its own host in a cloud: macOS does not let background
// services that are not Apple-signed reach other machines on the local
// network, and VMs reach each other freely. Stagelets and build workers run
// in Lima VMs too.
type Lima struct {
	API            API
	Server         string
	Environment    string
	StageletDomain string
	GatewayPort    int
	HostAddress    string
	RegistryPort   int
	// Binaries holds stagely-agent built for this Mac.
	Binaries string
	// GatewayBinary is stagely-gateway built for Linux on this Mac's CPU.
	GatewayBinary string
	Tofu          string
	WorkDir       string
	Module        fs.FS
	LaunchAgents  string
	// Launchctl runs launchctl with args.
	Launchctl func(ctx context.Context, args ...string) error
	// Limactl runs limactl with args and returns its standard output.
	Limactl func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	// ReadyTimeout bounds the wait for the gateway VM to boot.
	ReadyTimeout time.Duration
	UID          int
	Path         string
	Out          io.Writer
}

// Run bootstraps the local environment.
func (l Lima) Run(ctx context.Context) (Environment, error) {
	if l.Environment == "" || l.Binaries == "" || l.GatewayBinary == "" {
		return Environment{}, errors.New("setup needs an environment name, the agent's binaries directory and the gateway binary")
	}
	accountID, err := account(ctx, l.API)
	if err != nil {
		return Environment{}, err
	}
	env, err := ensureEnvironment(ctx, l.API, accountID, Environment{Name: l.Environment, Provider: "lima", StageletDomain: l.StageletDomain, StageletPort: l.GatewayPort})
	if err != nil {
		return Environment{}, err
	}
	name := moduleName(env.Name)
	_, _ = fmt.Fprintf(l.Out, "Applying the bootstrap module in %s\n", filepath.Join(l.WorkDir, "terraform"))
	outputs, err := Tofu{Binary: l.Tofu, Dir: filepath.Join(l.WorkDir, "terraform"), Out: l.Out}.Apply(ctx, l.Module, map[string]any{
		"name": name, "host_address": l.HostAddress, "registry_port": l.RegistryPort,
		"stagelet_domain": l.StageletDomain, "certificate_dir": filepath.Join(l.WorkDir, "gateway-tls"),
	})
	if err != nil {
		return Environment{}, err
	}
	registry, _ := outputs["registry"].(string)
	cert, _ := outputs["certificate_file"].(string)
	key, _ := outputs["key_file"].(string)
	if registry == "" || cert == "" || key == "" {
		return Environment{}, errors.New("the bootstrap module did not output the registry and the certificate")
	}

	agent := launchdService{role: roleAgent, binary: "stagely-agent", keyFile: "agent.key", args: []string{
		"-control-plane", controlplane.SessionURL(l.Server), "-provider", "lima", "-machine-prefix", "stagely-" + name + "-",
		"-registry", registry + "/" + name, "-lima-insecure-registry", registry,
	}}
	if err := l.installAndEnroll(ctx, accountID, env.ID, name, agent); err != nil {
		return Environment{}, err
	}
	if err := l.installGateway(ctx, accountID, env.ID, "stagely-"+name+"-gateway", cert, key); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// installGateway runs the gateway in its own Lima VM, which forwards the
// gateway port to this Mac.
func (l Lima) installGateway(ctx context.Context, accountID, envID, vm, certFile, keyFile string) error {
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return fmt.Errorf("read gateway certificate: %w", err)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("read gateway key: %w", err)
	}
	if err := l.ensureVM(ctx, vm); err != nil {
		return err
	}
	h := limaHost{vm: vm, limactl: l.Limactl}
	if err := waitReady(ctx, h, vm, l.ReadyTimeout); err != nil {
		return err
	}
	port := strconv.Itoa(l.GatewayPort)
	svc := service{
		name: "stagely-gateway", binary: l.GatewayBinary, keyFile: "gateway.key", env: map[string]string{},
		args: []string{
			"-control-plane", vmSessionURL(l.Server), "-data-dir", "/var/lib/stagely-gateway", "-listen", ":" + port,
			"-tls-cert", "/etc/stagely/tls.crt", "-tls-key", "/etc/stagely/tls.key",
		},
		files: []serviceFile{
			{path: "/etc/stagely/tls.crt", data: cert, mode: "0644"},
			{path: "/etc/stagely/tls.key", data: key, mode: "0640"},
		},
		bindLowPorts: l.GatewayPort < 1024,
	}
	keyed, err := hasKey(ctx, h, svc)
	if err != nil {
		return err
	}
	token, err := enrollmentFor(ctx, l.API, accountID, envID, roleGateway, keyed)
	if err != nil {
		return err
	}
	if token != "" {
		svc.env["STAGELY_ENROLLMENT_TOKEN"] = token
	}
	_, _ = fmt.Fprintf(l.Out, "Installing stagely-gateway in Lima VM %s\n", vm)
	before, err := clients(ctx, l.API, accountID, envID)
	if err != nil {
		return err
	}
	if err := install(ctx, h, vm, svc); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := waitConnected(waitCtx, l.API, accountID, envID, roleGateway, before, 2*time.Second)
	if err != nil {
		return fmt.Errorf("stagely-gateway: %w; see `limactl shell %s sudo journalctl -u stagely-gateway`", err, vm)
	}
	return keepOne(ctx, l.API, accountID, envID, roleGateway, id)
}

// ensureVM creates and starts the gateway's VM unless it runs. The VM is on
// Lima's user-v2 network, where it reaches stagelet VMs. Lima forwards every
// port the gateway listens on to the same port on this Mac's loopback
// address.
func (l Lima) ensureVM(ctx context.Context, vm string) error {
	out, err := l.Limactl(ctx, nil, "list", "--format", "{{.Name}} {{.Status}}")
	if err != nil {
		return err
	}
	status := ""
	for _, line := range strings.Split(string(out), "\n") {
		if n, st, ok := strings.Cut(strings.TrimSpace(line), " "); ok && n == vm {
			status = st
		}
	}
	if status == "" {
		_, _ = fmt.Fprintf(l.Out, "Creating Lima VM %s for the gateway\n", vm)
		if _, err := l.Limactl(ctx, nil, "create", "--tty=false", "--name="+vm, "--vm-type=vz", "--network=lima:user-v2",
			"--cpus=1", "--memory=1", "--disk=10", "--set=.mounts = []", "template:default"); err != nil {
			return fmt.Errorf("create gateway VM: %w", err)
		}
	}
	if status != "Running" {
		if _, err := l.Limactl(ctx, nil, "start", "--tty=false", vm); err != nil {
			return fmt.Errorf("start gateway VM: %w", err)
		}
	}
	return nil
}

// limaHost runs commands as root in a Lima VM.
type limaHost struct {
	vm      string
	limactl func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

func (h limaHost) Output(ctx context.Context, stdin io.Reader, command string) ([]byte, error) {
	return h.limactl(ctx, stdin, "shell", "--workdir", "/", h.vm, "--", "sudo", "sh", "-c", command)
}

// vmSessionURL is the agent hub's URL as a Lima VM reaches it: localhost on
// this Mac is host.lima.internal inside the VM.
func vmSessionURL(server string) string {
	u, err := url.Parse(controlplane.SessionURL(server))
	if err != nil {
		return controlplane.SessionURL(server)
	}
	if port, local := u.Port(), slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()); local {
		u.Host = "host.lima.internal"
		if port != "" {
			u.Host = net.JoinHostPort(u.Host, port)
		}
	}
	return u.String()
}

// launchdService is a daemon that runs on this Mac under launchd.
type launchdService struct {
	role    string
	binary  string
	keyFile string
	args    []string
}

func (l Lima) installAndEnroll(ctx context.Context, accountID, envID, name string, d launchdService) error {
	dataDir := filepath.Join(l.WorkDir, d.role)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dataDir, err)
	}
	binary, err := copyBinary(filepath.Join(l.Binaries, d.binary), filepath.Join(l.WorkDir, "bin", d.binary))
	if err != nil {
		return err
	}
	env := map[string]string{"PATH": l.Path}
	_, statErr := os.Stat(filepath.Join(dataDir, d.keyFile))
	token, err := enrollmentFor(ctx, l.API, accountID, envID, d.role, statErr == nil)
	if err != nil {
		return err
	}
	if token != "" {
		env["STAGELY_ENROLLMENT_TOKEN"] = token
	}
	label := "dev.stagely." + name + "." + d.role
	plist := launchdPlist(label, append([]string{binary, "run", "-data-dir", dataDir}, d.args...), env, filepath.Join(l.WorkDir, d.role+".log"))
	path := filepath.Join(l.LaunchAgents, label+".plist")
	if err := os.MkdirAll(l.LaunchAgents, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", l.LaunchAgents, err)
	}
	if err := os.WriteFile(path, plist, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	domain := "gui/" + strconv.Itoa(l.UID)
	before, err := clients(ctx, l.API, accountID, envID)
	if err != nil {
		return err
	}
	if err := l.stop(ctx, domain+"/"+label); err != nil {
		return err
	}
	if err := l.Launchctl(ctx, "bootstrap", domain, path); err != nil {
		return fmt.Errorf("start %s: %w", label, err)
	}
	_, _ = fmt.Fprintf(l.Out, "Started %s; its log is %s\n", label, filepath.Join(l.WorkDir, d.role+".log"))
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := waitConnected(waitCtx, l.API, accountID, envID, d.role, before, 2*time.Second)
	if err != nil {
		return err
	}
	return keepOne(ctx, l.API, accountID, envID, d.role, id)
}

// stop unloads the job and waits until launchd no longer has it, because
// bootout returns before the job has stopped.
func (l Lima) stop(ctx context.Context, job string) error {
	if l.Launchctl(ctx, "print", job) != nil {
		return nil
	}
	_ = l.Launchctl(ctx, "bootout", job)
	for range 50 {
		if l.Launchctl(ctx, "print", job) != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("launchd still has %s ten seconds after bootout", job)
}

func copyBinary(src, dst string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o700); err != nil {
		return "", fmt.Errorf("write %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("write %s: %w", dst, err)
	}
	return dst, nil
}

// launchdPlist renders a launchd job that keeps the program running.
func launchdPlist(label string, program []string, env map[string]string, log string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key := func(k string) { b.WriteString("  <key>" + escape(k) + "</key>\n") }
	str := func(v string) string { return "<string>" + escape(v) + "</string>" }
	key("Label")
	b.WriteString("  " + str(label) + "\n")
	key("ProgramArguments")
	b.WriteString("  <array>\n")
	for _, a := range program {
		b.WriteString("    " + str(a) + "\n")
	}
	b.WriteString("  </array>\n")
	key("EnvironmentVariables")
	b.WriteString("  <dict>\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("    <key>" + escape(k) + "</key>" + str(env[k]) + "\n")
	}
	b.WriteString("  </dict>\n")
	key("RunAtLoad")
	b.WriteString("  <true/>\n")
	key("KeepAlive")
	b.WriteString("  <true/>\n")
	key("StandardOutPath")
	b.WriteString("  " + str(log) + "\n")
	key("StandardErrorPath")
	b.WriteString("  " + str(log) + "\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
