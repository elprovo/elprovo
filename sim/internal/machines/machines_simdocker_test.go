//go:build simdocker

package machines

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testUserData = `#cloud-config
bootcmd:
  - echo "boot $INSTANCE_ID" >> /root/boots
write_files:
  - path: /etc/systemd/system/web.service
    content: |
      [Service]
      ExecStart=/bin/sh -c 'echo serving; exec sleep infinity'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl enable --now web
  - [sh, -c, "docker compose version > /root/compose-version"]
`

func newTestBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	return newTestBackendWith(t, Options{})
}

func newTestBackendWith(t *testing.T, options Options) (*Backend, string) {
	t.Helper()
	suffix := randomSuffix(t)
	network := "elprovo-sim-test-" + suffix
	b := New(network, options, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = b.docker(ctx, nil, "network", "rm", network, network+"-public")
	})
	return b, "test-" + suffix
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func createMachine(t *testing.T, b *Backend, account, name string, spec Spec) Machine {
	t.Helper()
	spec.Name, spec.Hostname, spec.Cloud, spec.Account, spec.Resource = "elprovo-sim-"+account+"-"+name, name, "test", account, name
	m, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := b.Delete(ctx, m.ID); err != nil {
			t.Errorf("delete %s: %v", name, err)
		}
	})
	return m
}

func dockerExec(t *testing.T, id string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", append([]string{"exec", id}, args...)...).CombinedOutput()
	return string(out), err
}

func waitCloudInit(t *testing.T, m Machine) {
	t.Helper()
	if out, err := dockerExec(t, m.ID, "cloud-init", "status", "--wait", "--long"); err != nil {
		t.Fatalf("cloud-init on %s: %v\n%s", m.Name, err, out)
	}
}

func canConnect(t *testing.T, from Machine, addr string) bool {
	t.Helper()
	_, err := dockerExec(t, from.ID, "timeout", "3", "bash", "-c", "exec 3<>/dev/tcp/"+addr+"/22")
	return err == nil
}

func TestMachineBootsSeedDataAndEnforcesItsFirewall(t *testing.T) {
	b, account := newTestBackend(t)
	agent := createMachine(t, b, account, "agent", Spec{Seed: Seed{InstanceID: "1"}})
	stranger := createMachine(t, b, account, "stranger", Spec{Seed: Seed{InstanceID: "2"}})
	web := createMachine(t, b, account, "web", Spec{
		Seed:     Seed{InstanceID: "3", UserData: testUserData},
		Firewall: &Firewall{Inbound: []Rule{{Protocol: "tcp", Ports: "22", Peers: []string{agent.PrivateIP}}}},
	})
	waitCloudInit(t, web)

	if out, err := dockerExec(t, web.ID, "cat", "/root/boots", "/root/compose-version"); err != nil || !strings.Contains(out, "boot 3") || !strings.Contains(out, "Docker Compose version") {
		t.Fatalf("boot results: %v\n%s", err, out)
	}
	if out, err := dockerExec(t, web.ID, "systemctl", "is-active", "web"); err != nil || strings.TrimSpace(out) != "active" {
		t.Fatalf("web unit: %v %q", err, out)
	}
	if !canConnect(t, agent, web.PrivateIP) {
		t.Fatal("the allowed peer cannot reach the machine")
	}
	if canConnect(t, stranger, web.PrivateIP) {
		t.Fatal("a peer the firewall does not allow reached the machine")
	}
	if !canConnect(t, stranger, agent.PublicIP) {
		t.Fatal("a machine without a firewall is not reachable on its public address")
	}

	restarted, err := b.Restart(t.Context(), web.ID, true)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.PrivateIP != web.PrivateIP || restarted.PublicIP != web.PublicIP {
		t.Fatalf("addresses changed across restart: %+v -> %+v", web, restarted)
	}
	waitCloudInit(t, restarted)
	if out, _ := dockerExec(t, web.ID, "cat", "/root/boots"); strings.Count(out, "boot 3") != 2 {
		t.Fatalf("bootcmd did not run on the second boot: %q", out)
	}
	if canConnect(t, stranger, web.PrivateIP) || !canConnect(t, agent, web.PrivateIP) {
		t.Fatal("the firewall did not survive the restart")
	}

	if err := b.SetFirewall(t.Context(), web.ID, nil); err != nil {
		t.Fatalf("remove firewall: %v", err)
	}
	if !canConnect(t, stranger, web.PrivateIP) {
		t.Fatal("the machine still drops traffic after its firewall was removed")
	}

	listed, err := b.List(t.Context(), "test", account)
	if err != nil || len(listed) != 3 {
		t.Fatalf("List = %d machines, %v; want 3", len(listed), err)
	}
}

