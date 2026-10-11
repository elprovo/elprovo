// Command stagely-vm-init boots a simulated cloud VM: a Docker container
// that behaves like a fresh Ubuntu cloud VM. It runs under Docker's init
// (tini), applies NoCloud seed data the way cloud-init does, supervises
// systemd-style units, and enforces the cloud firewall that the simulation
// pushes, and serves the machine's instance metadata. The same binary answers
// as cloud-init, systemctl and journalctl, chosen by the name it runs under.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "cloud-init":
		os.Exit(cloudInitCommand("/", os.Args[1:], os.Stdout, os.Stderr))
	case "systemctl":
		os.Exit(systemctlCommand("/", filepath.Join(runDir, "control.sock"), os.Args[1:], os.Stdout, os.Stderr))
	case "journalctl":
		os.Exit(journalctlCommand("/", os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "firewall":
			os.Exit(firewallCommand(context.Background(), "/", os.Stdin, os.Stderr))
		case "addresses":
			os.Exit(addressesCommand(context.Background(), "/", os.Stdin, os.Stderr))
		case "metadata-net":
			os.Exit(metadataNetCommand(context.Background(), os.Stderr))
		case "metadata-serve":
			os.Exit(metadataServeCommand("/", os.Stderr))
		}
	}
	if err := boot(); err != nil {
		fmt.Fprintln(os.Stderr, "stagely-vm-init:", err)
		os.Exit(1)
	}
}
