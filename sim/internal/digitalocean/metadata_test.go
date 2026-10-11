package digitalocean

import (
	"encoding/json"
	"testing"
)

func TestDropletMetadataFollowsDigitalOceansLayout(t *testing.T) {
	d := &droplet{ID: 42, Name: "web-1", Region: "fra1", Tags: []string{"stagely-stagelet"}}
	plan := dropletPlan{publicKeys: []string{"ssh-ed25519 AAAA one", "ssh-ed25519 BBBB two"}}

	docs := dropletMetadata(d, plan, "#cloud-config\n", "10.8.0.2", "203.0.113.7")

	want := map[string]string{
		"/metadata/v1/id":                                "42",
		"/metadata/v1/hostname":                          "web-1",
		"/metadata/v1/user-data":                         "#cloud-config\n",
		"/metadata/v1/public-keys":                       "ssh-ed25519 AAAA one\nssh-ed25519 BBBB two",
		"/metadata/v1/region":                            "fra1",
		"/metadata/v1/interfaces/private/0/ipv4/address": "10.8.0.2",
		"/metadata/v1/interfaces/public/0/ipv4/address":  "203.0.113.7",
		"/metadata/v1/tags/stagely-stagelet":             "stagely-stagelet",
	}
	for path, value := range want {
		if docs[path] != value {
			t.Errorf("%s = %q, want %q", path, docs[path], value)
		}
	}
	var full struct {
		DropletID  int64  `json:"droplet_id"`
		Hostname   string `json:"hostname"`
		Region     string `json:"region"`
		Interfaces map[string][]struct {
			IPv4 struct {
				IPAddress string `json:"ip_address"`
			} `json:"ipv4"`
		} `json:"interfaces"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(docs["/metadata/v1.json"]), &full); err != nil {
		t.Fatalf("v1.json: %v", err)
	}
	if full.DropletID != 42 || full.Hostname != "web-1" || full.Region != "fra1" || full.Interfaces["public"][0].IPv4.IPAddress != "203.0.113.7" || len(full.Tags) != 1 {
		t.Fatalf("v1.json = %+v", full)
	}
}
