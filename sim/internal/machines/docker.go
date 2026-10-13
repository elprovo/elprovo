package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"strings"
)

func (b *Backend) docker(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	msg := tail(strings.TrimSpace(stderr.String()), 2000)
	if strings.Contains(msg, "No such container") || strings.Contains(msg, "No such object") || strings.Contains(msg, "No such image") {
		return out, fmt.Errorf("docker %s: %w: %s", args[0], ErrNotFound, msg)
	}
	return out, fmt.Errorf("docker %s: %w: %s", args[0], err, msg)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// EnsureNetworks creates the private and public networks when missing.
func (b *Backend) EnsureNetworks(ctx context.Context) error {
	for _, name := range []string{b.network, b.publicNetwork} {
		if _, err := b.docker(ctx, nil, "network", "inspect", name); err == nil {
			continue
		}
		if _, err := b.docker(ctx, nil, "network", "create", "--label", "elprovo-sim.network=true", name); err != nil {
			if _, again := b.docker(ctx, nil, "network", "inspect", name); again != nil {
				return err
			}
		}
	}
	return nil
}

type networkJSON struct {
	IPAM struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
}

func (b *Backend) usedAddresses(ctx context.Context) (map[string]map[netip.Addr]bool, error) {
	used := map[string]map[netip.Addr]bool{b.network: {}, b.publicNetwork: {}}
	out, err := b.docker(ctx, nil, "ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+labelCloud)
	if err != nil {
		return nil, err
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		machines, err := b.inspectExisting(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, m := range machines {
			markUsed(used[b.network], m.PrivateIP)
			markUsed(used[b.publicNetwork], m.PublicIP)
		}
	}
	for network, addrs := range used {
		n, err := b.inspectNetwork(ctx, network)
		if err != nil {
			return nil, err
		}
		for _, c := range n.Containers {
			markUsed(addrs, strings.Split(c.IPv4Address, "/")[0])
		}
	}
	return used, nil
}

// inspectExisting inspects the machines with the ids and leaves out those
// that are gone by then: the simulations sharing the Docker host delete
// machines at any time.
func (b *Backend) inspectExisting(ctx context.Context, ids []string) ([]Machine, error) {
	machines, err := b.inspect(ctx, ids...)
	if !errors.Is(err, ErrNotFound) {
		return machines, err
	}
	machines = nil
	for _, id := range ids {
		m, err := b.inspect(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		machines = append(machines, m...)
	}
	return machines, nil
}

func markUsed(set map[netip.Addr]bool, ip string) {
	if addr, err := netip.ParseAddr(ip); err == nil {
		set[addr] = true
	}
}

func (b *Backend) inspectNetwork(ctx context.Context, name string) (networkJSON, error) {
	out, err := b.docker(ctx, nil, "network", "inspect", name)
	if err != nil {
		return networkJSON{}, err
	}
	var networks []networkJSON
	if err := json.Unmarshal(out, &networks); err != nil {
		return networkJSON{}, fmt.Errorf("parse docker network inspect %s: %w", name, err)
	}
	if len(networks) != 1 {
		return networkJSON{}, fmt.Errorf("docker network inspect %s returned %d networks", name, len(networks))
	}
	return networks[0], nil
}

func (b *Backend) subnet(ctx context.Context, network string) (Subnet, error) {
	n, err := b.inspectNetwork(ctx, network)
	if err != nil {
		return Subnet{}, err
	}
	for _, cfg := range n.IPAM.Config {
		prefix, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		gateway, _ := netip.ParseAddr(cfg.Gateway)
		return Subnet{Prefix: prefix, Gateway: gateway}, nil
	}
	return Subnet{}, fmt.Errorf("network %s has no IPv4 subnet", network)
}

func (b *Backend) freeAddress(ctx context.Context, network string, used map[string]map[netip.Addr]bool) (string, error) {
	n, err := b.inspectNetwork(ctx, network)
	if err != nil {
		return "", err
	}
	for _, cfg := range n.IPAM.Config {
		prefix, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		gateway, _ := netip.ParseAddr(cfg.Gateway)
		if addr, ok := highestFree(prefix, gateway, used[network]); ok {
			used[network][addr] = true
			return addr.String(), nil
		}
	}
	return "", fmt.Errorf("network %s has no free IPv4 address", network)
}

// takeAddress returns want, which must be free, as the machine's address
// on network, or the highest free address when want is empty.
func (b *Backend) takeAddress(ctx context.Context, network, want string, used map[string]map[netip.Addr]bool) (string, error) {
	if want == "" {
		return b.freeAddress(ctx, network, used)
	}
	addr, err := netip.ParseAddr(want)
	if err != nil || !addr.Is4() {
		return "", fmt.Errorf("address %q is not an IPv4 address", want)
	}
	if used[network][addr] {
		return "", fmt.Errorf("address %s is in use on network %s", want, network)
	}
	used[network][addr] = true
	return addr.String(), nil
}

func highestFree(prefix netip.Prefix, gateway netip.Addr, used map[netip.Addr]bool) (netip.Addr, bool) {
	prefix = prefix.Masked()
	network := prefix.Addr()
	b := network.As4()
	hostBits := 32 - prefix.Bits()
	if hostBits < 2 {
		return netip.Addr{}, false
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	broadcast := n | (1<<hostBits - 1)
	for v := broadcast - 1; v > n; v-- {
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if addr != gateway && !used[addr] {
			return addr, true
		}
	}
	return netip.Addr{}, false
}
