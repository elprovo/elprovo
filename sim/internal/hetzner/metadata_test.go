package hetzner

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestServerServesTheMetadataCloudInitReads(t *testing.T) {
	e := newEnv(t)
	var key struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "agent", "public_key": agentKey}, &key)
	n := e.createNetwork("stagely")
	req := serverRequest("web-1", nil)
	req["ssh_keys"] = []string{"agent"}
	req["user_data"] = "#cloud-config\nruncmd: [true]\n"
	req["networks"] = []int64{n.ID}
	srv := e.createServer(req)

	docs := e.machines.spec(srv.ID).Seed.Metadata("172.30.0.7", srv.PublicNet.IPv4.IP)

	want := fmt.Sprintf("availability-zone: fsn1-dc14\nhostname: \"web-1\"\ninstance-id: %d\npublic-ipv4: %s\npublic-keys:\n- %q\nregion: eu-central\n",
		srv.ID, srv.PublicNet.IPv4.IP, agentKey)
	if got := docs["/hetzner/v1/metadata"]; got != want {
		t.Fatalf("metadata =\n%s\nwant\n%s", got, want)
	}
	if got := docs["/hetzner/v1/userdata"]; got != "#cloud-config\nruncmd: [true]\n" {
		t.Fatalf("userdata = %q", got)
	}
	private := docs["/hetzner/v1/metadata/private-networks"]
	for _, line := range []string{"- ip: 172.30.0.7", fmt.Sprintf("network_id: %d", n.ID), "network: 10.0.0.0/16", "subnet: 10.0.1.0/24", "gateway: 10.0.0.1", `mac_address: "86:00:00:`} {
		if !strings.Contains(private, line) {
			t.Fatalf("private-networks =\n%s\nwant a line with %q", private, line)
		}
	}
	for path, value := range map[string]string{
		"/hetzner/v1/metadata/hostname": "web-1", "/hetzner/v1/metadata/instance-id": fmt.Sprint(srv.ID),
		"/hetzner/v1/metadata/public-ipv4": srv.PublicNet.IPv4.IP, "/hetzner/v1/metadata/region": "eu-central",
		"/hetzner/v1/metadata/availability-zone": "fsn1-dc14",
	} {
		if docs[path] != value {
			t.Fatalf("%s = %q, want %q", path, docs[path], value)
		}
	}
}

func TestServerOutsideNetworksHasNoPrivateNetworks(t *testing.T) {
	e := newEnv(t)
	srv := e.createServer(serverRequest("web-1", nil))

	docs := e.machines.spec(srv.ID).Seed.Metadata("172.30.0.7", srv.PublicNet.IPv4.IP)

	if got := docs["/hetzner/v1/metadata/private-networks"]; got != "[]\n" {
		t.Fatalf("private-networks = %q, want an empty YAML list", got)
	}
	if !strings.Contains(docs["/hetzner/v1/metadata"], "public-keys: []\n") {
		t.Fatalf("metadata = %q, want an empty public-keys list", docs["/hetzner/v1/metadata"])
	}
}
