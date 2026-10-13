package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// A cloud's reserved IP reaches a machine as an extra address on its public
// interface. The cloud pushes the machine's whole list with
// `elprovo-vm-init addresses`, and the machine keeps it for later boots.
const addressesState = "var/lib/elprovo-vm/addresses.json"

type extraAddresses struct {
	Addresses []string `json:"addresses"`
}

// addressesCommand reads the list of extra addresses on stdin, removes the
// ones that the machine no longer has, adds the new ones and keeps the list.
func addressesCommand(ctx context.Context, root string, stdin io.Reader, stderr io.Writer) int {
	err := func() error {
		data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return err
		}
		want, err := parseAddresses(data)
		if err != nil {
			return err
		}
		return withStateLock(root, addressesState, func() error {
			current, err := keptAddresses(root)
			if err != nil {
				return err
			}
			iface, err := publicInterface(ctx, root)
			if err != nil {
				return err
			}
			remove, add := diffAddresses(current, want)
			for _, addr := range remove {
				if err := ipAddress(ctx, "del", addr, iface); err != nil {
					return err
				}
			}
			for _, addr := range add {
				if err := addAddress(ctx, addr, iface); err != nil {
					return err
				}
			}
			kept, err := json.Marshal(extraAddresses{Addresses: want})
			if err != nil {
				return err
			}
			return writeFileAtomic(filepath.Join(root, addressesState), kept, 0o600)
		})
	}()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "addresses:", err)
		return 1
	}
	return 0
}

// restoreAddresses adds the kept extra addresses at boot, when the machine's
// interfaces are new.
func restoreAddresses(ctx context.Context, root string) error {
	return withStateLock(root, addressesState, func() error {
		addrs, err := keptAddresses(root)
		if err != nil || len(addrs) == 0 {
			return err
		}
		iface, err := publicInterface(ctx, root)
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			if err := addAddress(ctx, addr, iface); err != nil {
				return err
			}
		}
		return nil
	})
}

func parseAddresses(data []byte) ([]string, error) {
	var list extraAddresses
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse addresses: %w", err)
	}
	for _, a := range list.Addresses {
		if addr, err := netip.ParseAddr(a); err != nil || !addr.Is4() {
			return nil, fmt.Errorf("%q is not an IPv4 address", a)
		}
	}
	return list.Addresses, nil
}

func keptAddresses(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, addressesState))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseAddresses(data)
}

// diffAddresses returns the addresses in current but not in want, and the
// ones in want but not in current.
func diffAddresses(current, want []string) (remove, add []string) {
	for _, a := range current {
		if !slices.Contains(want, a) {
			remove = append(remove, a)
		}
	}
	for _, a := range want {
		if !slices.Contains(current, a) {
			add = append(add, a)
		}
	}
	return remove, add
}

// publicInterface returns the interface that holds the machine's public
// address from its meta-data.
func publicInterface(ctx context.Context, root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, seedDir, "meta-data"))
	if err != nil {
		return "", fmt.Errorf("read meta-data: %w", err)
	}
	var meta metaData
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("meta-data: %w", err)
	}
	out, err := exec.CommandContext(ctx, "ip", "-4", "-o", "addr", "show").Output()
	if err != nil {
		return "", fmt.Errorf("list addresses: %w", err)
	}
	if iface := interfaceWithAddress(string(out), meta.PublicIPv4); iface != "" {
		return iface, nil
	}
	return "", fmt.Errorf("no interface has the public address %q", meta.PublicIPv4)
}

// interfaceWithAddress finds the interface in `ip -4 -o addr show` output
// that holds addr.
func interfaceWithAddress(ipOutput, addr string) string {
	for _, line := range strings.Split(ipOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[2] == "inet" && strings.HasPrefix(fields[3], addr+"/") {
			return fields[1]
		}
	}
	return ""
}

// addAddress adds addr to iface and announces it with a gratuitous ARP, so
// neighbors that knew the address at another MAC address, such as this
// machine's interface before a restart, send to it at once.
func addAddress(ctx context.Context, addr, iface string) error {
	if err := ipAddress(ctx, "add", addr, iface); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "arping", "-U", "-c", "2", "-I", iface, addr).CombinedOutput(); err != nil {
		return fmt.Errorf("announce %s on %s: %w: %s", addr, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ipAddress adds or deletes addr as a /32 on iface. Adding an address that
// is there, or deleting one that is not, succeeds.
func ipAddress(ctx context.Context, verb, addr, iface string) error {
	out, err := exec.CommandContext(ctx, "ip", "addr", verb, addr+"/32", "dev", iface).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err == nil || (verb == "add" && strings.Contains(text, "File exists")) || (verb == "del" && strings.Contains(text, "Cannot assign")) {
		return nil
	}
	return fmt.Errorf("ip addr %s %s dev %s: %w: %s", verb, addr, iface, err, text)
}
