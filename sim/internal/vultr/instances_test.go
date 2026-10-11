package vultr

import (
	"errors"
	"net/http"
	"slices"
	"testing"
)

func TestCreateInstanceGoesFromPendingToActiveWithItsMachine(t *testing.T) {
	e := newEnv(t)
	var key struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/ssh-keys", map[string]string{"name": "agent", "ssh_key": agentKey}, &key)
	release := e.machines.hold("Create")
	var created struct {
		Instance instanceJSON `json:"instance"`
	}

	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/instances", map[string]any{
		"region": "fra", "plan": "vc2-2c-4gb", "os_id": 2284, "label": "web-1", "hostname": "web-1",
		"tags": []string{"web"}, "sshkey_id": []string{key.SSHKey.ID}, "user_data": b64("#cloud-config\n"),
		"activation_email": false, "enable_ipv6": false, "backups": "disabled",
	}, &created)

	inst := created.Instance
	if inst.Status != "pending" || inst.MainIP != "0.0.0.0" || inst.DefaultPassword == "" || inst.RAM != 4096 || inst.UserScheme != "root" {
		t.Fatalf("new instance = %+v", inst)
	}
	release()
	inst = e.waitActive(inst.ID)
	if inst.Status != "active" || inst.PowerStatus != "running" || inst.ServerStatus != "ok" || inst.DefaultPassword != "" {
		t.Fatalf("instance after create = %+v", inst)
	}
	if inst.MainIP != "172.31.255.254" || inst.NetmaskV4 != "255.255.0.0" || inst.GatewayV4 != "172.31.0.1" || inst.InternalIP != "" {
		t.Fatalf("addresses = main %s netmask %s gateway %s internal %q", inst.MainIP, inst.NetmaskV4, inst.GatewayV4, inst.InternalIP)
	}
	if inst.Label != "web-1" || inst.Hostname != "web-1" || !slices.Equal(inst.Tags, []string{"web"}) || inst.OsID != 2284 || inst.Region != "fra" {
		t.Fatalf("instance = %+v", inst)
	}
	spec := e.machines.spec(inst.ID)
	if spec.Hostname != "web-1" || spec.Seed.UserData != "#cloud-config\n" || !slices.Equal(spec.Seed.PublicKeys, []string{agentKey}) || spec.Seed.InstanceID != inst.ID {
		t.Fatalf("machine spec = %+v", spec)
	}
	if spec.Cloud != "vultr" || spec.Resource != inst.ID || spec.Account == "" || spec.Firewall != nil {
		t.Fatalf("machine labels = %q %q %q, firewall %+v", spec.Cloud, spec.Account, spec.Resource, spec.Firewall)
	}
}

func TestInstanceInAVPCReportsItsPrivateAddress(t *testing.T) {
	e := newEnv(t)
	network := e.createVPC("ewr")

	inst := e.createInstance("web-1", map[string]any{"attach_vpc": []string{network.ID}})

	if inst.InternalIP != "172.30.255.254" {
		t.Fatalf("internal_ip = %q, want the machine's private address", inst.InternalIP)
	}
	var vpcs struct {
		VPCs []struct {
			ID        string `json:"id"`
			IPAddress string `json:"ip_address"`
		} `json:"vpcs"`
		Meta meta `json:"meta"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances/"+inst.ID+"/vpcs", nil, &vpcs)
	if len(vpcs.VPCs) != 1 || vpcs.VPCs[0].ID != network.ID || vpcs.VPCs[0].IPAddress != inst.InternalIP || vpcs.Meta.Total != 1 {
		t.Fatalf("instance VPCs = %+v", vpcs)
	}
}

func TestInstanceOutsideAVPCHasNoPrivateAddress(t *testing.T) {
	e := newEnv(t)

	inst := e.createInstance("web-1", nil)

	if inst.InternalIP != "" {
		t.Fatalf("internal_ip = %q, want none outside a VPC", inst.InternalIP)
	}
	for _, path := range []string{"/vpcs", "/vpc2"} {
		var list struct {
			VPCs []any `json:"vpcs"`
			Meta *meta `json:"meta"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances/"+inst.ID+path, nil, &list)
		if list.VPCs == nil || len(list.VPCs) != 0 || list.Meta == nil {
			t.Fatalf("%s = %+v, want an empty list with meta, which the Terraform provider reads", path, list)
		}
	}
}

