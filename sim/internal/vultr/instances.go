package vultr

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const (
	instanceNotFound   = "Invalid instance ID."
	pendingDestruction = "Server is pending destruction."
)

var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

type instanceJSON struct {
	ID               string   `json:"id"`
	Os               string   `json:"os"`
	RAM              int      `json:"ram"`
	Disk             int      `json:"disk"`
	MainIP           string   `json:"main_ip"`
	VCPUCount        int      `json:"vcpu_count"`
	Region           string   `json:"region"`
	Plan             string   `json:"plan"`
	DefaultPassword  string   `json:"default_password,omitempty"`
	DateCreated      string   `json:"date_created"`
	Status           string   `json:"status"`
	PowerStatus      string   `json:"power_status"`
	ServerStatus     string   `json:"server_status"`
	AllowedBandwidth int      `json:"allowed_bandwidth"`
	NetmaskV4        string   `json:"netmask_v4"`
	GatewayV4        string   `json:"gateway_v4"`
	V6Network        string   `json:"v6_network"`
	V6MainIP         string   `json:"v6_main_ip"`
	V6NetworkSize    int      `json:"v6_network_size"`
	Label            string   `json:"label"`
	Tag              string   `json:"tag"`
	InternalIP       string   `json:"internal_ip"`
	VPCOnly          bool     `json:"vpc_only"`
	KVM              string   `json:"kvm"`
	Hostname         string   `json:"hostname"`
	OsID             int      `json:"os_id"`
	AppID            int      `json:"app_id"`
	ImageID          string   `json:"image_id"`
	SnapshotID       string   `json:"snapshot_id"`
	FirewallGroupID  string   `json:"firewall_group_id"`
	Features         []string `json:"features"`
	Tags             []string `json:"tags"`
	UserScheme       string   `json:"user_scheme"`
}

func (s *Simulation) renderInstance(i *instance) instanceJSON {
	p, _ := plan(i.Plan)
	out := instanceJSON{
		ID: i.ID, Os: ubuntuOS.Name, RAM: p.RAM, Disk: p.Disk, MainIP: "0.0.0.0", VCPUCount: p.VCPUCount, Region: i.Region, Plan: i.Plan,
		DateCreated: timestamp(i.CreatedAt), Status: i.Status, PowerStatus: i.PowerStatus, ServerStatus: i.ServerStatus,
		AllowedBandwidth: p.Bandwidth, GatewayV4: "0.0.0.0", Label: i.Label, Hostname: i.Hostname, OsID: i.OSID,
		SnapshotID: i.SnapshotID, FirewallGroupID: i.FirewallGroupID, Features: []string{}, Tags: nonNil(i.Tags), UserScheme: "root",
	}
	if i.PublicIP != "" {
		out.MainIP, out.NetmaskV4, out.GatewayV4 = i.PublicIP, netmask(s.networks.Public), s.networks.Public.Gateway.String()
	}
	if len(i.VPCs) > 0 {
		out.InternalIP = i.PrivateIP
	}
	return out
}

func netmask(subnet machines.Subnet) string {
	if !subnet.Prefix.IsValid() {
		return ""
	}
	return net.IP(net.CIDRMask(subnet.Prefix.Bits(), 32)).String()
}

type createInstanceRequest struct {
	Region            string            `json:"region"`
	Plan              string            `json:"plan"`
	OSID              int               `json:"os_id"`
	SnapshotID        string            `json:"snapshot_id"`
	ISOID             string            `json:"iso_id"`
	AppID             int               `json:"app_id"`
	ImageID           string            `json:"image_id"`
	ScriptID          string            `json:"script_id"`
	IPXEChainURL      string            `json:"ipxe_chain_url"`
	Label             string            `json:"label"`
	Hostname          string            `json:"hostname"`
	Tags              []string          `json:"tags"`
	SSHKeyIDs         []string          `json:"sshkey_id"`
	UserData          string            `json:"user_data"`
	AttachVPC         []string          `json:"attach_vpc"`
	EnableVPC         bool              `json:"enable_vpc"`
	VPCOnly           bool              `json:"vpc_only"`
	AttachVPC2        []string          `json:"attach_vpc2"`
	EnableVPC2        bool              `json:"enable_vpc2"`
	FirewallGroupID   string            `json:"firewall_group_id"`
	ReservedIPv4      string            `json:"reserved_ipv4"`
	EnableIPv6        bool              `json:"enable_ipv6"`
	DisablePublicIPv4 bool              `json:"disable_public_ipv4"`
	Backups           string            `json:"backups"`
	DDOSProtection    bool              `json:"ddos_protection"`
	UserScheme        string            `json:"user_scheme"`
	BlockDevices      []any             `json:"block_devices"`
	AppVariables      map[string]string `json:"app_variables"`
}