func TestSnapshotBootsAsANewInstance(t *testing.T) {
	b, account := newTestBackend(t)
	source := createMachine(t, b, account, "source", Spec{Seed: Seed{InstanceID: "10", UserData: testUserData}})
	waitCloudInit(t, source)
	ref := "elprovo-sim-test-snapshot:" + account
	if size, err := b.Commit(t.Context(), source.ID, ref); err != nil || size <= 0 {
		t.Fatalf("commit: size %d, %v", size, err)
	}
	t.Cleanup(func() { _ = b.RemoveImage(context.Background(), ref) })

	clone := createMachine(t, b, account, "clone", Spec{Image: ref, Seed: Seed{InstanceID: "11", UserData: "#cloud-config\nruncmd:\n  - touch /root/clone-ran\n"}})
	waitCloudInit(t, clone)

	out, err := dockerExec(t, clone.ID, "sh", "-c", "cat /root/boots; ls /root/clone-ran; systemctl is-active web")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if !strings.Contains(out, "boot 10") || strings.Contains(out, "boot 11") {
		t.Fatalf("clone boots = %q; want the source's record and no bootcmd of its own", out)
	}
	if !strings.Contains(out, "active") {
		t.Fatalf("the unit enabled on the source does not run on the clone: %q", out)
	}
}

func TestMachineServesItsMetadataToItselfAndToContainers(t *testing.T) {
	b, account := newTestBackend(t)
	m := createMachine(t, b, account, "metadata", Spec{Seed: Seed{
		InstanceID: "30",
		Metadata: func(privateIPv4, _ string) map[string]string {
			return map[string]string{"/meta/v1/private-ipv4": privateIPv4, "/meta/v1/user-data": "#cloud-config\n"}
		},
	}, Hosts: map[string]string{"metadata.test": "169.254.169.254"}})
	waitCloudInit(t, m)

	fromMachine, err := dockerExec(t, m.ID, "curl", "-fsS", "--retry", "10", "--retry-connrefused", "--retry-delay", "1", "http://metadata.test/meta/v1/private-ipv4")
	if err != nil || strings.TrimSpace(fromMachine) == "" {
		t.Fatalf("the machine read its private address: %q, %v", fromMachine, err)
	}
	fromContainer, err := dockerExec(t, m.ID, "docker", "run", "--rm", "--quiet", "busybox:1.37", "wget", "-qO-", "-T", "5", "http://169.254.169.254/meta/v1/user-data")
	if err != nil || fromContainer != "#cloud-config\n" {
		t.Fatalf("a container in the machine read the user data: %q, %v; want it, as on a cloud VM without a firewall rule", fromContainer, err)
	}
}

func TestMachineTrustsTheSimulationCA(t *testing.T) {
	caPEM, leafPEM := testCertificateChain(t, "registry.digitalocean.com")
	b, account := newTestBackendWith(t, Options{CACerts: caPEM})
	m := createMachine(t, b, account, "trust", Spec{Seed: Seed{InstanceID: "40"}})
	waitCloudInit(t, m)

	out, err := dockerExec(t, m.ID, "sh", "-c", "echo "+base64.StdEncoding.EncodeToString(leafPEM)+
		" | base64 -d > /root/leaf.pem && openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt /root/leaf.pem")

	if err != nil || !strings.Contains(out, "/root/leaf.pem: OK") {
		t.Fatalf("verify a certificate the simulation's CA signed: %v\n%s", err, out)
	}
}

// testCertificateChain returns a CA certificate and a server certificate for
// host that it signed, both as PEM.
func testCertificateChain(t *testing.T, host string) (caPEM, leafPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "elprovo-sim test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
}

func TestExtraAddressReachesTheMachineAcrossRestarts(t *testing.T) {
	b, account := newTestBackend(t)
	server := createMachine(t, b, account, "server", Spec{Seed: Seed{InstanceID: "50"}})
	visitor := createMachine(t, b, account, "visitor", Spec{Seed: Seed{InstanceID: "51"}})
	waitCloudInit(t, server)
	networks, err := b.Networks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reserved := middleOf(networks.Public.Prefix).String()

	if err := b.SetAddresses(t.Context(), server.ID, []string{reserved}); err != nil {
		t.Fatalf("set addresses: %v", err)
	}

	if !canConnect(t, visitor, reserved) {
		t.Fatalf("another machine cannot reach the server at the extra address %s", reserved)
	}
	restarted, err := b.Restart(t.Context(), server.ID, true)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitCloudInit(t, restarted)
	if !canConnect(t, visitor, reserved) {
		logs, _ := exec.CommandContext(t.Context(), "docker", "logs", server.ID).CombinedOutput()
		addrs, _ := dockerExec(t, server.ID, "sh", "-c", "ip -4 -o addr show; cat /var/lib/elprovo-vm/addresses.json")
		t.Fatalf("the extra address %s is gone after a restart\n%s\n%s", reserved, addrs, logs)
	}
	if err := b.SetAddresses(t.Context(), server.ID, nil); err != nil {
		t.Fatalf("clear addresses: %v", err)
	}
	if canConnect(t, visitor, reserved) {
		t.Fatalf("the server still answers at %s after it lost the address", reserved)
	}
}