func TestCreateInstanceRejectsWhatItCannotRun(t *testing.T) {
	e := newEnv(t)
	frankfurt := e.createVPC("fra")
	valid := func() map[string]any {
		return map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "os_id": 2284}
	}
	for name, change := range map[string]func(map[string]any){
		"unknown region":         func(r map[string]any) { r["region"] = "mars" },
		"unknown plan":           func(r map[string]any) { r["plan"] = "vc2-64c-1tb" },
		"other operating system": func(r map[string]any) { r["os_id"] = 1743 },
		"no image":               func(r map[string]any) { delete(r, "os_id") },
		"os and snapshot":        func(r map[string]any) { r["snapshot_id"] = "snap" },
		"user data not base64":   func(r map[string]any) { r["user_data"] = "#cloud-config" },
		"unknown SSH key":        func(r map[string]any) { r["sshkey_id"] = []string{"nope"} },
		"unknown VPC":            func(r map[string]any) { r["attach_vpc"] = []string{"vpc-1"} },
		"VPC in another region":  func(r map[string]any) { r["attach_vpc"] = []string{frankfurt.ID} },
		"automatic VPC":          func(r map[string]any) { r["enable_vpc"] = true },
		"unknown firewall group": func(r map[string]any) { r["firewall_group_id"] = "nope" },
		"invalid hostname":       func(r map[string]any) { r["hostname"] = "under_score" },
		"IPv6":                   func(r map[string]any) { r["enable_ipv6"] = true },
		"VPC 2.0":                func(r map[string]any) { r["attach_vpc2"] = []string{"v2"} },
		"automatic backups":      func(r map[string]any) { r["backups"] = "enabled" },
		"ISO install":            func(r map[string]any) { delete(r, "os_id"); r["iso_id"] = "iso" },
		"limited user":           func(r map[string]any) { r["user_scheme"] = "limited" },
	} {
		t.Run(name, func(t *testing.T) {
			req := valid()
			change(req)
			var body errorBody
			if status := e.call(http.MethodPost, "/v2/instances", req, &body); status != http.StatusBadRequest || body.Status != http.StatusBadRequest || body.Error == "" {
				t.Fatalf("response = %d %+v, want 400 with a message", status, body)
			}
		})
	}
}

func TestListInstancesFiltersByLabelAndTag(t *testing.T) {
	e := newEnv(t)
	e.createInstance("web-1", map[string]any{"tags": []string{"web"}})
	e.createInstance("web-2", map[string]any{"tags": []string{"web"}})
	db := e.createInstance("db-1", map[string]any{"tags": []string{"db"}})
	list := func(query string) []string {
		var got struct {
			Instances []instanceJSON `json:"instances"`
		}
		e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances"+query, nil, &got)
		var labels []string
		for _, i := range got.Instances {
			labels = append(labels, i.Label)
		}
		return labels
	}

	if got := list("?tag=web"); !slices.Equal(got, []string{"web-1", "web-2"}) {
		t.Fatalf("tag=web = %q", got)
	}
	if got := list("?label=db-1"); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("label=db-1 = %q", got)
	}
	if got := list("?main_ip=" + db.MainIP); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("main_ip = %q", got)
	}
}

func TestDeletedInstanceIsPendingDestructionUntilItsMachineIsGone(t *testing.T) {
	e := newEnv(t)
	inst := e.createInstance("web-1", nil)
	release := e.machines.hold("Delete")

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+inst.ID, nil, nil)

	var body errorBody
	if status := e.call(http.MethodGet, "/v2/instances/"+inst.ID, nil, &body); status != http.StatusBadRequest || body.Error != pendingDestruction {
		t.Fatalf("GET while the machine is deleted = %d %+v", status, body)
	}
	if status := e.call(http.MethodDelete, "/v2/instances/"+inst.ID, nil, &body); status != http.StatusBadRequest || body.Error != pendingDestruction {
		t.Fatalf("second DELETE = %d %+v", status, body)
	}
	release()
	eventually(t, "the instance is gone", func() bool {
		return e.call(http.MethodGet, "/v2/instances/"+inst.ID, nil, nil) == http.StatusNotFound
	})
	if e.machines.hasMachine("c-" + inst.ID) {
		t.Fatal("the machine still exists after the instance is gone")
	}
}