// unsupported names a requested feature the simulation does not run, or ""
// when it runs them all.
func (req createInstanceRequest) unsupported() string {
	switch {
	case req.ISOID != "" || req.AppID != 0 || req.ImageID != "" || req.IPXEChainURL != "":
		return "ISO, application and iPXE installs are"
	case req.ScriptID != "":
		return "startup scripts are"
	case req.EnableIPv6 || req.DisablePublicIPv4:
		return "IPv6 is"
	case req.EnableVPC:
		return "enable_vpc (a default VPC) is"
	case req.VPCOnly:
		return "vpc_only (NAT gateways) is"
	case req.EnableVPC2 || len(req.AttachVPC2) > 0:
		return "VPC 2.0 is"
	case req.Backups == "enabled":
		return "automatic backups are"
	case req.DDOSProtection:
		return "DDoS protection is"
	case req.UserScheme != "" && req.UserScheme != "root":
		return "the limited user scheme is"
	case len(req.BlockDevices) > 0:
		return "block devices are"
	case len(req.AppVariables) > 0:
		return "marketplace app variables are"
	}
	return ""
}

type instancePlan struct {
	inst       *instance
	imageRef   string
	publicKeys []string
}

// planInstance validates the request against the account and returns the
// new instance, or an error to answer with.
func (s *Simulation) planInstance(a *account, req createInstanceRequest) (instancePlan, error) {
	if what := req.unsupported(); what != "" {
		return instancePlan{}, errBadRequest("%s not supported by the simulation.", what)
	}
	if _, ok := region(req.Region); !ok {
		return instancePlan{}, errBadRequest("Invalid region.")
	}
	if _, ok := plan(req.Plan); !ok {
		return instancePlan{}, errBadRequest("Invalid plan.")
	}
	userData, err := base64.StdEncoding.DecodeString(req.UserData)
	if err != nil {
		return instancePlan{}, errBadRequest("user_data must be base64 encoded.")
	}
	inst := &instance{
		ID: newUUID(), Label: req.Label, Hostname: req.Hostname, Region: req.Region, Plan: req.Plan, Tags: nonNil(req.Tags),
		UserData: string(userData), Status: "pending", PowerStatus: "running", ServerStatus: "none", CreatedAt: s.now(),
	}
	if inst.Hostname == "" {
		inst.Hostname = "vultr-guest"
	}
	if len(inst.Hostname) > 253 || !hostnamePattern.MatchString(inst.Hostname) {
		return instancePlan{}, errBadRequest("Invalid hostname.")
	}
	p := instancePlan{inst: inst}
	if err := s.planImage(a, req, &p); err != nil {
		return instancePlan{}, err
	}
	for _, id := range req.SSHKeyIDs {
		k := a.sshKey(id)
		if k == nil {
			return instancePlan{}, errBadRequest("Invalid SSH key: %s.", id)
		}
		p.publicKeys = append(p.publicKeys, k.SSHKey)
	}
	if err := planNetworks(a, req, inst); err != nil {
		return instancePlan{}, err
	}
	return p, nil
}

func (s *Simulation) planImage(a *account, req createInstanceRequest, p *instancePlan) error {
	switch {
	case req.SnapshotID != "" && req.OSID != 0:
		return errBadRequest("Choose one of os_id and snapshot_id.")
	case req.SnapshotID != "":
		snap := a.snapshot(req.SnapshotID)
		if snap == nil || snap.Status != "complete" {
			return errBadRequest("Invalid snapshot_id: the snapshot does not exist or is not complete.")
		}
		p.inst.SnapshotID, p.inst.OSID, p.imageRef = snap.ID, snap.OSID, snap.ImageRef
	case req.OSID == ubuntuOS.ID:
		p.inst.OSID = req.OSID
	case req.OSID == 0:
		return errBadRequest("Choose an os_id or a snapshot_id.")
	default:
		return errBadRequest("Invalid os_id: the simulation boots %s (os_id %d) only.", ubuntuOS.Name, ubuntuOS.ID)
	}
	return nil
}