func TestMachineTakesTheGivenPublicAddress(t *testing.T) {
	b, account := newTestBackend(t)
	networks, err := b.Networks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	address := middleOf(networks.Public.Prefix).String()

	m := createMachine(t, b, account, "fixed", Spec{PublicIP: address, Seed: Seed{InstanceID: "70"}})

	if m.PublicIP != address {
		t.Fatalf("public address = %s, want %s", m.PublicIP, address)
	}
	_, err = b.Create(t.Context(), Spec{Name: "elprovo-sim-" + account + "-taken", Hostname: "taken", Cloud: "test", Account: account, Resource: "taken", PublicIP: address})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a second machine with the same public address: %v; want the address refused as in use", err)
	}
}

func middleOf(prefix netip.Prefix) netip.Addr {
	b := prefix.Masked().Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	n += 1 << (31 - prefix.Bits())
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

func TestUnprivilegedUnitBindsALowPortWithItsStateDirectory(t *testing.T) {
	b, account := newTestBackend(t)
	server := createMachine(t, b, account, "lowport", Spec{Seed: Seed{InstanceID: "60", UserData: `#cloud-config
write_files:
  - path: /etc/systemd/system/lowport.service
    content: |
      [Service]
      User=nobody
      StateDirectory=lowport
      AmbientCapabilities=CAP_NET_BIND_SERVICE
      ExecStart=/usr/bin/python3 -m http.server 80 --directory /var/lib/lowport
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl enable --now lowport
`}})
	visitor := createMachine(t, b, account, "lowport-visitor", Spec{Seed: Seed{InstanceID: "61"}})
	waitCloudInit(t, server)

	owner, err := dockerExec(t, server.ID, "stat", "-c", "%U", "/var/lib/lowport")
	if err != nil || strings.TrimSpace(owner) != "nobody" {
		t.Fatalf("state directory owner = %q, %v; want nobody", owner, err)
	}
	status, err := dockerExec(t, visitor.ID, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--retry", "10", "--retry-connrefused", "--retry-delay", "1", "http://"+server.PrivateIP+"/")
	if err != nil || status != "200" {
		journal, _ := dockerExec(t, server.ID, "journalctl", "-u", "lowport", "--no-pager")
		t.Fatalf("a service running as nobody on port 80 answered %q, %v\n%s", status, err, journal)
	}
}

func TestExecRunsACommandAsRootWithStdin(t *testing.T) {
	b, account := newTestBackend(t)
	m := createMachine(t, b, account, "exec", Spec{Seed: Seed{InstanceID: "21"}})

	out, err := b.Exec(t.Context(), m.ID, strings.NewReader("from stdin"), "sh", "-c", `id -u; cat`)

	if err != nil || string(out) != "0\nfrom stdin" {
		t.Fatalf("Exec = %q, %v; want root's uid and the input", out, err)
	}
	if _, err := b.Exec(t.Context(), m.ID, nil, "false"); err == nil {
		t.Fatal("Exec of a failing command succeeded")
	}
}

func TestMachineTakesThePrivateAddressItIsGiven(t *testing.T) {
	b, account := newTestBackend(t)
	nets, err := b.Networks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := nets.Private.Prefix.Masked().Addr().Next().Next().Next().String()

	m := createMachine(t, b, account, "fixed", Spec{PrivateIP: want, Seed: Seed{InstanceID: "22"}})

	if m.PrivateIP != want {
		t.Fatalf("private address = %s, want %s", m.PrivateIP, want)
	}
}

func TestDeleteRemovesTheContainer(t *testing.T) {
	b, account := newTestBackend(t)
	m := createMachine(t, b, account, "gone", Spec{Seed: Seed{InstanceID: "20"}})
	if err := b.Delete(t.Context(), m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.Inspect(t.Context(), m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect after delete = %v, want ErrNotFound", err)
	}
}

func TestInspectingMachinesSkipsOnesThatAreGone(t *testing.T) {
	b, account := newTestBackend(t)
	kept := createMachine(t, b, account, "kept", Spec{Seed: Seed{InstanceID: "1"}})
	gone := createMachine(t, b, account, "gone", Spec{Seed: Seed{InstanceID: "2"}})
	if err := b.Delete(t.Context(), gone.ID); err != nil {
		t.Fatal(err)
	}

	found, err := b.inspectExisting(t.Context(), []string{gone.ID, kept.ID})

	if err != nil || len(found) != 1 || found[0].ID != kept.ID {
		t.Fatalf("inspectExisting = %+v, %v; want only the machine that still exists", found, err)
	}
}
