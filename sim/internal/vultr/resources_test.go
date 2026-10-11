package vultr

import (
	"net/http"
	"strings"
	"testing"
)

func TestSSHKeyLifecycle(t *testing.T) {
	e := newEnv(t)
	var created struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/ssh-keys", map[string]string{"name": "agent", "ssh_key": agentKey + "\n"}, &created)
	if created.SSHKey.SSHKey != agentKey+"\n" || created.SSHKey.Name != "agent" || created.SSHKey.DateCreated == "" {
		t.Fatalf("created = %+v; want the key stored as sent", created.SSHKey)
	}
	path := "/v2/ssh-keys/" + created.SSHKey.ID

	e.mustCall(http.StatusNoContent, http.MethodPatch, path, map[string]string{"name": "renamed"}, nil)
	var read struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, path, nil, &read)
	if read.SSHKey.Name != "renamed" || read.SSHKey.SSHKey != agentKey+"\n" {
		t.Fatalf("after rename = %+v", read.SSHKey)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, path, nil, nil)
	var body errorBody
	if status := e.call(http.MethodGet, path, nil, &body); status != http.StatusNotFound || !strings.Contains(body.Error, "Invalid ssh key") {
		t.Fatalf("deleted key = %d %+v; the Terraform provider looks for \"Invalid ssh key\"", status, body)
	}
}

func TestSSHKeyMustBeAPublicKey(t *testing.T) {
	e := newEnv(t)
	for _, key := range []string{"", "ssh-ed25519", "ssh-ed25519 not-base64!", "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIEGiip+8bOcPUgKryMNIrrEq6am8Gw3YZSPOV/Py82KY"} {
		if status := e.call(http.MethodPost, "/v2/ssh-keys", map[string]string{"name": "k", "ssh_key": key}, nil); status != http.StatusBadRequest {
			t.Errorf("key %q = %d, want 400", key, status)
		}
	}
}

func TestVPCWithoutASubnetGetsAFreeSlash20(t *testing.T) {
	e := newEnv(t)

	first, second := e.createVPC("ewr"), e.createVPC("ewr")

	if first.V4Subnet != "10.1.0.0" || first.V4SubnetMask != 20 || second.V4Subnet != "10.1.16.0" {
		t.Fatalf("subnets = %s/%d and %s/%d", first.V4Subnet, first.V4SubnetMask, second.V4Subnet, second.V4SubnetMask)
	}
}

func TestVPCSubnetMustBeAPrivateNetworkAddress(t *testing.T) {
	e := newEnv(t)
	var created struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/vpcs", map[string]any{"region": "ewr", "v4_subnet": "10.42.0.0", "v4_subnet_mask": 20, "description": "env"}, &created)
	if created.VPC.V4Subnet != "10.42.0.0" || created.VPC.V4SubnetMask != 20 || created.VPC.Description != "env" || created.VPC.Region != "ewr" {
		t.Fatalf("created = %+v", created.VPC)
	}
	for name, req := range map[string]map[string]any{
		"public network":      {"region": "ewr", "v4_subnet": "8.8.8.0", "v4_subnet_mask": 24},
		"host bits set":       {"region": "ewr", "v4_subnet": "10.42.0.1", "v4_subnet_mask": 24},
		"subnet without bits": {"region": "ewr", "v4_subnet": "10.43.0.0"},
		"unknown region":      {"region": "mars"},
	} {
		if status := e.call(http.MethodPost, "/v2/vpcs", req, nil); status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, status)
		}
	}
}

func TestVPCWithAnAttachedInstanceCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	network := e.createVPC("ewr")
	inst := e.createInstance("web-1", map[string]any{"attach_vpc": []string{network.ID}})
	var body errorBody

	if status := e.call(http.MethodDelete, "/v2/vpcs/"+network.ID, nil, &body); status != http.StatusBadRequest || !strings.Contains(body.Error, "attached to this VPC network") {
		t.Fatalf("delete with an instance = %d %+v; the Terraform provider retries on \"attached to this VPC network\"", status, body)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+inst.ID, nil, nil)
	eventually(t, "the VPC can be deleted", func() bool {
		return e.call(http.MethodDelete, "/v2/vpcs/"+network.ID, nil, nil) == http.StatusNoContent
	})
	if status := e.call(http.MethodGet, "/v2/vpcs/"+network.ID, nil, &body); status != http.StatusNotFound || !strings.Contains(body.Error, "Invalid VPC ID") {
		t.Fatalf("deleted VPC = %d %+v", status, body)
	}
}

func TestVPCUpdateChangesTheDescription(t *testing.T) {
	e := newEnv(t)
	network := e.createVPC("ewr")

	e.mustCall(http.StatusNoContent, http.MethodPut, "/v2/vpcs/"+network.ID, map[string]string{"description": "stagelets"}, nil)

	var read struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/vpcs/"+network.ID, nil, &read)
	if read.VPC.Description != "stagelets" {
		t.Fatalf("description = %q", read.VPC.Description)
	}
}

func TestRegistryHandsOutItsRootUserForItsRegionsHost(t *testing.T) {
	e := newEnv(t)
	var created registryJSON
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/registry", map[string]any{"name": "acme", "public": false, "region": "ewr", "plan": "start_up"}, &created)
	if created.URN != "ewr.vultrcr.com/acme" || created.RootUser.Username != "root-user" || created.RootUser.Password != "root-secret" || !created.RootUser.Root {
		t.Fatalf("created = %+v", created)
	}
	if created.Storage.Allowed.MB != 10240 || created.Metadata.Region.URN != "ewr.vultrcr.com" {
		t.Fatalf("storage %+v, region %+v", created.Storage, created.Metadata.Region)
	}

	var creds struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	e.mustCall(http.StatusOK, http.MethodOptions, "/v2/registry/"+created.ID+"/docker-credentials?expiry_seconds=0&read_write=true", nil, &creds)
	if creds.Auths["ewr.vultrcr.com"].Auth != b64("root-user:root-secret") {
		t.Fatalf("docker credentials = %+v", creds)
	}
	if status := e.call(http.MethodOptions, "/v2/registry/"+created.ID+"/docker-credentials?read_write=maybe", nil, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid read_write = %d, want 422", status)
	}
	if status := e.call(http.MethodGet, "/v2/registry/"+created.ID+"/docker-credentials", nil, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET docker-credentials = %d, want 405: Vultr serves them for OPTIONS only", status)
	}
}

func TestRegistryLifecycle(t *testing.T) {
	e := newEnv(t)
	var created registryJSON
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/registry", map[string]any{"name": "acme", "public": false, "region": "fra", "plan": "business"}, &created)
	path := "/v2/registry/" + created.ID

	var updated registryJSON
	e.mustCall(http.StatusOK, http.MethodPut, path, map[string]any{"plan": "premium", "public": true}, &updated)
	if !updated.Public || updated.Storage.Allowed.MB != 51200 {
		t.Fatalf("updated = %+v", updated)
	}
	var list struct {
		Registries []registryJSON `json:"registries"`
		Meta       meta           `json:"meta"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/registries", nil, &list)
	if len(list.Registries) != 1 || list.Meta.Total != 1 {
		t.Fatalf("list = %+v", list)
	}
	for name, req := range map[string]map[string]any{
		"taken name":   {"name": "acme", "public": false, "region": "fra", "plan": "business"},
		"dash in name": {"name": "ac-me", "public": false, "region": "fra", "plan": "business"},
		"unknown plan": {"name": "other", "public": false, "region": "fra", "plan": "free"},
	} {
		if status := e.call(http.MethodPost, "/v2/registry", req, nil); status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, status)
		}
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, path, nil, nil)
	var body errorBody
	if status := e.call(http.MethodGet, path, nil, &body); status != http.StatusNotFound || !strings.Contains(body.Error, "Invalid container registry ID") {
		t.Fatalf("deleted registry = %d %+v", status, body)
	}
}
