package digitalocean

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

func TestCreateDropletGoesFromNewToActiveWithItsAddresses(t *testing.T) {
	e := newEnv(t)
	var key struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/account/keys", map[string]string{"name": "agent", "public_key": agentKey}, &key)
	release := e.machines.hold("Create")
	var created struct {
		Droplet dropletJSON `json:"droplet"`
		Links   links       `json:"links"`
	}

	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{
		"name": "web-1.example.com", "region": "nyc3", "size": "s-2vcpu-4gb", "image": "ubuntu-24-04-x64",
		"ssh_keys": []any{key.SSHKey.Fingerprint}, "tags": []string{"web"}, "user_data": "#cloud-config\n",
	}, &created)

	d := created.Droplet
	if d.Status != "new" || len(d.Networks.V4) != 0 || d.Image.ID != ubuntuImage.ID || d.Size.Slug != "s-2vcpu-4gb" || d.Region.Slug != "nyc3" {
		t.Fatalf("new droplet = %+v", d)
	}
	if len(created.Links.Actions) != 1 || created.Links.Actions[0].Rel != "create" ||
		created.Links.Actions[0].HREF != e.server.URL+"/v2/actions/"+strconv.FormatInt(created.Links.Actions[0].ID, 10) {
		t.Fatalf("links = %+v", created.Links)
	}
	if got := e.waitActionStatus(created.Links.Actions[0].ID); got != "in-progress" {
		t.Fatalf("create action while the machine is created = %s, want in-progress", got)
	}
	release()
	if act := e.waitAction(created.Links.Actions[0].ID); act.Status != "completed" || act.CompletedAt == nil {
		t.Fatalf("create action = %+v", act)
	}

	d = e.droplet(d.ID)
	if d.Status != "active" || d.Locked {
		t.Fatalf("droplet after create = %s locked %v", d.Status, d.Locked)
	}
	want := []networkV4{{"172.30.255.254", "255.255.0.0", "nil", "private"}, {"172.31.255.254", "255.255.0.0", "172.31.0.1", "public"}}
	if !slices.Equal(d.Networks.V4, want) {
		t.Fatalf("networks = %+v, want %+v", d.Networks.V4, want)
	}
	spec := e.machines.spec(d.ID)
	if spec.Hostname != "web-1" || spec.Seed.UserData != "#cloud-config\n" || !slices.Equal(spec.Seed.PublicKeys, []string{agentKey}) || spec.Seed.InstanceID != strconv.FormatInt(d.ID, 10) {
		t.Fatalf("machine spec = %+v", spec)
	}
	if spec.Cloud != "digitalocean" || spec.Resource != strconv.FormatInt(d.ID, 10) || spec.Account == "" {
		t.Fatalf("machine labels = %q %q %q", spec.Cloud, spec.Account, spec.Resource)
	}
	var vpcs struct {
		VPCs []vpcJSON `json:"vpcs"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/vpcs", nil, &vpcs)
	i := slices.IndexFunc(vpcs.VPCs, func(v vpcJSON) bool { return v.Region == "nyc3" && v.Default })
	if i < 0 || d.VPCUUID != vpcs.VPCs[i].ID {
		t.Fatalf("droplet VPC = %q, want the nyc3 default VPC", d.VPCUUID)
	}
}

func (e *testEnv) waitActionStatus(id int64) string {
	e.t.Helper()
	var got struct {
		Action actionJSON `json:"action"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/actions/"+strconv.FormatInt(id, 10), nil, &got)
	return got.Action.Status
}

func TestCreateDropletsByNamesReturnsOneActionEach(t *testing.T) {
	e := newEnv(t)
	var created struct {
		Droplets []dropletJSON `json:"droplets"`
		Links    links         `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{
		"names": []string{"a", "b"}, "region": "fra1", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64",
	}, &created)
	if len(created.Droplets) != 2 || len(created.Links.Actions) != 2 || created.Droplets[1].Name != "b" {
		t.Fatalf("created = %+v", created)
	}
}

func TestCreateDropletRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t)
	valid := func() map[string]any {
		return map[string]any{"name": "ok", "region": "nyc3", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64"}
	}
	var otherRegionVPC struct {
		VPC vpcJSON `json:"vpc"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/vpcs", map[string]string{"name": "frankfurt", "region": "fra1"}, &otherRegionVPC)
	for name, change := range map[string]func(map[string]any){
		"invalid name":        func(r map[string]any) { r["name"] = "under_score" },
		"too many names":      func(r map[string]any) { delete(r, "name"); r["names"] = strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",") },
		"unknown region":      func(r map[string]any) { r["region"] = "mars1" },
		"unknown size":        func(r map[string]any) { r["size"] = "s-64vcpu-1tb" },
		"unknown image":       func(r map[string]any) { r["image"] = "debian-12-x64" },
		"unknown SSH key":     func(r map[string]any) { r["ssh_keys"] = []any{12345} },
		"VPC in other region": func(r map[string]any) { r["vpc_uuid"] = otherRegionVPC.VPC.ID },
		"IPv6":                func(r map[string]any) { r["ipv6"] = true },
		"volumes":             func(r map[string]any) { r["volumes"] = []string{"v-1"} },
		"user data too big":   func(r map[string]any) { r["user_data"] = strings.Repeat("x", maxUserData+1) },
	} {
		t.Run(name, func(t *testing.T) {
			req := valid()
			change(req)
			var body errorBody
			if status := e.call(http.MethodPost, "/v2/droplets", req, &body); status != http.StatusUnprocessableEntity || body.ID != "unprocessable_entity" {
				t.Fatalf("response = %d %+v, want 422", status, body)
			}
		})
	}
}

