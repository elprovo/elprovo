package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSupervisor(t *testing.T) (*supervisor, string) {
	t.Helper()
	root := t.TempDir()
	sockets, err := os.MkdirTemp("", "vmi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	sup := newSupervisor(root, sockets, newJournal(filepath.Join(root, unitLogDir), "vm1"))
	t.Cleanup(sup.stopAll)
	return sup, root
}

func writeUnit(t *testing.T, root, name, text string) {
	t.Helper()
	dir := filepath.Join(root, unitDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unitState(sup *supervisor, name string) string {
	snap, _ := sup.snapshot(name)
	return snap.state
}

func TestSimpleUnitRunsUntilStoppedAndLogsItsOutput(t *testing.T) {
	sup, root := newTestSupervisor(t)
	writeUnit(t, root, "app.service", "[Unit]\nDescription=App\n[Service]\nExecStart=/bin/sh -c 'echo started; exec sleep 60'\n")

	if err := sup.start("app.service"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := unitState(sup, "app.service"); got != "active" {
		t.Fatalf("state after start = %q, want active", got)
	}
	logPath := filepath.Join(root, unitLogDir, "app.service.log")
	eventually(t, "the unit's output is logged", func() bool {
		data, _ := os.ReadFile(logPath)
		return bytes.Contains(data, []byte("vm1 sh[")) && bytes.Contains(data, []byte("]: started"))
	})
	if err := sup.stop("app.service"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := unitState(sup, "app.service"); got != "inactive" {
		t.Fatalf("state after stop = %q, want inactive", got)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "systemd[1]: Stopped app.service - App.") {
		t.Fatalf("log lacks the stop line:\n%s", data)
	}
}

func TestStateDirectoryExistsBeforeTheServiceStarts(t *testing.T) {
	sup, root := newTestSupervisor(t)
	writeUnit(t, root, "app.service", "[Service]\nStateDirectory=app\nStateDirectoryMode=0700\nExecStart=/bin/sh -c 'echo dir=$STATE_DIRECTORY; exec sleep 60'\n")

	if err := sup.start("app.service"); err != nil {
		t.Fatalf("start: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "var/lib/app"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory = %v, %v; want a 0700 directory", info, err)
	}
	logPath := filepath.Join(root, unitLogDir, "app.service.log")
	eventually(t, "the service sees STATE_DIRECTORY", func() bool {
		data, _ := os.ReadFile(logPath)
		return bytes.Contains(data, []byte("dir=/var/lib/app"))
	})
}

func TestOnFailureUnitRestartsAfterItExitsWithAnError(t *testing.T) {
	sup, root := newTestSupervisor(t)
	marker := filepath.Join(root, "runs")
	writeUnit(t, root, "flaky.service", "[Service]\nExecStart=/bin/sh -c 'echo run >> "+marker+"; exit 3'\nRestart=on-failure\nRestartSec=10ms\n")

	if err := sup.start("flaky.service"); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, "the unit ran three times", func() bool {
		data, _ := os.ReadFile(marker)
		return strings.Count(string(data), "run") >= 3
	})
}

func TestUnitThatExitsCleanlyWithoutRestartBecomesInactive(t *testing.T) {
	sup, root := newTestSupervisor(t)
	writeUnit(t, root, "once.service", "[Service]\nExecStart=/bin/sh -c 'exit 0'\nRestart=on-failure\n")
	if err := sup.start("once.service"); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, "the unit is inactive", func() bool { return unitState(sup, "once.service") == "inactive" })
}

func TestFailingStartPreFailsTheStart(t *testing.T) {
	sup, root := newTestSupervisor(t)
	writeUnit(t, root, "app.service", "[Service]\nExecStartPre=/bin/sh -c 'exit 1'\nExecStart=/bin/sleep 60\n")
	err := sup.start("app.service")
	if err == nil || !strings.Contains(err.Error(), "Job for app.service failed") {
		t.Fatalf("start error = %v, want systemctl's job failure message", err)
	}
	if got := unitState(sup, "app.service"); got != "failed" {
		t.Fatalf("state = %q, want failed", got)
	}
}

func TestOneshotWithRemainAfterExitStaysActive(t *testing.T) {
	sup, root := newTestSupervisor(t)
	marker := filepath.Join(root, "done")
	writeUnit(t, root, "setup.service", "[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/touch "+marker+"\n")

	if err := sup.start("setup.service"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("start returned before the oneshot command finished: %v", err)
	}
	snap, _ := sup.snapshot("setup.service")
	if snap.state != "active" || snap.sub != "exited" {
		t.Fatalf("state = %s (%s), want active (exited)", snap.state, snap.sub)
	}
}

func TestNotifyUnitIsActiveOnlyAfterReady(t *testing.T) {
	sup, root := newTestSupervisor(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeUnit(t, root, "ready.service", "[Service]\nType=notify\nEnvironment=VMINIT_NOTIFY_HELPER=1\nExecStart="+exe+" -test.run=TestNotifyHelper\n")

	if err := sup.start("ready.service"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := unitState(sup, "ready.service"); got != "active" {
		t.Fatalf("state = %q, want active after READY=1", got)
	}
}

// TestNotifyHelper is not a test: TestNotifyUnitIsActiveOnlyAfterReady runs
// the test binary as a Type=notify service, which reports READY=1 here.
func TestNotifyHelper(t *testing.T) {
	if os.Getenv("VMINIT_NOTIFY_HELPER") != "1" {
		t.Skip("runs only as a unit process")
	}
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unixgram", os.Getenv("NOTIFY_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("STATUS=starting\nREADY=1"))
	time.Sleep(time.Minute)
}

func TestStartingAMissingUnitReportsNotFound(t *testing.T) {
	sup, _ := newTestSupervisor(t)
	resp := sup.handle(controlRequest{Verb: "start", Units: []string{"nope.service"}})
	if resp.Code != 5 || resp.Stderr != "Failed to start nope.service: Unit nope.service not found.\n" {
		t.Fatalf("response = %+v", resp)
	}
}
