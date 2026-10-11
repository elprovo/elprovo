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
	"strconv"
	"strings"
	"syscall"
)

const (
	firewallState = "/var/lib/stagely-vm/firewall.json"
	inboundChain  = "STAGELY-FIREWALL-IN"
	outboundChain = "STAGELY-FIREWALL-OUT"
)

type firewallPolicy struct {
	Inbound  []firewallRule `json:"inbound"`
	Outbound []firewallRule `json:"outbound"`
}

type firewallRule struct {
	Protocol string   `json:"protocol"`
	Ports    string   `json:"ports"`
	Peers    []string `json:"peers"`
}

func firewallCommand(ctx context.Context, root string, stdin io.Reader, stderr io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err == nil {
		err = withFirewallLock(root, func() error {
			if err := applyFirewall(ctx, data); err != nil {
				return err
			}
			return writeFileAtomic(filepath.Join(root, firewallState), data, 0o600)
		})
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "firewall:", err)
		return 1
	}
	return 0
}

func restoreFirewall(ctx context.Context, root string) error {
	return withFirewallLock(root, func() error {
		data, err := os.ReadFile(filepath.Join(root, firewallState))
		if errors.Is(err, os.ErrNotExist) {
			data, err = []byte("null"), nil
		}
		if err != nil {
			return err
		}
		return applyFirewall(ctx, data)
	})
}

func withFirewallLock(root string, fn func() error) error {
	return withStateLock(root, firewallState, fn)
}

// withStateLock runs fn while it holds the lock of a state file, so a push
// from the cloud and the boot never apply the same state at once.
func withStateLock(root, state string, fn func() error) error {
	path := filepath.Join(root, state+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", state, err)
	}
	return fn()
}

func applyFirewall(ctx context.Context, data []byte) error {
	var policy *firewallPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return fmt.Errorf("parse policy: %w", err)
	}
	rules, err := iptablesRules(policy)
	if err != nil {
		return err
	}
	restore := exec.CommandContext(ctx, "iptables-restore", "--noflush")
	restore.Stdin = strings.NewReader(rules)
	if out, err := restore.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore: %w: %s", err, out)
	}
	for chain, hook := range map[string]string{inboundChain: "PREROUTING", outboundChain: "POSTROUTING"} {
		if exec.CommandContext(ctx, "iptables", "-t", "mangle", "-C", hook, "-j", chain).Run() == nil {
			continue
		}
		if out, err := exec.CommandContext(ctx, "iptables", "-t", "mangle", "-I", hook, "1", "-j", chain).CombinedOutput(); err != nil {
			return fmt.Errorf("hook %s into %s: %w: %s", chain, hook, err, out)
		}
	}
	return nil
}

func iptablesRules(policy *firewallPolicy) (string, error) {
	var b strings.Builder
	b.WriteString("*mangle\n:" + inboundChain + " - [0:0]\n:" + outboundChain + " - [0:0]\n")
	if policy != nil {
		if err := writeChain(&b, inboundChain, "-i", "-s", policy.Inbound); err != nil {
			return "", err
		}
		if err := writeChain(&b, outboundChain, "-o", "-d", policy.Outbound); err != nil {
			return "", err
		}
	}
	b.WriteString("COMMIT\n")
	return b.String(), nil
}

func writeChain(b *strings.Builder, chain, ifaceFlag, peerFlag string, rules []firewallRule) error {
	fmt.Fprintf(b, "-A %s ! %s eth+ -j RETURN\n", chain, ifaceFlag)
	fmt.Fprintf(b, "-A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n", chain)
	for _, r := range rules {
		match, err := ruleMatch(r)
		if err != nil {
			return err
		}
		for _, peer := range r.Peers {
			prefix, err := parsePeer(peer)
			if err != nil {
				return err
			}
			fmt.Fprintf(b, "-A %s %s %s %s -j RETURN\n", chain, match, peerFlag, prefix)
		}
	}
	fmt.Fprintf(b, "-A %s -j DROP\n", chain)
	return nil
}

func ruleMatch(r firewallRule) (string, error) {
	switch r.Protocol {
	case "icmp":
		if r.Ports != "" {
			return "", errors.New("ICMP rules take no ports")
		}
		return "-p icmp", nil
	case "tcp", "udp":
		if r.Ports == "" {
			return "-p " + r.Protocol, nil
		}
		ports, err := portRange(r.Ports)
		if err != nil {
			return "", err
		}
		return "-p " + r.Protocol + " --dport " + ports, nil
	default:
		return "", fmt.Errorf("protocol %q is not tcp, udp or icmp", r.Protocol)
	}
}

func portRange(ports string) (string, error) {
	low, high, isRange := strings.Cut(ports, "-")
	if !isRange {
		high = low
	}
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return "", fmt.Errorf("invalid ports %q", ports)
	}
	if lo == hi {
		return strconv.Itoa(lo), nil
	}
	return strconv.Itoa(lo) + ":" + strconv.Itoa(hi), nil
}

func parsePeer(peer string) (string, error) {
	if prefix, err := netip.ParsePrefix(peer); err == nil && prefix.Addr().Is4() {
		return prefix.Masked().String(), nil
	}
	if addr, err := netip.ParseAddr(peer); err == nil && addr.Is4() {
		return addr.String() + "/32", nil
	}
	return "", fmt.Errorf("peer %q is not an IPv4 address or CIDR block", peer)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