func TestListDropletsFiltersByTagAndName(t *testing.T) {
	e := newEnv(t)
	e.createDroplet("web-1", "web")
	e.createDroplet("web-2", "web")
	e.createDroplet("db-1", "db")
	list := func(query string) []string {
		var got struct {
			Droplets []dropletJSON `json:"droplets"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v2/droplets"+query, nil, &got)
		var names []string
		for _, d := range got.Droplets {
			names = append(names, d.Name)
		}
		return names
	}

	if got := list("?tag_name=web"); !slices.Equal(got, []string{"web-1", "web-2"}) {
		t.Fatalf("tag_name=web = %q", got)
	}
	if got := list("?name=DB-1"); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("name=DB-1 = %q, want a case-insensitive match", got)
	}
	if got := list("?per_page=1&page=3"); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("page 3 = %q", got)
	}
	if status := e.call(http.MethodGet, "/v2/droplets?tag_name=web&name=web-1", nil, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("tag_name with name = %d, want 422", status)
	}
}

func TestDeletedDropletIsGoneOnlyAfterItsMachine(t *testing.T) {
	e := newEnv(t)
	d := e.createDroplet("web-1")
	release := e.machines.hold("Delete")

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/droplets/"+strconv.FormatInt(d.ID, 10), nil, nil)
	if got := e.droplet(d.ID); got.Status != "active" || !got.Locked {
		t.Fatalf("droplet while its machine is deleted = %s locked %v; want it listed and locked", got.Status, got.Locked)
	}
	if status := e.call(http.MethodDelete, "/v2/droplets/"+strconv.FormatInt(d.ID, 10), nil, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("second delete = %d, want 422 for the pending event", status)
	}
	release()
	eventually(t, "the droplet is gone", func() bool {
		return e.call(http.MethodGet, "/v2/droplets/"+strconv.FormatInt(d.ID, 10), nil, nil) == http.StatusNotFound
	})
	if e.machines.hasMachine(d.ID) {
		t.Fatal("the machine still exists after the droplet is gone")
	}
}

func TestDeleteDropletsByTagDeletesOnlyTaggedDroplets(t *testing.T) {
	e := newEnv(t)
	web := e.createDroplet("web-1", "web")
	db := e.createDroplet("db-1", "db")

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/droplets?tag_name=web", nil, nil)

	eventually(t, "the tagged droplet is gone", func() bool {
		return e.call(http.MethodGet, "/v2/droplets/"+strconv.FormatInt(web.ID, 10), nil, nil) == http.StatusNotFound
	})
	if e.droplet(db.ID).Status != "active" {
		t.Fatal("an untagged droplet was deleted")
	}
}

func TestFailedMachineCreationErrorsTheCreateAction(t *testing.T) {
	e := newEnv(t)
	e.machines.fail("Create", machines.ErrNotFound)
	var created struct {
		Droplet dropletJSON `json:"droplet"`
		Links   links       `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{
		"name": "broken", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64",
	}, &created)

	if act := e.waitAction(created.Links.Actions[0].ID); act.Status != "errored" {
		t.Fatalf("create action = %s, want errored", act.Status)
	}
	if d := e.droplet(created.Droplet.ID); d.Status != "off" || d.Locked || d.Region.Slug != "nyc3" {
		t.Fatalf("droplet = %s locked %v region %s; want off, unlocked, in the first region", d.Status, d.Locked, d.Region.Slug)
	}
}

func TestPublishRuleGivesItsPortsToOneDropletAtATime(t *testing.T) {
	e := newEnv(t, func(c *config) {
		c.publish = []publishRule{{tag: "gateway", ports: map[int]int{443: 8443}}}
	})
	first := e.createDroplet("gw-1", "gateway")
	second := e.createDroplet("gw-2", "gateway")

	if got := e.machines.spec(first.ID).Ports; !slices.Equal(got, []machines.Port{{Host: 8443, Machine: 443}}) {
		t.Fatalf("first gateway ports = %+v", got)
	}
	if got := e.machines.spec(second.ID).Ports; len(got) != 0 {
		t.Fatalf("second gateway ports = %+v, want none while the first holds the rule", got)
	}
}

func TestDropletsCreatedTogetherKeepTheirOwnTags(t *testing.T) {
	e := newEnv(t)
	var created struct {
		Droplets []dropletJSON `json:"droplets"`
		Links    links         `json:"links"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/droplets", map[string]any{
		"names": []string{"a", "b"}, "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64", "tags": []string{"web", "prod"},
	}, &created)
	for _, act := range created.Links.Actions {
		e.waitAction(act.ID)
	}
	a, b := created.Droplets[0], created.Droplets[1]

	untag := map[string]any{"resources": []resourceRef{{ResourceID: strconv.FormatInt(a.ID, 10), ResourceType: "droplet"}}}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/tags/web/resources", untag, nil)

	if got := e.droplet(a.ID).Tags; !slices.Equal(got, []string{"prod"}) {
		t.Fatalf("untagged droplet tags = %q", got)
	}
	if got := e.droplet(b.ID).Tags; !slices.Equal(got, []string{"web", "prod"}) {
		t.Fatalf("other droplet tags = %q, want them unchanged", got)
	}
}
