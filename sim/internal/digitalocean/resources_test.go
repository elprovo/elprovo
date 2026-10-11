package digitalocean

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
)

func TestSSHKeyIsFoundByIDAndByFingerprint(t *testing.T) {
	e := newEnv(t)
	var created struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/account/keys", map[string]string{"name": "agent", "public_key": agentKey}, &created)
	if created.SSHKey.Fingerprint != testKeyFingerprint || created.SSHKey.PublicKey != agentKey {
		t.Fatalf("key = %+v", created.SSHKey)
	}
	for _, ref := range []string{strconv.FormatInt(created.SSHKey.ID, 10), testKeyFingerprint} {
		if status := e.call(http.MethodGet, "/v2/account/keys/"+ref, nil, nil); status != http.StatusOK {
			t.Fatalf("GET key %s = %d", ref, status)
		}
	}
	var updated struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	e.mustCall(http.StatusOK, http.MethodPut, "/v2/account/keys/"+testKeyFingerprint, map[string]string{"name": "renamed"}, &updated)
	if updated.SSHKey.Name != "renamed" {
		t.Fatalf("updated = %+v", updated.SSHKey)
	}
	if status := e.call(http.MethodPost, "/v2/account/keys", map[string]string{"name": "again", "public_key": agentKey}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate key = %d, want 422", status)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/account/keys/"+testKeyFingerprint, nil, nil)
	if status := e.call(http.MethodDelete, "/v2/account/keys/"+testKeyFingerprint, nil, nil); status != http.StatusNotFound {
		t.Fatalf("delete of a deleted key = %d, want 404", status)
	}
}

func TestSSHKeyMustBeAPublicKey(t *testing.T) {
	e := newEnv(t)
	for _, key := range []string{"", "ssh-ed25519", "ssh-ed25519 not-base64!", "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY"} {
		if status := e.call(http.MethodPost, "/v2/account/keys", map[string]string{"name": "k", "public_key": key}, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("key %q = %d, want 422", key, status)
		}
	}
}

func TestTagCreateIsIdempotentAndCountsTaggedDroplets(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	var first, second struct {
		Tag tagJSON `json:"tag"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/tags", map[string]string{"name": "Web"}, &first)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/tags", map[string]string{"name": "web"}, &second)
	if second.Tag.Name != "Web" {
		t.Fatalf("second create = %q, want the first spelling", second.Tag.Name)
	}

	resources := map[string]any{"resources": []resourceRef{{ResourceID: strconv.FormatInt(d.ID, 10), ResourceType: "droplet"}}}
	e.mustCall(http.StatusNoContent, http.MethodPost, "/v2/tags/Web/resources", resources, nil)
	var got struct {
		Tag tagJSON `json:"tag"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/tags/Web", nil, &got)
	if got.Tag.Resources.Count != 1 || got.Tag.Resources.Droplets.Count != 1 || got.Tag.Resources.Droplets.LastTaggedURI == "" {
		t.Fatalf("tag resources = %+v", got.Tag.Resources)
	}
	if !slices.Equal(e.droplet(d.ID).Tags, []string{"Web"}) {
		t.Fatalf("droplet tags = %v", e.droplet(d.ID).Tags)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/tags/Web/resources", resources, nil)
	if tags := e.droplet(d.ID).Tags; len(tags) != 0 {
		t.Fatalf("droplet tags after untag = %v", tags)
	}
	if status := e.call(http.MethodPost, "/v2/tags/Web/resources", map[string]any{"resources": []resourceRef{{ResourceID: "x", ResourceType: "volume"}}}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("tagging a volume = %d, want 422", status)
	}
}

func TestDeletingATagRemovesItFromDroplets(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1", "web", "prod")
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/tags/web", nil, nil)
	if tags := e.droplet(d.ID).Tags; !slices.Equal(tags, []string{"prod"}) {
		t.Fatalf("droplet tags = %v", tags)
	}
	if status := e.call(http.MethodGet, "/v2/tags/web", nil, nil); status != http.StatusNotFound {
		t.Fatalf("deleted tag = %d, want 404", status)
	}
}

func TestEachRegionHasADefaultVPCThatCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	var list struct {
		VPCs []vpcJSON `json:"vpcs"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/vpcs", nil, &list)
	if len(list.VPCs) != 2 || !list.VPCs[0].Default || list.VPCs[0].Name != "default-nyc3" || list.VPCs[0].URN != "do:vpc:"+list.VPCs[0].ID {
		t.Fatalf("vpcs = %+v", list.VPCs)
	}
	var body errorBody
	if status := e.call(http.MethodDelete, "/v2/vpcs/"+list.VPCs[0].ID, nil, &body); status != http.StatusForbidden {
		t.Fatalf("delete default = %d %+v, want 403", status, body)
	}
}

func TestVPCRangesDoNotOverlapAndMembersBlockDeletion(t *testing.T) {
	e := newEnv(t)
	var created struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/vpcs", map[string]string{"name": "stagelets", "region": "nyc3", "ip_range": "10.200.0.0/24"}, &created)
	for _, ipRange := range []string{"10.200.0.128/25", "8.8.8.0/24", "10.0.0.0/8", "10.250.0.0/29"} {
		if status := e.call(http.MethodPost, "/v2/vpcs", map[string]string{"name": "other", "region": "nyc3", "ip_range": ipRange}, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("ip_range %s = %d, want 422", ipRange, status)
		}
	}
	var droplet struct {
		Links links `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{"name": "m", "region": "nyc3", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64", "vpc_uuid": created.VPC.ID}, &droplet)
	e.waitAction(droplet.Links.Actions[0].ID)
	var members struct {
		Members []struct {
			URN  string `json:"urn"`
			Name string `json:"name"`
		} `json:"members"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/vpcs/"+created.VPC.ID+"/members", nil, &members)
	if len(members.Members) != 1 || members.Members[0].Name != "m" {
		t.Fatalf("members = %+v", members.Members)
	}
	if status := e.call(http.MethodDelete, "/v2/vpcs/"+created.VPC.ID, nil, nil); status != http.StatusForbidden {
		t.Fatalf("delete with a member = %d, want 403", status)
	}
}

func TestVPCUpdateRenamesAndMovesTheDefault(t *testing.T) {
	e := newEnv(t)
	var created struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/vpcs", map[string]string{"name": "a", "region": "fra1"}, &created)
	if created.VPC.IPRange == "" || created.VPC.Default {
		t.Fatalf("created = %+v; want a generated range and not the default", created.VPC)
	}
	var updated struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusOK, http.MethodPatch, "/v2/vpcs/"+created.VPC.ID, map[string]any{"description": "stagelets", "default": true}, &updated)
	if updated.VPC.Name != "a" || updated.VPC.Description != "stagelets" || !updated.VPC.Default {
		t.Fatalf("updated = %+v", updated.VPC)
	}
	var list struct {
		VPCs []vpcJSON `json:"vpcs"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/vpcs", nil, &list)
	defaults := 0
	for _, v := range list.VPCs {
		if v.Region == "fra1" && v.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("fra1 has %d default VPCs, want 1", defaults)
	}
	if status := e.call(http.MethodPut, "/v2/vpcs/"+created.VPC.ID, map[string]any{"description": "x"}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT without a name = %d, want 422", status)
	}
}
