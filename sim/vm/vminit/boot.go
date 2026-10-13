package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const rfc2822 = "Mon, 02 Jan 2006 15:04:05 -0700"

func boot() error {
	if err := enableCgroupNesting(); err != nil {
		fmt.Fprintln(os.Stderr, "elprovo-vm-init: Docker inside the machine may fail: enable cgroup nesting:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	hostname, _ := os.Hostname()
	sup := newSupervisor("/", runDir, newJournal(unitLogDir, hostname))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	socket := filepath.Join(runDir, "control.sock")
	_ = os.Remove(socket)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return err
	}
	go sup.serveControl(ln)
	if err := restoreFirewall(ctx, "/"); err != nil {
		fmt.Fprintln(os.Stderr, "elprovo-vm-init: apply the kept firewall:", err)
	}
	if err := restoreAddresses(ctx, "/"); err != nil {
		fmt.Fprintln(os.Stderr, "elprovo-vm-init: add the kept extra addresses:", err)
	}
	if err := trustSimulationCA(ctx, "/"); err != nil {
		fmt.Fprintln(os.Stderr, "elprovo-vm-init: trust the simulation's CA certificates:", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCloudInit(ctx, "/", func() { startBootUnits(ctx, sup) })
	}()
	<-ctx.Done()
	_ = ln.Close()
	sup.stopAll()
	<-done
	return nil
}

func enableCgroupNesting() error {
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return nil
	}
	if err := os.MkdirAll("/sys/fs/cgroup/init", 0o755); err != nil {
		return err
	}
	procs, err := os.ReadFile("/sys/fs/cgroup/cgroup.procs")
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(procs)) {
		_ = os.WriteFile("/sys/fs/cgroup/init/cgroup.procs", []byte(pid), 0o644)
	}
	var enable []string
	for _, c := range strings.Fields(string(controllers)) {
		enable = append(enable, "+"+c)
	}
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte(strings.Join(enable, " ")), 0o644)
}

func startBootUnits(ctx context.Context, sup *supervisor) {
	for _, name := range enabledUnits("/") {
		if ctx.Err() != nil {
			return
		}
		if err := sup.start(name); err != nil {
			fmt.Fprintf(os.Stderr, "elprovo-vm-init: start %s: %v\n", name, err)
		}
	}
}

func runCloudInit(ctx context.Context, root string, startUnits func()) {
	status := newStatusFile(root, readUptime)
	output := openLog(root, outputLogPath)
	defer func() { _ = output.Close() }()
	file := openLog(root, ciLogPath)
	defer func() { _ = file.Close() }()
	log := &ciLog{file: file, output: output, status: status}

	stage(status, output, "init-local", "init-local")
	meta, data, seedErr := readSeed(ctx, root)
	if seedErr == nil {
		status.setDatasource(datasourceName)
	}
	status.finish()

	stage(status, output, "init", "init")
	in := &instance{root: root, id: meta.InstanceID, meta: meta, script: data.script, log: log, output: output}
	if seedErr != nil {
		log.write("", "ERROR", seedErr.Error())
		status.fail("No instance datasource found.")
		in.id = "nocloud"
	}
	in.cfg = in.configure(data)
	if err := in.prepare(); err != nil {
		log.fail("init", err)
	}
	in.runModule(ctx, "consume_data", in.script != nil, false, in.saveUserScript)
	in.runModule(ctx, "bootcmd", len(in.cfg.bootcmd) > 0, true, in.bootcmd)
	in.runModule(ctx, "write_files", in.cfg.hasWriteFiles(false), false, in.writeFiles(false))
	in.runModule(ctx, "ssh", true, false, in.configSSH)
	status.finish()

	startUnits()

	stage(status, output, "modules-config", "modules:config")
	in.runModule(ctx, "runcmd", len(in.cfg.runcmd) > 0, false, in.saveRuncmd)
	status.finish()

	stage(status, output, "modules-final", "modules:final")
	in.runModule(ctx, "package_update_upgrade_install", in.cfg.packageUpdate || len(in.cfg.packages) > 0, false, in.packages)
	in.runModule(ctx, "write_files_deferred", in.cfg.hasWriteFiles(true), false, in.writeFiles(true))
	in.runModule(ctx, "scripts_user", true, false, in.scriptsUser)
	if ctx.Err() != nil {
		return
	}
	in.finalMessage()
	status.finish()
	status.writeResult()
}

func (in *instance) configure(data userData) cloudConfig {
	for _, n := range data.notices {
		in.log.notice("", n)
	}
	if data.cloudConfig == nil {
		return cloudConfig{sshDeleteKeys: true}
	}
	cfg, notices, err := decodeCloudConfig(data.cloudConfig)
	if err != nil {
		in.log.warn("", "Failed at merging in cloud config part from part-001: "+err.Error())
	}
	for _, n := range notices {
		in.log.notice("", n)
	}
	return cfg
}

func (in *instance) finalMessage() {
	up, now := readUptime(), time.Now().UTC().Format(rfc2822)
	msg := fmt.Sprintf("Cloud-init v. %s finished at %s. Datasource %s.  Up %.2f seconds", ciVersion, now, datasourceName, up)
	_, _ = fmt.Fprintln(in.output, msg)
	fmt.Fprintln(os.Stderr, msg)
	in.log.debug("final_message", msg)
	_ = os.WriteFile(in.dir("boot-finished"), fmt.Appendf(nil, "%.2f - %s - v. %s\n", up, now, ciVersion), 0o644)
}

func stage(status *statusFile, output io.Writer, name, title string) {
	status.begin(name)
	_, _ = fmt.Fprintf(output, "Cloud-init v. %s running '%s' at %s. Up %.2f seconds.\n", ciVersion, title, time.Now().UTC().Format(rfc2822), readUptime())
}

func openLog(root, path string) *os.File {
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err == nil {
		if f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
			return f
		}
	}
	f, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	return f
}

// simulationCA is the PEM bundle that the simulation's -machine-ca names, as
// the simulation copies it into the machine.
const simulationCA = "usr/local/share/ca-certificates/elprovo-sim.crt"

// trustSimulationCA adds the simulation's CA certificates to the machine's
// trust store before any service starts, so services trust them as they trust
// the public CAs on a cloud image.
func trustSimulationCA(ctx context.Context, root string) error {
	if _, err := os.Stat(filepath.Join(root, simulationCA)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	out, err := exec.CommandContext(ctx, "update-ca-certificates").CombinedOutput()
	if err != nil {
		return fmt.Errorf("update-ca-certificates: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
