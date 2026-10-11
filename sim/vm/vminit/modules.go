package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	cloudDir      = "/var/lib/cloud"
	outputLogPath = "/var/log/cloud-init-output.log"
	sshDir        = "/etc/ssh"
	rootSSHDir    = "/root/.ssh"
	aptEnv        = "DEBIAN_FRONTEND=noninteractive"
)

var aptOptions = []string{"--option=Dpkg::Options::=--force-confold", "--option=Dpkg::options::=--force-unsafe-io", "--assume-yes", "--quiet"}

type instance struct {
	root    string
	id      string
	meta    metaData
	cfg     cloudConfig
	script  []byte
	log     *ciLog
	output  *os.File
	updated bool
}

func (in *instance) dir(parts ...string) string {
	return filepath.Join(append([]string{in.root, cloudDir, "instances", in.id}, parts...)...)
}

func (in *instance) prepare() error {
	for _, d := range []string{in.dir("sem"), in.dir("scripts"), filepath.Join(in.root, cloudDir, "data")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	link := filepath.Join(in.root, cloudDir, "instance")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join(cloudDir, "instances", in.id), link); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(in.root, cloudDir, "data", "instance-id"), []byte(in.id+"\n"), 0o644)
}

func (in *instance) runModule(ctx context.Context, name string, active, perBoot bool, fn func(context.Context) error) {
	if ctx.Err() != nil {
		return
	}
	if !active {
		in.log.debug("", fmt.Sprintf("Skipping module %s because no applicable config is provided.", name))
		return
	}
	if !perBoot {
		sem := in.dir("sem", "config_"+name)
		if _, err := os.Stat(sem); err == nil {
			in.log.debug("", fmt.Sprintf("config-%s already ran (freq=once-per-instance)", name))
			return
		}
		_ = os.WriteFile(sem, fmt.Appendf(nil, "%d: %.6f\n", os.Getpid(), readUptime()), 0o644)
	}
	in.log.debug("", "Running module "+name)
	if err := fn(ctx); err != nil {
		in.log.fail(name, err)
	}
}

func (in *instance) run(ctx context.Context, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = in.output, in.output
	return cmd.Run()
}