func planNetworks(a *account, req createInstanceRequest, inst *instance) error {
	if err := checkVPCs(a, req.AttachVPC, inst.Region); err != nil {
		return err
	}
	inst.VPCs = slices.Compact(slices.Sorted(slices.Values(req.AttachVPC)))
	if req.FirewallGroupID != "" {
		if a.firewallGroup(req.FirewallGroupID) == nil {
			return errBadRequest("Invalid firewall group.")
		}
		inst.FirewallGroupID = req.FirewallGroupID
	}
	if req.ReservedIPv4 != "" {
		r := a.reservedIP(req.ReservedIPv4)
		switch {
		case r == nil || r.Region != inst.Region:
			return errBadRequest("Invalid reserved_ipv4: %s is not a reserved IP in region %s.", req.ReservedIPv4, inst.Region)
		case r.InstanceID != "":
			return errBadRequest("Floating IPv4 address is already attached to another server.")
		}
		inst.MainReservedIP = r.ID
	}
	return nil
}

func defaultPassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Simulation) createInstance(r *http.Request) (int, any, error) {
	var req createInstanceRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out instanceJSON
	var spec machines.Spec
	err := s.store.Update(func(a *account) error {
		p, err := s.planInstance(a, req)
		if err != nil {
			return err
		}
		a.Instances = append(a.Instances, p.inst)
		if r := a.reservedIP(p.inst.MainReservedIP); r != nil {
			r.InstanceID = p.inst.ID
		}
		spec = s.machineSpec(a, p)
		out = s.renderInstance(p.inst)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	out.DefaultPassword = defaultPassword()
	instanceID := out.ID
	s.background(func(ctx context.Context) { s.provision(ctx, instanceID, spec) })
	return http.StatusAccepted, map[string]any{"instance": out}, nil
}

// machineSpec describes the instance's machine. A reserved IP the instance
// is created with becomes the machine's public address, so the machine sends
// from it, as an instance does from its main address.
func (s *Simulation) machineSpec(a *account, p instancePlan) machines.Spec {
	inst := *p.inst
	publicIP := ""
	if r := a.reservedIP(inst.MainReservedIP); r != nil {
		publicIP = r.Address
	}
	reg, _ := region(inst.Region)
	vpcID := ""
	if len(inst.VPCs) > 0 {
		vpcID = inst.VPCs[0]
	}
	return machines.Spec{
		Name:     fmt.Sprintf("stagely-sim-vultr-%s-%s", a.ID[:8], inst.ID),
		Hostname: inst.Hostname,
		Cloud:    cloudLabel, Account: a.ID, Resource: inst.ID,
		Image:    p.imageRef,
		PublicIP: publicIP,
		Seed: machines.Seed{
			InstanceID: inst.ID, PublicKeys: p.publicKeys, UserData: inst.UserData,
			Metadata: func(privateIPv4, publicIPv4 string) map[string]string {
				return instanceMetadata(metadataInput{
					instance: &inst, region: reg, publicKeys: p.publicKeys, vpcID: vpcID,
					privateIPv4: privateIPv4, publicIPv4: publicIPv4, networks: s.networks,
				})
			},
		},
		Firewall: policyFor(a, &inst, s.networks.Private.Prefix),
	}
}

// provision creates the instance's machine, gives it its extra reserved IPs, and
// marks the instance active. A machine that fails leaves the instance
// suspended and stopped, so a client waiting for it fails instead of
// waiting on. An instance deleted while its machine was created is
// destroyed now.
func (s *Simulation) provision(ctx context.Context, instanceID string, spec machines.Spec) {
	m, err := s.machines.Create(ctx, spec)
	if err == nil {
		err = s.pushAddresses(ctx, instanceID, m.ID)
	}
	if err != nil {
		s.log.Error("create instance machine", "instance", instanceID, "error", err)
	}
	destroying := false
	updateErr := s.store.Update(func(a *account) error {
		inst := a.instance(instanceID)
		if inst == nil {
			return nil
		}
		inst.Machine, inst.PrivateIP, inst.PublicIP = m.ID, m.PrivateIP, m.PublicIP
		destroying = inst.Destroying
		if err != nil {
			inst.Status, inst.PowerStatus, inst.ServerStatus = "suspended", "stopped", "none"
			return nil
		}
		inst.Status, inst.ServerStatus = "active", "ok"
		return nil
	})
	if updateErr != nil {
		s.log.Error("record instance machine", "instance", instanceID, "error", updateErr)
	}
	if destroying {
		s.destroy(ctx, instanceID, m.ID)
		return
	}
	if err == nil {
		s.firewalls.applied(instanceID, spec.Firewall)
	}
	s.firewalls.kick()
}

// pushAddresses gives the instance's machine exactly the extra reserved IPs
// that the account attaches to the instance.
func (s *Simulation) pushAddresses(ctx context.Context, instanceID, machine string) error {
	s.addressMu.Lock()
	defer s.addressMu.Unlock()
	var addrs []string
	s.store.Read(func(a *account) { addrs = a.extraAddresses(instanceID) })
	if len(addrs) == 0 {
		return nil
	}
	return s.machines.SetAddresses(ctx, machine, addrs)
}

func (s *Simulation) listInstances(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	var out []instanceJSON
	s.store.Read(func(a *account) {
		for _, i := range a.Instances {
			j := s.renderInstance(i)
			if matches(q.Get("label"), j.Label) && matches(q.Get("main_ip"), j.MainIP) && matches(q.Get("region"), j.Region) &&
				matches(q.Get("firewall_group_id"), j.FirewallGroupID) && matches(q.Get("hostname"), j.Hostname) &&
				(q.Get("tag") == "" || slices.Contains(j.Tags, q.Get("tag"))) {
				out = append(out, j)
			}
		}
	})
	return listResponse(r, "instances", out)
}

func matches(filter, value string) bool {
	return filter == "" || filter == value
}

// withInstance calls fn with the instance the path names. A missing instance
// is a 404, and one that is being destroyed a 400, as on Vultr.
func withInstance(a *account, r *http.Request, fn func(*instance) error) error {
	inst := a.instance(r.PathValue("id"))
	switch {
	case inst == nil:
		return errNotFound(instanceNotFound)
	case inst.Destroying:
		return errBadRequest(pendingDestruction)
	}
	return fn(inst)
}

func (s *Simulation) getInstance(r *http.Request) (int, any, error) {
	var out instanceJSON
	var err error
	s.store.Read(func(a *account) {
		err = withInstance(a, r, func(inst *instance) error {
			out = s.renderInstance(inst)
			return nil
		})
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"instance": out}, nil
}

type updateInstanceRequest struct {
	Label           *string  `json:"label"`
	Tags            []string `json:"tags"`
	Plan            string   `json:"plan"`
	FirewallGroupID *string  `json:"firewall_group_id"`
	AttachVPC       []string `json:"attach_vpc"`
	DetachVPC       []string `json:"detach_vpc"`
	EnableVPC       bool     `json:"enable_vpc"`
	EnableIPv6      bool     `json:"enable_ipv6"`
	DDOSProtection  bool     `json:"ddos_protection"`
	Backups         string   `json:"backups"`
	UserData        string   `json:"user_data"`
	OSID            any      `json:"os_id"`
	AppID           int      `json:"app_id"`
	ImageID         string   `json:"image_id"`
	UserScheme      string   `json:"user_scheme"`
	EnableVPC2      bool     `json:"enable_vpc2"`
	AttachVPC2      []string `json:"attach_vpc2"`
}

func (req updateInstanceRequest) unsupported() string {
	switch {
	case req.EnableIPv6:
		return "IPv6 is"
	case req.DDOSProtection:
		return "DDoS protection is"
	case req.Backups == "enabled":
		return "automatic backups are"
	case req.UserData != "":
		return "changing user data is"
	case req.OSID != nil || req.AppID != 0 || req.ImageID != "":
		return "reinstalling is"
	case req.UserScheme != "" && req.UserScheme != "root":
		return "the limited user scheme is"
	case req.EnableVPC:
		return "enable_vpc (a default VPC) is"
	case req.EnableVPC2 || len(req.AttachVPC2) > 0:
		return "VPC 2.0 is"
	}
	return ""
}

// updateInstance changes what a running instance can change in place. A
// null tags list keeps the tags, as the Terraform provider sends null when
// they did not change.
func (s *Simulation) updateInstance(r *http.Request) (int, any, error) {
	var req updateInstanceRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if what := req.unsupported(); what != "" {
		return 0, nil, errBadRequest("%s not supported by the simulation.", what)
	}
	var out instanceJSON
	err := s.store.Update(func(a *account) error {
		return withInstance(a, r, func(inst *instance) error {
			if err := checkInstanceUpdate(a, inst, req); err != nil {
				return err
			}
			if req.Label != nil {
				inst.Label = *req.Label
			}
			if req.Tags != nil {
				inst.Tags = slices.Clone(req.Tags)
			}
			if req.Plan != "" {
				inst.Plan = req.Plan
			}
			if req.FirewallGroupID != nil {
				inst.FirewallGroupID = *req.FirewallGroupID
			}
			for _, id := range req.AttachVPC {
				if !slices.Contains(inst.VPCs, id) {
					inst.VPCs = append(inst.VPCs, id)
				}
			}
			inst.VPCs = slices.DeleteFunc(inst.VPCs, func(id string) bool { return slices.Contains(req.DetachVPC, id) })
			out = s.renderInstance(inst)
			return nil
		})
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusAccepted, map[string]any{"instance": out}, nil
}

func checkInstanceUpdate(a *account, inst *instance, req updateInstanceRequest) error {
	if _, ok := plan(req.Plan); req.Plan != "" && !ok {
		return errBadRequest("Invalid plan.")
	}
	if req.FirewallGroupID != nil && *req.FirewallGroupID != "" && a.firewallGroup(*req.FirewallGroupID) == nil {
		return errBadRequest("Invalid firewall group.")
	}
	return checkVPCs(a, req.AttachVPC, inst.Region)
}

// checkVPCs checks that each of ids is a VPC in the region.
func checkVPCs(a *account, ids []string, region string) error {
	for _, id := range ids {
		if v := a.vpc(id); v == nil || v.Region != region {
			return errBadRequest("Invalid VPC: %s is not a VPC in region %s.", id, region)
		}
	}
	return nil
}

// deleteInstance starts destroying the instance. While its machine is still
// being created, provision destroys it once the machine exists.
func (s *Simulation) deleteInstance(r *http.Request) (int, any, error) {
	var machine, id string
	creating := false
	err := s.store.Update(func(a *account) error {
		return withInstance(a, r, func(inst *instance) error {
			inst.Destroying = true
			machine, id, creating = inst.Machine, inst.ID, inst.Status == "pending"
			return nil
		})
	})
	if err != nil {
		return 0, nil, err
	}
	if !creating {
		s.background(func(ctx context.Context) { s.destroy(ctx, id, machine) })
	}
	return http.StatusNoContent, nil, nil
}

// destroy deletes the instance's machine and then forgets the instance, so
// GET answers 404 only once the machine is gone. A failed deletion leaves
// the instance in place to delete again.
func (s *Simulation) destroy(ctx context.Context, id, machine string) {
	if machine != "" {
		if err := s.machines.Delete(ctx, machine); err != nil {
			s.log.Error("delete instance machine", "instance", id, "error", err)
			_ = s.store.Update(func(a *account) error {
				if inst := a.instance(id); inst != nil {
					inst.Destroying = false
				}
				return nil
			})
			return
		}
	}
	if err := s.store.Update(func(a *account) error { a.removeInstance(id); return nil }); err != nil {
		s.log.Error("remove deleted instance", "instance", id, "error", err)
	}
	s.firewalls.forget(id)
}

// listInstanceVPCs lists the VPCs the instance is in, with its address in
// them, which is its machine's private address.
func (s *Simulation) listInstanceVPCs(r *http.Request) (int, any, error) {
	type vpcInfo struct {
		ID         string `json:"id"`
		MacAddress string `json:"mac_address"`
		IPAddress  string `json:"ip_address"`
	}
	out := []vpcInfo{}
	var err error
	s.store.Read(func(a *account) {
		err = withInstance(a, r, func(inst *instance) error {
			for _, id := range inst.VPCs {
				out = append(out, vpcInfo{ID: id, IPAddress: inst.PrivateIP})
			}
			return nil
		})
	})
	if err != nil {
		return 0, nil, err
	}
	return listResponse(r, "vpcs", out)
}

// listInstanceVPC2s lists no VPC 2.0 networks: the simulation has none, but
// the Terraform provider reads the list for every instance.
func (s *Simulation) listInstanceVPC2s(r *http.Request) (int, any, error) {
	var err error
	s.store.Read(func(a *account) { err = withInstance(a, r, func(*instance) error { return nil }) })
	if err != nil {
		return 0, nil, err
	}
	return listResponse(r, "vpcs", []struct{}{})
}

func (s *Simulation) getBackupSchedule(r *http.Request) (int, any, error) {
	var err error
	s.store.Read(func(a *account) { err = withInstance(a, r, func(*instance) error { return nil }) })
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"backup_schedule": map[string]any{"enabled": false}}, nil
}

func (s *Simulation) getUserData(r *http.Request) (int, any, error) {
	var data string
	var err error
	s.store.Read(func(a *account) {
		err = withInstance(a, r, func(inst *instance) error {
			data = base64.StdEncoding.EncodeToString([]byte(inst.UserData))
			return nil
		})
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"user_data": map[string]string{"data": data}}, nil
}
