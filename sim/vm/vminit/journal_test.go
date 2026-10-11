package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalctlPrintsTheLastLinesOfAUnit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, unitLogDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.service.log"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer

	if code := journalctlCommand(root, []string{"-u", "app", "-n", "2", "--no-pager"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.String() != "two\nthree\n" {
		t.Fatalf("output = %q, want the last two lines", stdout.String())
	}
}

func TestJournalctlWithoutEntriesSaysSo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := journalctlCommand(t.TempDir(), []string{"--unit=app.service"}, &stdout, &stderr); code != 0 || stdout.String() != "-- No entries --\n" {
		t.Fatalf("output = %q, exit %d", stdout.String(), code)
	}
}

func TestJournalctlNeedsOneUnit(t *testing.T) {
	for _, args := range [][]string{{}, {"-u", "a", "-u", "b"}, {"-u", "a", "-b"}} {
		if _, err := parseJournalctlArgs(args); err == nil {
			t.Errorf("parseJournalctlArgs(%q) succeeded, want an error", args)
		}
	}
}

func TestUnitLogKeepsReadingAfterAVeryLongLine(t *testing.T) {
	j := newJournal(t.TempDir(), "vm1")
	log := j.open("app.service")
	long := strings.Repeat("x", 200*1024)

	log.copyLines(io.NopCloser(strings.NewReader(long+"\nafter\n")), "app", 7)

	data, err := os.ReadFile(filepath.Join(j.dir, "app.service.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "x") != len(long) || !strings.HasSuffix(string(data), "vm1 app[7]: after\n") {
		t.Fatalf("log has %d x and ends %q", strings.Count(string(data), "x"), data[len(data)-30:])
	}
}
