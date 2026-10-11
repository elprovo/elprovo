// Package cloudinit writes the cloud-init user data that Stagely gives a new
// machine: an SSH host key that the creator generated, so it can pin the key
// and never trust an unknown one, the packages the machine needs, for
// machines that run customer containers a firewall rule that keeps those
// containers away from the cloud's metadata service, and on a private
// network that the cloud's firewall does not filter, firewall rules that
// accept new connections from the network only from given addresses.
package cloudinit

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HostKeyConfig returns user data that makes the Ed25519 key in hostKeyPEM,
// an OpenSSH private key, the machine's only SSH host key and installs
// packages, with the public key to pin. It embeds the PEM as given: encoding
// the key again would give different bytes, and a module that sees changed
// user data replaces the machine.
func HostKeyConfig(hostKeyPEM string, packages ...string) (string, ssh.PublicKey, error) {
	raw, err := ssh.ParseRawPrivateKey([]byte(hostKeyPEM))
	if err != nil {
		return "", nil, fmt.Errorf("parse host key: %w", err)
	}
	hostKey, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return "", nil, errors.New("the host key is not an Ed25519 key")
	}
	public, err := ssh.NewPublicKey(hostKey.Public())
	if err != nil {
		return "", nil, fmt.Errorf("encode host key: %w", err)
	}
	var b strings.Builder
	b.WriteString("#cloud-config\nssh_deletekeys: true\nssh_genkeytypes: []\nssh_keys:\n  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimSpace(hostKeyPEM), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("  ed25519_public: " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))) + "\n")
	if len(packages) > 0 {
		b.WriteString("package_update: true\npackages:\n")
		for _, p := range packages {
			b.WriteString("  - " + p + "\n")
		}
	}
	return b.String(), public, nil
}

// MachineConfig is HostKeyConfig for a build worker or a stagelet machine,
// which run customer code in containers. Every cloud serves instance metadata,
// including this user data and so the host key, at 169.254.169.254. On every
// boot, before Docker starts, the machine adds a rule to Docker's DOCKER-USER
// chain that rejects new connections from containers to that address. Docker
// keeps the chain's rules when it starts.
func MachineConfig(hostKeyPEM string, packages ...string) (string, error) {
	return MachineConfigBlocking(hostKeyPEM, nil, packages...)
}

// HostBlock is an address, or an address range, on the cloud's network that
// containers must not open new connections to.
type HostBlock struct {
	Address string
	// ExceptPort, when not zero, stays reachable over TCP and UDP, such as
	// 53 for a DNS server that shares the address.
	ExceptPort int
}

// MachineConfigBlocking is MachineConfig that also rejects new connections
// from containers to each of blocks, with the same kind of rule.
func MachineConfigBlocking(hostKeyPEM string, blocks []HostBlock, packages ...string) (string, error) {
	data, _, err := HostKeyConfig(hostKeyPEM, packages...)
	if err != nil {
		return "", err
	}
	return data + bootcmd(metadataRules(blocks)), nil
}

// PrivateNetwork is a private network that the cloud's firewall does not
// filter, such as a Hetzner Cloud network or a Vultr VPC.
type PrivateNetwork struct {
	// Range is the network's IPv4 address range.
	Range netip.Prefix
	// Sources are the IPv4 addresses on the network that may open new
	// connections to the machine. Without any, none may.
	Sources []netip.Addr
}

// privateChain is the iptables chain that accepts a new connection from a
// private network only from one of its sources.
const privateChain = "STAGELY-PRIVATE"

// rules returns bootcmd lines that fill privateChain and send each new
// connection from the network in chains to it.
func (n PrivateNetwork) rules(chains ...string) ([]string, error) {
	if !n.Range.IsValid() || !n.Range.Addr().Is4() {
		return nil, fmt.Errorf("the private network range %q is not an IPv4 range", n.Range)
	}
	lines := []string{"iptables -N " + privateChain + " 2>/dev/null || iptables -F " + privateChain}
	for _, source := range n.Sources {
		if !source.Is4() {
			return nil, fmt.Errorf("the private network source %q is not an IPv4 address", source)
		}
		lines = append(lines, "iptables -A "+privateChain+" -s "+source.String()+" -j RETURN")
	}
	lines = append(lines, "iptables -A "+privateChain+" -j REJECT")
	for _, chain := range chains {
		lines = append(lines, insertOnce(chain+" -s "+n.Range.Masked().String()+" -m conntrack --ctstate NEW -j "+privateChain))
	}
	return lines, nil
}

// HostKeyConfigOnNetwork is HostKeyConfig for a host on network that runs no
// containers, such as the agent's or the gateway's. On every boot, its INPUT
// chain starts to reject new connections from the network, except from the
// network's Sources.
func HostKeyConfigOnNetwork(hostKeyPEM string, network PrivateNetwork) (string, ssh.PublicKey, error) {
	lines, err := network.rules("INPUT")
	if err != nil {
		return "", nil, err
	}
	data, public, err := HostKeyConfig(hostKeyPEM)
	if err != nil {
		return "", nil, err
	}
	return data + bootcmd(lines), public, nil
}

// MachineConfigOnNetwork is MachineConfig for a machine on network. On every
// boot, before Docker starts, the machine also starts to reject new
// connections from the network, except from the network's Sources: in the
// INPUT chain for its own ports, and in DOCKER-USER for the ports Docker
// publishes, which never reach INPUT. DOCKER-USER also rejects every new
// connection from containers to the network, so a container can't reach
// other machines there with a forged source address either.
func MachineConfigOnNetwork(hostKeyPEM string, network PrivateNetwork, packages ...string) (string, error) {
	lines, err := network.rules("INPUT", "DOCKER-USER")
	if err != nil {
		return "", err
	}
	data, _, err := HostKeyConfig(hostKeyPEM, packages...)
	if err != nil {
		return "", err
	}
	blocks := []HostBlock{{Address: network.Range.Masked().String()}}
	return data + bootcmd(append(metadataRules(blocks), lines...)), nil
}

// metadataRules returns bootcmd lines that make Docker's DOCKER-USER chain
// reject new connections from containers to the metadata service and to
// each of blocks. Docker keeps the chain's rules when it starts.
func metadataRules(blocks []HostBlock) []string {
	rules := []string{"DOCKER-USER -d 169.254.169.254 -m conntrack --ctstate NEW -j REJECT"}
	for _, b := range blocks {
		if b.ExceptPort == 0 {
			rules = append(rules, "DOCKER-USER -d "+b.Address+" -m conntrack --ctstate NEW -j REJECT")
			continue
		}
		port := strconv.Itoa(b.ExceptPort)
		for _, proto := range []string{"tcp", "udp"} {
			rules = append(rules, "DOCKER-USER -d "+b.Address+" -p "+proto+" ! --dport "+port+" -m conntrack --ctstate NEW -j REJECT")
		}
	}
	lines := []string{"iptables -N DOCKER-USER 2>/dev/null || true"}
	for _, rule := range rules {
		lines = append(lines, insertOnce(rule))
	}
	return lines
}

// insertOnce returns a command that inserts the rule at the top of its
// chain unless the chain has it already.
func insertOnce(rule string) string {
	return "iptables -C " + rule + " 2>/dev/null || iptables -I " + rule
}

// bootcmd returns a cloud-config bootcmd section that runs lines in order
// on every boot.
func bootcmd(lines []string) string {
	var b strings.Builder
	b.WriteString("bootcmd:\n")
	for _, line := range lines {
		b.WriteString("  - " + line + "\n")
	}
	return b.String()
}
