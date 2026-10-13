package cloudinit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testHostKey returns a new Ed25519 key and its OpenSSH PEM.
func testHostKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(block))
}

func TestHostKeyConfigPinsTheGivenKey(t *testing.T) {
	key, keyPEM := testHostKey(t)

	userData, public, err := HostKeyConfig(keyPEM, "docker.io")
	if err != nil {
		t.Fatalf("HostKeyConfig: %v", err)
	}
	want, _ := ssh.NewPublicKey(key.Public())
	if string(public.Marshal()) != string(want.Marshal()) {
		t.Fatal("the returned public key is not the host key's")
	}
	for _, part := range []string{"#cloud-config\n", "ssh_deletekeys: true", "ed25519_private: |\n    -----BEGIN OPENSSH PRIVATE KEY-----", "packages:\n  - docker.io\n"} {
		if !strings.Contains(userData, part) {
			t.Fatalf("user data lacks %q:\n%s", part, userData)
		}
	}
}

func TestHostKeyConfigIsTheSameForTheSameKey(t *testing.T) {
	_, keyPEM := testHostKey(t)

	first, _, err := HostKeyConfig(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _ := HostKeyConfig(keyPEM)

	if first != second {
		t.Fatal("the same host key gave different user data; a bootstrap module would replace the host on every setup run")
	}
}

func TestHostKeyConfigWithoutPackagesInstallsNothing(t *testing.T) {
	_, keyPEM := testHostKey(t)
	userData, _, err := HostKeyConfig(keyPEM)
	if err != nil || strings.Contains(userData, "packages:") {
		t.Fatalf("user data = %q, %v; want no package section", userData, err)
	}
}

func TestMachineConfigKeepsContainersFromTheMetadataService(t *testing.T) {
	_, keyPEM := testHostKey(t)

	userData, err := MachineConfig(keyPEM)

	want := "bootcmd:\n" +
		"  - iptables -N DOCKER-USER 2>/dev/null || true\n" +
		"  - iptables -C DOCKER-USER -d 169.254.169.254 -m conntrack --ctstate NEW -j REJECT 2>/dev/null || iptables -I DOCKER-USER -d 169.254.169.254 -m conntrack --ctstate NEW -j REJECT\n"
	if err != nil || !strings.HasSuffix(userData, want) {
		t.Fatalf("user data ends with:\n%s\nwant:\n%s", userData[strings.Index(userData, "bootcmd:"):], want)
	}
}

func TestMachineConfigBlockingRejectsAHostButItsExceptedPort(t *testing.T) {
	_, keyPEM := testHostKey(t)
	plain, err := MachineConfig(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	userData, err := MachineConfigBlocking(keyPEM, []HostBlock{{Address: "168.63.129.16", ExceptPort: 53}})

	if err != nil || !strings.HasPrefix(userData, plain) {
		t.Fatalf("user data does not start with MachineConfig's: %v\n%s", err, userData)
	}
	for _, proto := range []string{"tcp", "udp"} {
		rule := "DOCKER-USER -d 168.63.129.16 -p " + proto + " ! --dport 53 -m conntrack --ctstate NEW -j REJECT"
		if !strings.Contains(userData, "  - iptables -C "+rule+" 2>/dev/null || iptables -I "+rule+"\n") {
			t.Fatalf("user data lacks the %s rule for 168.63.129.16 that keeps port 53 open:\n%s", proto, userData)
		}
	}
}

func TestMachineConfigOnNetworkAcceptsNewConnectionsFromTheNetworkOnlyFromItsSources(t *testing.T) {
	_, keyPEM := testHostKey(t)
	plain, err := MachineConfig(keyPEM, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	network := PrivateNetwork{Range: netip.MustParsePrefix("10.42.0.0/20"), Sources: []netip.Addr{netip.MustParseAddr("10.42.0.2"), netip.MustParseAddr("10.42.0.3")}}

	userData, err := MachineConfigOnNetwork(keyPEM, network, "docker.io")

	want := plain +
		"  - iptables -C DOCKER-USER -d 10.42.0.0/20 -m conntrack --ctstate NEW -j REJECT 2>/dev/null || iptables -I DOCKER-USER -d 10.42.0.0/20 -m conntrack --ctstate NEW -j REJECT\n" +
		"  - iptables -N ELPROVO-PRIVATE 2>/dev/null || iptables -F ELPROVO-PRIVATE\n" +
		"  - iptables -A ELPROVO-PRIVATE -s 10.42.0.2 -j RETURN\n" +
		"  - iptables -A ELPROVO-PRIVATE -s 10.42.0.3 -j RETURN\n" +
		"  - iptables -A ELPROVO-PRIVATE -j REJECT\n" +
		"  - iptables -C INPUT -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE 2>/dev/null || iptables -I INPUT -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE\n" +
		"  - iptables -C DOCKER-USER -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE 2>/dev/null || iptables -I DOCKER-USER -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE\n"
	if err != nil || userData != want {
		t.Fatalf("user data = %v\n%s\nwant MachineConfig's, a rule that keeps containers off the network, then rules for host ports and published ports:\n%s", err, userData, want)
	}
}

func TestHostKeyConfigOnNetworkRejectsEveryNewConnectionFromTheNetwork(t *testing.T) {
	key, keyPEM := testHostKey(t)
	plain, _, err := HostKeyConfig(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	userData, public, err := HostKeyConfigOnNetwork(keyPEM, PrivateNetwork{Range: netip.MustParsePrefix("10.42.0.0/20")})

	want := plain + "bootcmd:\n" +
		"  - iptables -N ELPROVO-PRIVATE 2>/dev/null || iptables -F ELPROVO-PRIVATE\n" +
		"  - iptables -A ELPROVO-PRIVATE -j REJECT\n" +
		"  - iptables -C INPUT -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE 2>/dev/null || iptables -I INPUT -s 10.42.0.0/20 -m conntrack --ctstate NEW -j ELPROVO-PRIVATE\n"
	if err != nil || userData != want {
		t.Fatalf("user data = %v\n%s\nwant HostKeyConfig's, then a rule that rejects the network:\n%s", err, userData, want)
	}
	if wantKey, _ := ssh.NewPublicKey(key.Public()); string(public.Marshal()) != string(wantKey.Marshal()) {
		t.Fatal("the returned public key is not the host key's")
	}
}

func TestPrivateNetworkRulesRefuseWhatIsNotIPv4(t *testing.T) {
	_, keyPEM := testHostKey(t)
	for name, network := range map[string]PrivateNetwork{
		"no range":    {},
		"IPv6 range":  {Range: netip.MustParsePrefix("fd00::/64")},
		"IPv6 source": {Range: netip.MustParsePrefix("10.42.0.0/20"), Sources: []netip.Addr{netip.MustParseAddr("fd00::2")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MachineConfigOnNetwork(keyPEM, network); err == nil {
				t.Fatal("MachineConfigOnNetwork succeeded; want an error, as iptables rules cover IPv4 only")
			}
		})
	}
}
