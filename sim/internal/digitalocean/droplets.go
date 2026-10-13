package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const maxUserData = 64 << 10

var dropletNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]?[a-z0-9A-Z.\-]*[a-z0-9A-Z]$`)

type dropletJSON struct {
	ID               int64        `json:"id"`
	Name             string       `json:"name"`
	Memory           int          `json:"memory"`
	VCPUs            int          `json:"vcpus"`
	Disk             int          `json:"disk"`
	DiskInfo         []diskInfo   `json:"disk_info"`
	Locked           bool         `json:"locked"`
	Status           string       `json:"status"`
	Kernel           *struct{}    `json:"kernel"`
	CreatedAt        string       `json:"created_at"`
	Features         []string     `json:"features"`
	BackupIDs        []int64      `json:"backup_ids"`
	NextBackupWindow *struct{}    `json:"next_backup_window"`
	SnapshotIDs      []int64      `json:"snapshot_ids"`
	Image            imageJSON    `json:"image"`
	VolumeIDs        []string     `json:"volume_ids"`
	Size             sizeJSON     `json:"size"`
	SizeSlug         string       `json:"size_slug"`
	Networks         networksJSON `json:"networks"`
	Region           regionJSON   `json:"region"`
	Tags             []string     `json:"tags"`
	VPCUUID          string       `json:"vpc_uuid"`
}

type networksJSON struct {
	V4 []networkV4 `json:"v4"`
	V6 []struct{}  `json:"v6"`
}

type networkV4 struct {
	IPAddress string `json:"ip_address"`
	Netmask   string `json:"netmask"`
	Gateway   string `json:"gateway"`
	Type      string `json:"type"`
}

func (s *Simulation) render(d *droplet) dropletJSON {
	size, _ := s.catalog.size(d.Size)
	out := dropletJSON{
		ID: d.ID, Name: d.Name, Memory: size.Memory, VCPUs: size.VCPUs, Disk: size.Disk, DiskInfo: size.DiskInfo,
		Locked: d.Locked, Status: d.Status, CreatedAt: timestamp(d.CreatedAt), Features: nonNil(d.Features),
		BackupIDs: []int64{}, SnapshotIDs: nonNil(d.SnapshotIDs), Image: d.Image, VolumeIDs: []string{},
		Size: size, SizeSlug: d.Size, Region: s.catalog.region(d.Region), Tags: nonNil(d.Tags), VPCUUID: d.VPCUUID,
		Networks: networksJSON{V4: []networkV4{}, V6: []struct{}{}},
	}
	if d.PrivateIP != "" {
		out.Networks.V4 = append(out.Networks.V4, networkV4{d.PrivateIP, netmask(s.networks.Private), "nil", "private"})
	}
	if d.PublicIP != "" {
		out.Networks.V4 = append(out.Networks.V4, networkV4{d.PublicIP, netmask(s.networks.Public), s.networks.Public.Gateway.String(), "public"})
	}
	return out
}

func netmask(subnet machines.Subnet) string {
	if !subnet.Prefix.IsValid() {
		return ""
	}
	return net.IP(net.CIDRMask(subnet.Prefix.Bits(), 32)).String()
}

type createDropletRequest struct {
	Name             string            `json:"name"`
	Names            []string          `json:"names"`
	Region           string            `json:"region"`
	Size             string            `json:"size"`
	Image            json.RawMessage   `json:"image"`
	SSHKeys          []json.RawMessage `json:"ssh_keys"`
	Backups          bool              `json:"backups"`
	IPv6             bool              `json:"ipv6"`
	Monitoring       bool              `json:"monitoring"`
	UserData         string            `json:"user_data"`
	Volumes          []json.RawMessage `json:"volumes"`
	Tags             []string          `json:"tags"`
	VPCUUID          string            `json:"vpc_uuid"`
	PublicNetworking *bool             `json:"public_networking"`
}

type dropletPlan struct {
	names      []string
	region     string
	size       string
	image      imageJSON
	imageRef   string
	publicKeys []string
	tags       []string
	vpcUUID    string
	features   []string
}

func (s *Simulation) planDroplets(a *account, req createDropletRequest) (dropletPlan, error) {
	p := dropletPlan{names: req.Names}
	if req.Name != "" {
		p.names = []string{req.Name}
	}
	switch {
	case len(p.names) == 0:
		return p, errUnprocessable("Name is required")
	case len(p.names) > 10:
		return p, errUnprocessable("You may only create up to 10 Droplets at a time.")
	case len(a.Droplets)+len(p.names) > dropletLimit:
		return p, errUnprocessable("creating this/these droplet(s) will exceed your droplet limit")
	case req.IPv6:
		return p, errUnprocessable("ipv6 is not supported by the simulation: machines have IPv4 addresses only")
	case len(req.Volumes) > 0:
		return p, errUnprocessable("volumes are not supported by the simulation")
	case req.PublicNetworking != nil && !*req.PublicNetworking:
		return p, errUnprocessable("public_networking false is not supported by the simulation: every droplet gets a public address")
	case len(req.UserData) > maxUserData:
		return p, errUnprocessable("user_data must be 64 KiB or smaller")
	}
	for _, n := range p.names {
		if len(n) > 255 || !dropletNamePattern.MatchString(n) {
			return p, errUnprocessable("Name is invalid: %q", n)
		}
	}
	p.region = req.Region
	if p.region == "" {
		p.region = s.cfg.regions[0]
	}
	if !s.catalog.hasRegion(p.region) {
		return p, errUnprocessable("You specified an invalid region for Droplet creation.")
	}
	if _, ok := s.catalog.size(req.Size); !ok {
		return p, errUnprocessable("You specified an invalid size for Droplet creation.")
	}
	p.size = req.Size
	img, snap, ok := s.findImage(a, rawString(req.Image))
	if !ok {
		return p, errUnprocessable("You specified an invalid image for Droplet creation.")
	}
	if snap != nil {
		if snap.Region != p.region {
			return p, errUnprocessable("The image is not available in region %s.", p.region)
		}
		p.imageRef = snap.ImageRef
	}
	p.image = img
	for _, raw := range req.SSHKeys {
		k := a.keyByRef(rawString(raw))
		if k == nil {
			return p, errUnprocessable("You specified an invalid SSH key ID or fingerprint (%s) for Droplet creation.", rawString(raw))
		}
		p.publicKeys = append(p.publicKeys, k.PublicKey)
	}
	for _, t := range req.Tags {
		if !tagNamePattern.MatchString(t) {
			return p, errUnprocessable("tag %q is not a valid tag name", t)
		}
		p.tags = appendUnique(p.tags, canonicalTag(a, t))
	}
	p.vpcUUID = req.VPCUUID
	if def := a.defaultVPC(p.region); p.vpcUUID == "" && def != nil {
		p.vpcUUID = def.ID
	}
	if v := a.vpc(p.vpcUUID); v == nil || v.Region != p.region {
		return p, errUnprocessable("vpc_uuid must name a VPC in region %s", p.region)
	}
	p.features = []string{"private_networking"}
	if req.Backups {
		p.features = append(p.features, "backups")
	}
	if req.Monitoring {
		p.features = append(p.features, "monitoring")
	}
	return p, nil
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

type provisioning struct {
	dropletID int64
	actionID  int64
	spec      machines.Spec
}

func (s *Simulation) createDroplets(r *http.Request) (int, any, error) {
	var req createDropletRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var created []dropletJSON
	var actions []actionLink
	var jobs []provisioning
	err := s.store.Update(func(a *account) error {
		plan, err := s.planDroplets(a, req)
		if err != nil {
			return err
		}
		for _, t := range plan.tags {
			if a.tag(t) == nil {
				a.Tags = append(a.Tags, &tag{Name: t})
			}
		}
		for _, name := range plan.names {
			d := &droplet{
				ID: a.nextID(), Name: name, Region: plan.region, Size: plan.size, Image: plan.image, Tags: slices.Clone(plan.tags),
				VPCUUID: plan.vpcUUID, Features: slices.Clone(plan.features), Status: "new", Locked: true, CreatedAt: s.now().UTC(),
			}
			a.Droplets = append(a.Droplets, d)
			act := s.newAction(a, "create", d)
			created = append(created, s.render(d))
			actions = append(actions, actionLink{ID: act.ID, Rel: "create", HREF: baseURL(r) + "/v2/actions/" + strconv.FormatInt(act.ID, 10)})
			jobs = append(jobs, provisioning{dropletID: d.ID, actionID: act.ID, spec: s.machineSpec(a, d, plan, req.UserData)})
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	for _, job := range jobs {
		s.background(func(ctx context.Context) { s.provision(ctx, job) })
	}
	body := map[string]any{"links": links{Actions: actions}}
	if req.Name != "" {
		body["droplet"] = created[0]
	} else {
		body["droplets"] = created
	}
	return http.StatusAccepted, body, nil
}

func (s *Simulation) machineSpec(a *account, d *droplet, plan dropletPlan, userData string) machines.Spec {
	id := strconv.FormatInt(d.ID, 10)
	return machines.Spec{
		Name:     fmt.Sprintf("elprovo-sim-do-%s-%s", a.UUID[:8], id),
		Hostname: hostname(d.Name),
		Cloud:    cloudLabel, Account: a.UUID, Resource: id,
		Image: plan.imageRef,
		Seed: machines.Seed{
			InstanceID: id, PublicKeys: plan.publicKeys, UserData: userData,
			Metadata: func(privateIPv4, publicIPv4 string) map[string]string {
				return dropletMetadata(d, plan, userData, privateIPv4, publicIPv4)
			},
		},
		Firewall: policyFor(a, d),
		Ports:    s.publishedPorts(a, d),
	}
}

func hostname(name string) string {
	host, _, _ := strings.Cut(name, ".")
	if len(host) > 63 {
		host = host[:63]
	}
	return host
}

func (s *Simulation) publishedPorts(a *account, d *droplet) []machines.Port {
	var ports []machines.Port
	for _, rule := range s.cfg.publish {
		if !rule.matches(d.Name, d.Tags) {
			continue
		}
		holder := slices.IndexFunc(a.Droplets, func(o *droplet) bool { return o.ID != d.ID && rule.matches(o.Name, o.Tags) })
		if holder >= 0 {
			s.log.Warn("publish rule already serves another droplet; not publishing", "droplet", d.ID, "holder", a.Droplets[holder].ID)
			continue
		}
		for machinePort, hostPort := range rule.ports {
			ports = append(ports, machines.Port{Host: hostPort, Machine: machinePort})
		}
	}
	return ports
}

func (s *Simulation) provision(ctx context.Context, job provisioning) {
	m, err := s.machines.Create(ctx, job.spec)
	if err != nil {
		s.log.Error("create droplet machine", "droplet", job.dropletID, "error", err)
		s.finishAction(job.actionID, "errored", func(d *droplet) { d.Status = "off" })
		return
	}
	s.finishAction(job.actionID, "completed", func(d *droplet) {
		d.Status, d.Machine, d.PrivateIP, d.PublicIP = "active", m.ID, m.PrivateIP, m.PublicIP
	})
}

func (s *Simulation) listDroplets(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	tagName, name := q.Get("tag_name"), q.Get("name")
	if tagName != "" && (name != "" || q.Get("type") != "") {
		return 0, nil, errUnprocessable("tag_name can not be combined with name or type")
	}
	var out []dropletJSON
	if q.Get("type") != "gpus" {
		s.store.Read(func(a *account) {
			for _, d := range a.Droplets {
				if (tagName == "" || slices.Contains(d.Tags, tagName)) && (name == "" || strings.EqualFold(d.Name, name)) {
					out = append(out, s.render(d))
				}
			}
		})
	}
	return listResponse(r, "droplets", out)
}

func (s *Simulation) getDroplet(r *http.Request) (int, any, error) {
	var out *dropletJSON
	s.store.Read(func(a *account) {
		if d := a.dropletByPath(r.PathValue("id")); d != nil {
			j := s.render(d)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"droplet": out}, nil
}

type destruction struct {
	dropletID int64
	machine   string
}

func (s *Simulation) deleteDroplet(r *http.Request) (int, any, error) {
	var job destruction
	err := s.store.Update(func(a *account) error {
		d := a.dropletByPath(r.PathValue("id"))
		if d == nil {
			return errNotFound()
		}
		if d.Locked {
			return errUnprocessable("Droplet already has a pending event.")
		}
		d.Locked = true
		job = destruction{d.ID, d.Machine}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	return http.StatusNoContent, nil, nil
}

func (s *Simulation) deleteDropletsByTag(r *http.Request) (int, any, error) {
	name := r.URL.Query().Get("tag_name")
	if name == "" {
		return 0, nil, errUnprocessable("tag_name is required")
	}
	var jobs []destruction
	err := s.store.Update(func(a *account) error {
		if a.tag(name) == nil {
			return errNotFound()
		}
		tagged := a.dropletsTagged(name)
		for _, d := range tagged {
			if d.Locked {
				return errUnprocessable("Droplet %d already has a pending event.", d.ID)
			}
		}
		for _, d := range tagged {
			d.Locked = true
			jobs = append(jobs, destruction{d.ID, d.Machine})
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	for _, job := range jobs {
		s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	}
	return http.StatusNoContent, nil, nil
}

func (s *Simulation) destroy(ctx context.Context, job destruction) {
	if job.machine != "" {
		if err := s.machines.Delete(ctx, job.machine); err != nil {
			s.log.Error("delete droplet machine", "droplet", job.dropletID, "error", err)
			_ = s.store.Update(func(a *account) error {
				if d := a.droplet(job.dropletID); d != nil {
					d.Locked = false
				}
				return nil
			})
			return
		}
	}
	if err := s.store.Update(func(a *account) error { a.removeDroplet(job.dropletID); return nil }); err != nil {
		s.log.Error("remove deleted droplet", "droplet", job.dropletID, "error", err)
	}
	s.firewalls.forget(job.dropletID)
	s.firewalls.kick()
}
