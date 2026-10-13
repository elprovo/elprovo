package machines

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/netip"
	"testing"
	"testing/fstest"
)

func TestHighestFreeSkipsGatewayAndUsedAddresses(t *testing.T) {
	prefix := netip.MustParsePrefix("172.30.0.0/29")
	gateway := netip.MustParseAddr("172.30.0.6")
	used := map[netip.Addr]bool{netip.MustParseAddr("172.30.0.5"): true}

	got, ok := highestFree(prefix, gateway, used)
	if !ok || got != netip.MustParseAddr("172.30.0.4") {
		t.Fatalf("highestFree = %v %v, want 172.30.0.4 below the gateway and the used address", got, ok)
	}
}

func TestHighestFreeReportsAFullSubnet(t *testing.T) {
	prefix := netip.MustParsePrefix("10.0.0.0/30")
	used := map[netip.Addr]bool{netip.MustParseAddr("10.0.0.2"): true}
	if _, ok := highestFree(prefix, netip.MustParseAddr("10.0.0.1"), used); ok {
		t.Fatal("found a free address in a full subnet")
	}
}

func TestBuildContextTagFollowsContentButNotTests(t *testing.T) {
	base := fstest.MapFS{
		"Dockerfile":          {Data: []byte("FROM ubuntu:24.04\n")},
		"vminit/main.go":      {Data: []byte("package main\n")},
		"vminit/main_test.go": {Data: []byte("package main\n")},
	}
	_, tag, err := buildContext(base)
	if err != nil {
		t.Fatal(err)
	}
	withChangedTest := fstest.MapFS{"Dockerfile": base["Dockerfile"], "vminit/main.go": base["vminit/main.go"], "vminit/main_test.go": {Data: []byte("changed")}}
	withChangedCode := fstest.MapFS{"Dockerfile": base["Dockerfile"], "vminit/main.go": {Data: []byte("package main // changed\n")}}

	if _, same, _ := buildContext(withChangedTest); same != tag {
		t.Fatalf("a test file change moved the tag from %s to %s", tag, same)
	}
	if _, other, _ := buildContext(withChangedCode); other == tag {
		t.Fatal("a source change kept the tag")
	}
	archive, _, _ := buildContext(base)
	if names := tarNames(t, archive); names["vminit/main_test.go"] != nil {
		t.Fatal("the build context contains a test file")
	}
}

func TestSeedArchiveWritesMetaDataUserDataAndFirewall(t *testing.T) {
	spec := Spec{
		Hostname: "web-1",
		Seed:     Seed{InstanceID: "101", PublicKeys: []string{"ssh-ed25519 AAAA agent"}, UserData: "#cloud-config\n"},
	}
	archive, err := seedArchive(spec, addresses{private: "172.30.255.254", public: "172.31.255.254"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := tarNames(t, archive)

	var meta map[string]any
	if err := json.Unmarshal(files["var/lib/cloud/seed/nocloud/meta-data"], &meta); err != nil {
		t.Fatalf("meta-data is not JSON: %v", err)
	}
	if meta["instance-id"] != "101" || meta["local-hostname"] != "web-1" || meta["local-ipv4"] != "172.30.255.254" || meta["public-ipv4"] != "172.31.255.254" {
		t.Fatalf("meta-data = %v", meta)
	}
	if string(files["var/lib/cloud/seed/nocloud/user-data"]) != "#cloud-config\n" {
		t.Fatalf("user-data = %q", files["var/lib/cloud/seed/nocloud/user-data"])
	}
	if string(files["var/lib/elprovo-vm/firewall.json"]) != "null" {
		t.Fatalf("firewall = %q, want null so a snapshot's old firewall is replaced", files["var/lib/elprovo-vm/firewall.json"])
	}
}

func tarNames(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = data
	}
}