func (in *instance) bootcmd(ctx context.Context) error {
	dir := filepath.Join(in.root, "/run/cloud-init/tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "tmp*.sh")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(shellify(in.cfg.bootcmd))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return in.run(ctx, []string{"INSTANCE_ID=" + in.id}, "/bin/sh", f.Name())
}

func (in *instance) writeFiles(deferred bool) func(context.Context) error {
	return func(context.Context) error {
		plan, notices, err := planWriteFiles(in.cfg.writeFiles, deferred)
		for _, n := range notices {
			in.log.notice("write_files", n)
		}
		if err != nil {
			return err
		}
		for _, w := range plan {
			if err := writeFileEntry(in.root, w); err != nil {
				return fmt.Errorf("write %s: %w", w.path, err)
			}
			in.log.debug("write_files", "Wrote "+w.path)
		}
		return nil
	}
}

var hostKeyTypes = []string{"rsa", "ecdsa", "ed25519"}

func (in *instance) configSSH(ctx context.Context) error {
	dir := filepath.Join(in.root, sshDir)
	if in.cfg.sshDeleteKeys {
		old, _ := filepath.Glob(filepath.Join(dir, "ssh_host_*key*"))
		for _, f := range old {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	var err error
	if in.cfg.hasSSHKeys {
		err = in.writeHostKeys(ctx, dir)
	} else {
		err = in.generateHostKeys(ctx, dir)
	}
	if err != nil {
		return err
	}
	keys, err := in.meta.keys()
	if err != nil {
		return err
	}
	return addAuthorizedKeys(filepath.Join(in.root, rootSSHDir), append(keys, in.cfg.sshAuthorizedKeys...))
}

func (in *instance) writeHostKeys(ctx context.Context, dir string) error {
	names := make([]string, 0, len(in.cfg.sshKeys))
	for name := range in.cfg.sshKeys {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		keyType, kind, _ := strings.Cut(name, "_")
		switch {
		case !slices.Contains(hostKeyTypes, keyType) || (kind != "private" && kind != "public" && kind != "certificate"):
			in.log.warn("ssh", fmt.Sprintf("Skipping unrecognized ssh_keys entry: %q", name))
			continue
		case kind == "certificate":
			in.log.warn("ssh", fmt.Sprintf("Skipping ssh_keys entry %q: the simulated VM does not install host certificates", name))
			continue
		}
		path, mode := hostKeyPath(dir, keyType), os.FileMode(0o600)
		if kind == "public" {
			path, mode = path+".pub", 0o644
		}
		if err := os.WriteFile(path, []byte(in.cfg.sshKeys[name]), mode); err != nil {
			return err
		}
	}
	for _, keyType := range hostKeyTypes {
		_, hasPrivate := in.cfg.sshKeys[keyType+"_private"]
		_, hasPublic := in.cfg.sshKeys[keyType+"_public"]
		if hasPrivate && !hasPublic {
			out, err := exec.CommandContext(ctx, "ssh-keygen", "-yf", hostKeyPath(dir, keyType)).Output()
			if err != nil {
				return fmt.Errorf("derive %s public host key: %w", keyType, err)
			}
			pub := strings.TrimSpace(string(out)) + " root@localhost\n"
			if err := os.WriteFile(hostKeyPath(dir, keyType)+".pub", []byte(pub), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func hostKeyPath(dir, keyType string) string {
	return filepath.Join(dir, "ssh_host_"+keyType+"_key")
}

func (in *instance) generateHostKeys(ctx context.Context, dir string) error {
	types := in.cfg.sshGenKeyTypes
	if types == nil {
		types = defaultKeyTypes
	}
	for _, t := range types {
		path := hostKeyPath(dir, t)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := in.run(ctx, nil, "ssh-keygen", "-t", t, "-N", "", "-f", path); err != nil {
			return fmt.Errorf("generate %s host key: %w", t, err)
		}
	}
	return nil
}

func addAuthorizedKeys(dir string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "authorized_keys")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	have := splitLines(string(existing))
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" && !slices.Contains(have, k) {
			content += k + "\n"
			have = append(have, k)
		}
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func (in *instance) saveUserScript(context.Context) error {
	script := strings.ReplaceAll(string(in.script), "\r\n", "\n")
	return os.WriteFile(in.dir("scripts", "part-001"), []byte(script), 0o700)
}

func (in *instance) saveRuncmd(context.Context) error {
	return os.WriteFile(in.dir("scripts", "runcmd"), []byte(shellify(in.cfg.runcmd)), 0o700)
}

func (in *instance) packages(ctx context.Context) error {
	if in.cfg.packageUpdate {
		if err := in.aptUpdate(ctx); err != nil {
			in.log.write("package_update_upgrade_install", "ERROR", "Failed to update package using apt: "+err.Error())
		}
	}
	var installed, missing []string
	for _, p := range in.cfg.packages {
		switch {
		case in.isInstalled(ctx, p):
			installed = append(installed, p.name)
		case p.version != "":
			missing = append(missing, p.name+"="+p.version)
		default:
			missing = append(missing, p.name)
		}
	}
	if len(installed) > 0 {
		in.log.info("package_update_upgrade_install", "Skipping packages that are already installed: "+strings.Join(installed, ", "))
	}
	if len(missing) == 0 {
		return nil
	}
	if !in.updated {
		if err := in.aptUpdate(ctx); err != nil {
			in.log.write("package_update_upgrade_install", "ERROR", "Failed to update package using apt: "+err.Error())
		}
	}
	args := append(append(slices.Clone(aptOptions), "install"), missing...)
	if err := in.run(ctx, []string{aptEnv}, "apt-get", args...); err != nil {
		return fmt.Errorf("install %s: %w", strings.Join(missing, ", "), err)
	}
	in.log.info("package_update_upgrade_install", "Installed packages: "+strings.Join(missing, ", "))
	return nil
}

func (in *instance) aptUpdate(ctx context.Context) error {
	if err := in.run(ctx, []string{aptEnv}, "apt-get", append(slices.Clone(aptOptions), "update")...); err != nil {
		return err
	}
	in.updated = true
	return nil
}

func (in *instance) isInstalled(ctx context.Context, p packageSpec) bool {
	out, err := exec.CommandContext(ctx, "dpkg-query", "-W", "-f", "${Status}\t${Version}", p.name).Output()
	if err != nil {
		return false
	}
	status, version, _ := strings.Cut(string(out), "\t")
	return status == "install ok installed" && (p.version == "" || version == p.version)
}

func (in *instance) scriptsUser(ctx context.Context) error {
	entries, err := os.ReadDir(in.dir("scripts"))
	if err != nil {
		return err
	}
	var failed []string
	attempted := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := in.dir("scripts", e.Name())
		if info.Mode()&0o111 == 0 {
			in.log.warn("scripts_user", path+" is not executable")
			continue
		}
		attempted++
		if err := in.run(ctx, nil, path); err != nil {
			in.log.warn("scripts_user", fmt.Sprintf("Failed running %s (%v)", path, err))
			failed = append(failed, e.Name())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("runparts: %d failures (%s) in %d attempted commands", len(failed), strings.Join(failed, ","), attempted)
	}
	return nil
}