func TestInstanceDeletedWhileItsMachineIsCreatedIsDestroyedOnceCreated(t *testing.T) {
	e := newEnv(t)
	release := e.machines.hold("Create")
	var created struct {
		Instance instanceJSON `json:"instance"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/instances", map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "os_id": 2284}, &created)

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+created.Instance.ID, nil, nil)
	release()

	eventually(t, "the instance is gone", func() bool {
		return e.call(http.MethodGet, "/v2/instances/"+created.Instance.ID, nil, nil) == http.StatusNotFound
	})
	if e.machines.hasMachine("c-" + created.Instance.ID) {
		t.Fatal("the machine created after the delete was kept")
	}
}

func TestFailedMachineLeavesTheInstanceSuspended(t *testing.T) {
	e := newEnv(t)
	e.machines.fail("Create", errors.New("docker is gone"))
	var created struct {
		Instance instanceJSON `json:"instance"`
	}
	e.mustCall(http.StatusAccepted, http.MethodPost, "/v2/instances", map[string]any{"region": "ewr", "plan": "vc2-1c-1gb", "os_id": 2284}, &created)

	inst := e.waitActive(created.Instance.ID)

	if inst.Status != "suspended" || inst.PowerStatus != "stopped" {
		t.Fatalf("instance = %s %s, want suspended and stopped", inst.Status, inst.PowerStatus)
	}
	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/instances/"+inst.ID, nil, nil)
	eventually(t, "the failed instance is gone", func() bool {
		return e.call(http.MethodGet, "/v2/instances/"+inst.ID, nil, nil) == http.StatusNotFound
	})
}

func TestUpdateInstanceKeepsTagsThatAreNull(t *testing.T) {
	e := newEnv(t)
	inst := e.createInstance("web-1", map[string]any{"tags": []string{"web"}})
	path := "/v2/instances/" + inst.ID
	var updated struct {
		Instance instanceJSON `json:"instance"`
	}

	e.mustCall(http.StatusAccepted, http.MethodPatch, path, map[string]any{"label": "web-2", "tags": nil, "ddos_protection": nil}, &updated)
	if updated.Instance.Label != "web-2" || !slices.Equal(updated.Instance.Tags, []string{"web"}) {
		t.Fatalf("after label change = %+v", updated.Instance)
	}
	e.mustCall(http.StatusAccepted, http.MethodPatch, path, map[string]any{"tags": []string{}}, &updated)
	if len(updated.Instance.Tags) != 0 {
		t.Fatalf("tags after clearing = %q", updated.Instance.Tags)
	}
	for name, change := range map[string]map[string]any{"reinstall": {"os_id": 1743}, "unknown VPC": {"attach_vpc": []string{"vpc-1"}}} {
		if status := e.call(http.MethodPatch, path, change, nil); status != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", name, status)
		}
	}
}

func TestInstanceServesItsBackupScheduleAndUserData(t *testing.T) {
	e := newEnv(t)
	inst := e.createInstance("web-1", map[string]any{"user_data": b64("#!/bin/sh\necho hi\n")})
	var schedule struct {
		BackupSchedule struct {
			Enabled *bool `json:"enabled"`
		} `json:"backup_schedule"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances/"+inst.ID+"/backup-schedule", nil, &schedule)
	if schedule.BackupSchedule.Enabled == nil || *schedule.BackupSchedule.Enabled {
		t.Fatalf("backup schedule = %+v, want enabled false", schedule)
	}
	var userData struct {
		UserData struct {
			Data string `json:"data"`
		} `json:"user_data"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/instances/"+inst.ID+"/user-data", nil, &userData)
	if userData.UserData.Data != b64("#!/bin/sh\necho hi\n") {
		t.Fatalf("user data = %q", userData.UserData.Data)
	}
}

func TestUpdateInstanceAttachesAndDetachesVPCs(t *testing.T) {
	e := newEnv(t)
	network := e.createVPC("ewr")
	inst := e.createInstance("web-1", nil)
	path := "/v2/instances/" + inst.ID
	var updated struct {
		Instance instanceJSON `json:"instance"`
	}

	e.mustCall(http.StatusAccepted, http.MethodPatch, path, map[string]any{"attach_vpc": []string{network.ID}}, &updated)
	if updated.Instance.InternalIP == "" {
		t.Fatal("instance attached to a VPC has no internal_ip")
	}
	e.mustCall(http.StatusAccepted, http.MethodPatch, path, map[string]any{"detach_vpc": []string{network.ID}}, &updated)
	if updated.Instance.InternalIP != "" {
		t.Fatalf("internal_ip after detach = %q", updated.Instance.InternalIP)
	}
}
