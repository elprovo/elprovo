package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// cleanCommand is `cloud-init clean`: it removes the cloud-init state in
// /var/lib/cloud, so the next boot runs every module again, as image builds
// do before a snapshot. The seed stays unless --seed is given; --logs
// removes the logs and --machine-id resets /etc/machine-id.
func cleanCommand(root string, args []string, stdout, stderr io.Writer) int {
	logs, machineID, seed := false, false, false
	for _, a := range args {
		switch a {
		case "-l", "--logs":
			logs = true
		case "--machine-id":
			machineID = true
		case "-s", "--seed":
			seed = true
		default:
			_, _ = fmt.Fprintf(stderr, "cloud-init clean: unsupported argument %s\n", a)
			return 2
		}
	}
	if err := cleanArtifacts(root, logs, machineID, seed); err != nil {
		_, _ = fmt.Fprintln(stderr, "cloud-init clean:", err)
		return 1
	}
	return 0
}

func cleanArtifacts(root string, logs, machineID, seed bool) error {
	dir := filepath.Join(root, cloudDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.Name() == "seed" && !seed {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if logs {
		for _, path := range []string{ciLogPath, outputLogPath} {
			if err := os.Remove(filepath.Join(root, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if machineID {
		if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte("uninitialized\n"), 0o444); err != nil {
			return err
		}
	}
	return nil
}
