package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanRemovesStateAndKeepsTheSeed(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"var/lib/cloud/seed/nocloud/user-data", "var/lib/cloud/instances/7/sem/config_runcmd", ciLogPath, outputLogPath, "etc/machine-id"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if code := cleanCommand(root, []string{"--logs", "--machine-id"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("clean exited %d", code)
	}

	if _, err := os.Stat(filepath.Join(root, "var/lib/cloud/seed/nocloud/user-data")); err != nil {
		t.Fatalf("the seed is gone: %v", err)
	}
	for _, path := range []string{"var/lib/cloud/instances", ciLogPath, outputLogPath} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after clean: %v", path, err)
		}
	}
	if id, _ := os.ReadFile(filepath.Join(root, "etc/machine-id")); string(id) != "uninitialized\n" {
		t.Fatalf("machine-id = %q, want uninitialized", id)
	}
}
