package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableLinksUnitIntoItsTargetAndDisableRemovesIt(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, "app.service", "[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n")
	var out bytes.Buffer

	if err := enableUnit(root, "app.service", &out); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := enabledUnits(root); len(got) != 1 || got[0] != "app.service" {
		t.Fatalf("enabled units = %q, want app.service", got)
	}
	if !strings.Contains(out.String(), "Created symlink /etc/systemd/system/multi-user.target.wants/app.service") {
		t.Fatalf("enable output = %q", out.String())
	}
	if err := disableUnit(root, "app.service", &out); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := enabledUnits(root); len(got) != 0 {
		t.Fatalf("enabled units after disable = %q, want none", got)
	}
}

func TestSystemctlStartsUnitThroughTheControlSocket(t *testing.T) {
	sup, root := newTestSupervisor(t)
	writeUnit(t, root, "app.service", "[Service]\nExecStart=/bin/sleep 60\n[Install]\nWantedBy=multi-user.target\n")
	socket := filepath.Join(sup.runDir, "control.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go sup.serveControl(ln)
	var stdout, stderr bytes.Buffer

	if code := systemctlCommand(root, socket, []string{"enable", "--now", "app"}, &stdout, &stderr); code != 0 {
		t.Fatalf("systemctl enable --now exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := systemctlCommand(root, socket, []string{"is-active", "app.service"}, &stdout, &stderr); code != 0 || stdout.String() != "active\n" {
		t.Fatalf("is-active = %q exit %d, want active and 0", stdout.String(), code)
	}
	stdout.Reset()
	code := systemctlCommand(root, socket, []string{"status", "app"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Active: active (running)") || !strings.Contains(stdout.String(), "enabled;") {
		t.Fatalf("status exit %d:\n%s", code, stdout.String())
	}
	if code := systemctlCommand(root, socket, []string{"stop", "app"}, &stdout, &stderr); code != 0 {
		t.Fatalf("stop exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := systemctlCommand(root, socket, []string{"is-active", "app"}, &stdout, &stderr); code != 3 || stdout.String() != "inactive\n" {
		t.Fatalf("is-active after stop = %q exit %d, want inactive and 3", stdout.String(), code)
	}
}

func TestSystemctlStatusOfUnknownUnitExitsFour(t *testing.T) {
	sup, _ := newTestSupervisor(t)
	_, stderr, code := sup.status("nope.service")
	if code != 4 || stderr != "Unit nope.service could not be found.\n" {
		t.Fatalf("status = %q, %d", stderr, code)
	}
}

func TestSystemctlWithoutSupervisorExplainsWhy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := systemctlCommand(t.TempDir(), filepath.Join(os.TempDir(), "no-such-vmi.sock"), []string{"start", "app"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "supervisor is not running") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestSystemctlRejectsUnsupportedVerbsAndOptions(t *testing.T) {
	for _, args := range [][]string{{"list-units"}, {"start", "--user", "app"}, {"mask", "app"}} {
		var stdout, stderr bytes.Buffer
		if code := systemctlCommand(t.TempDir(), "unused.sock", args, &stdout, &stderr); code != 1 {
			t.Errorf("systemctl %q exit %d, want 1", args, code)
		}
	}
}
