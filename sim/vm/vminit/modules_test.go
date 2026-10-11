package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newTestInstance(t *testing.T) *instance {
	t.Helper()
	root := t.TempDir()
	output, err := os.Create(filepath.Join(root, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	status := newStatusFile(root, fakeUptime())
	status.begin("modules-final")
	in := &instance{root: root, id: "i-1", log: &ciLog{file: io.Discard, output: io.Discard, status: status}, output: output}
	if err := in.prepare(); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestPerInstanceModuleRunsOnceEvenAfterFailing(t *testing.T) {
	in := newTestInstance(t)
	runs := 0
	fail := func(context.Context) error { runs++; return errors.New("boom") }

	in.runModule(t.Context(), "runcmd", true, false, fail)
	in.runModule(t.Context(), "runcmd", true, false, fail)

	if runs != 1 {
		t.Fatalf("module ran %d times, want 1", runs)
	}
	errs := in.log.status.status.stages["modules-final"].Errors
	if len(errs) != 1 || errs[0] != "('runcmd', 'boom')" {
		t.Fatalf("stage errors = %q", errs)
	}
}

func TestPerBootModuleRunsEveryTimeAndInactiveModuleNever(t *testing.T) {
	in := newTestInstance(t)
	runs := 0
	count := func(context.Context) error { runs++; return nil }

	in.runModule(t.Context(), "bootcmd", true, true, count)
	in.runModule(t.Context(), "bootcmd", true, true, count)
	in.runModule(t.Context(), "write_files", false, false, count)

	if runs != 2 {
		t.Fatalf("modules ran %d times, want 2", runs)
	}
	if _, err := os.Stat(in.dir("sem", "config_write_files")); err == nil {
		t.Fatal("an inactive module left a semaphore")
	}
}

func TestScriptsUserRunsScriptsInNameOrderAndReportsFailures(t *testing.T) {
	in := newTestInstance(t)
	trace := filepath.Join(in.root, "trace")
	for name, body := range map[string]string{
		"part-001": "#!/bin/sh\necho part >> " + trace + "\n",
		"runcmd":   "#!/bin/sh\necho runcmd >> " + trace + "\nexit 1\n",
	} {
		if err := os.WriteFile(in.dir("scripts", name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	err := in.scriptsUser(t.Context())

	if err == nil || err.Error() != "runparts: 1 failures (runcmd) in 2 attempted commands" {
		t.Fatalf("err = %v", err)
	}
	data, _ := os.ReadFile(trace)
	if string(data) != "part\nruncmd\n" {
		t.Fatalf("trace = %q, want part-001 before runcmd", data)
	}
}

func TestAddAuthorizedKeysAppendsOnlyNewKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ssh")
	if err := addAuthorizedKeys(dir, []string{"ssh-ed25519 AAA one"}); err != nil {
		t.Fatal(err)
	}
	if err := addAuthorizedKeys(dir, []string{"ssh-ed25519 AAA one", "ssh-ed25519 BBB two"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "authorized_keys"))
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); !slices.Equal(got, []string{"ssh-ed25519 AAA one", "ssh-ed25519 BBB two"}) {
		t.Fatalf("authorized_keys = %q", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "authorized_keys"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestUserDataFormatsAreClassifiedByTheirFirstLine(t *testing.T) {
	if ud := classifyUserData(t.Context(), []byte("  #!/bin/bash\necho hi\n")); ud.script == nil {
		t.Fatal("a #! script after leading space was not recognized")
	}
	ud := classifyUserData(t.Context(), []byte("Content-Type: multipart/mixed; boundary=x\n"))
	if ud.script != nil || ud.cloudConfig != nil || len(ud.notices) != 1 || ud.notices[0].level != "WARNING" {
		t.Fatalf("MIME user data = %+v, want one warning and nothing applied", ud)
	}
	if ud := classifyUserData(t.Context(), []byte("\n")); ud.script != nil || len(ud.notices) != 0 {
		t.Fatalf("empty user data = %+v, want nothing", ud)
	}
}

func TestMetaDataPublicKeysMayBeAStringOrAList(t *testing.T) {
	for raw, want := range map[string][]string{
		`"ssh-ed25519 A\nssh-ed25519 B\n"`: {"ssh-ed25519 A", "ssh-ed25519 B"},
		`["ssh-ed25519 A"]`:                {"ssh-ed25519 A"},
		``:                                 nil,
	} {
		got, err := metaData{PublicKeys: []byte(raw)}.keys()
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("keys(%s) = %q, %v", raw, got, err)
		}
	}
}

func TestModulesStopRunningOnceShutdownBegins(t *testing.T) {
	in := newTestInstance(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false

	in.runModule(ctx, "scripts_user", true, false, func(context.Context) error { ran = true; return nil })

	if ran {
		t.Fatal("a module ran after shutdown began")
	}
	if _, err := os.Stat(in.dir("sem", "config_scripts_user")); err == nil {
		t.Fatal("a module that did not run left a semaphore, so it would never run")
	}
}
