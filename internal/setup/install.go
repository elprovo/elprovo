package setup

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/elprovo/elprovo/internal/sshrun"
)

// host runs shell commands as root on a machine setup installs on.
type host interface {
	Output(ctx context.Context, stdin io.Reader, command string) ([]byte, error)
}

// service is an Elprovo daemon that setup installs on a host as a systemd
// service, running as its own system user with its state in
// /var/lib/<name>.
type service struct {
	name    string
	binary  string
	keyFile string
	args    []string
	env     map[string]string
	// files are put on the host before the service starts.
	files []serviceFile
	// bindLowPorts lets the service listen on ports below 1024.
	bindLowPorts bool
}

// serviceFile is a file the service reads. It belongs to root and the
// service's group, with mode such as 0640.
type serviceFile struct {
	path string
	data []byte
	mode string
}

var safeArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]*$`)

func (s service) stateDir() string { return "/var/lib/" + s.name }

func (s service) unit() (string, error) {
	for _, a := range s.args {
		if !safeArg.MatchString(a) {
			return "", fmt.Errorf("%s argument %q has characters a systemd unit would need to quote", s.name, a)
		}
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=" + s.name + "\nWants=network-online.target\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nUser=" + s.name + "\nGroup=" + s.name + "\n")
	b.WriteString("EnvironmentFile=/etc/elprovo/" + s.name + ".env\n")
	b.WriteString("ExecStart=/usr/local/bin/" + s.name + " run " + strings.Join(s.args, " ") + "\n")
	b.WriteString("StateDirectory=" + s.name + "\nStateDirectoryMode=0700\n")
	b.WriteString("Restart=always\nRestartSec=5\nNoNewPrivileges=yes\nProtectSystem=strict\nProtectHome=yes\nPrivateTmp=yes\n")
	if s.bindLowPorts {
		b.WriteString("AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n")
	}
	b.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	return b.String(), nil
}

// envFile renders the service's environment for systemd.
func (s service) envFile() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(s.env)) {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s.env[k])
		b.WriteString(k + `="` + v + "\"\n")
	}
	return b.String()
}

// waitReady waits until the host answers and cloud-init has finished.
func waitReady(ctx context.Context, h host, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, err := h.Output(ctx, nil, "cloud-init status --wait >/dev/null 2>&1; command -v systemctl")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("host %s is not ready after %v: %w", name, timeout, err)
		case <-time.After(3 * time.Second):
		}
	}
}

// hasKey reports whether the service on the host already has its key.
func hasKey(ctx context.Context, h host, s service) (bool, error) {
	out, err := h.Output(ctx, nil, "test -s "+sshrun.Quote(s.stateDir()+"/"+s.keyFile)+" && echo yes || true")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// installStep is one command of an install, with what it reads on stdin.
type installStep struct {
	what    string
	command string
	input   func() (io.ReadCloser, error)
}

// install puts the service's binary, environment, files and unit on the
// host and (re)starts it. Running it again replaces them with the same
// result.
func install(ctx context.Context, h host, name string, s service) error {
	unit, err := s.unit()
	if err != nil {
		return err
	}
	user := sshrun.Quote(s.name)
	bin := "/usr/local/bin/" + s.name
	text := func(t string) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(t)), nil }
	}
	steps := []installStep{
		{"create user", "id -u " + user + " >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin " + user, nil},
		{"install binary", "cat > " + bin + ".new && chmod 0755 " + bin + ".new && mv " + bin + ".new " + bin, func() (io.ReadCloser, error) { return os.Open(s.binary) }},
		{"write environment", "install -d -m 0755 /etc/elprovo && umask 077 && cat > /etc/elprovo/" + s.name + ".env", text(s.envFile())},
		{"write unit", "cat > /etc/systemd/system/" + s.name + ".service", text(unit)},
	}
	for _, f := range s.files {
		path := sshrun.Quote(f.path)
		steps = append(steps, installStep{"write " + f.path, "umask 077 && cat > " + path + " && chown root:" + user + " " + path + " && chmod " + f.mode + " " + path, text(string(f.data))})
	}
	steps = append(steps, installStep{"start service", "systemctl daemon-reload && systemctl enable " + user + " && systemctl restart " + user, nil})
	for _, step := range steps {
		var stdin io.ReadCloser
		if step.input != nil {
			if stdin, err = step.input(); err != nil {
				return fmt.Errorf("%s %s: %w", step.what, s.name, err)
			}
		}
		_, err := h.Output(ctx, stdin, step.command)
		if stdin != nil {
			_ = stdin.Close()
		}
		if err != nil {
			return fmt.Errorf("%s %s on %s: %w", step.what, s.name, name, err)
		}
	}
	return nil
}
