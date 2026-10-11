package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const maxUserData = 32 << 10

var hostnameLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type ipv4JSON struct {
	ID      int64  `json:"id"`
	IP      string `json:"ip"`
	Blocked bool   `json:"blocked"`
	DNSPtr  string `json:"dns_ptr"`
}

type serverFirewallJSON struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type publicNetJSON struct {
	IPv4        *ipv4JSON            `json:"ipv4"`
	IPv6        *struct{}            `json:"ipv6"`
	FloatingIPs []int64              `json:"floating_ips"`
	Firewalls   []serverFirewallJSON `json:"firewalls"`
}

type privateNetJSON struct {
	Network    int64    `json:"network"`
	IP         string   `json:"ip"`
	AliasIPs   []string `json:"alias_ips"`
	MACAddress string   `json:"mac_address"`
}

type serverProtectionJSON struct {
	Delete  bool `json:"delete"`
	Rebuild bool `json:"rebuild"`
}

type serverJSON struct {
	ID              int64                `json:"id"`
	Name            string               `json:"name"`
	Status          string               `json:"status"`
	Created         string               `json:"created"`
	PublicNet       publicNetJSON        `json:"public_net"`
	PrivateNet      []privateNetJSON     `json:"private_net"`
	ServerType      serverTypeJSON       `json:"server_type"`
	Location        locationJSON         `json:"location"`
	Image           *imageJSON           `json:"image"`
	ISO             *struct{}            `json:"iso"`
	RescueEnabled   bool                 `json:"rescue_enabled"`
	Locked          bool                 `json:"locked"`
	BackupWindow    *string              `json:"backup_window"`
	OutgoingTraffic *int64               `json:"outgoing_traffic"`
	IngoingTraffic  *int64               `json:"ingoing_traffic"`
	IncludedTraffic *int64               `json:"included_traffic"`
	Protection      serverProtectionJSON `json:"protection"`
	Labels          map[string]string    `json:"labels"`
	Volumes         []int64              `json:"volumes"`
	LoadBalancers   []int64              `json:"load_balancers"`
	PrimaryDiskSize int                  `json:"primary_disk_size"`
	PlacementGroup  *struct{}            `json:"placement_group"`
}

func (s *Simulation) renderServer(p *project, srv *server) serverJSON {
	t, _ := findServerType(srv.ServerType)
	loc, _ := location(srv.Location)
	out := serverJSON{
		ID: srv.ID, Name: srv.Name, Status: srv.Status, Created: timestamp(srv.Created),
		PublicNet:  publicNetJSON{FloatingIPs: []int64{}, Firewalls: []serverFirewallJSON{}},
		PrivateNet: []privateNetJSON{}, ServerType: t.render(), Location: loc, Image: renderImageByID(p, srv.Image),
		Labels: labelsOrEmpty(srv.Labels), Volumes: []int64{}, LoadBalancers: []int64{}, PrimaryDiskSize: t.disk,
	}
	if ip := p.primaryIPOf(srv.ID); ip != nil {
		out.PublicNet.IPv4 = &ipv4JSON{ID: ip.ID, IP: ip.IP}
	}
	settled := s.firewallSettled(p, srv)
	for _, f := range p.firewallsOf(srv) {
		status := "pending"
		if settled {
			status = "applied"
		}
		out.PublicNet.Firewalls = append(out.PublicNet.Firewalls, serverFirewallJSON{ID: f.ID, Status: status})
	}
	if srv.PrivateIP != "" {
		for _, id := range srv.Networks {
			out.PrivateNet = append(out.PrivateNet, privateNetJSON{Network: id, IP: srv.PrivateIP, AliasIPs: []string{}, MACAddress: macAddress(srv.ID, id)})
		}
	}
	return out
}

// macAddress is the MAC address the API reports for a server's interface in
// a network. Hetzner's private interfaces start with 86:00:00.
func macAddress(serverID, networkID int64) string {
	v := uint32(serverID*31 + networkID)
	return fmt.Sprintf("86:00:00:%02x:%02x:%02x", byte(v>>16), byte(v>>8), byte(v))
}

type createServerRequest struct {
	Name             string            `json:"name"`
	Location         json.RawMessage   `json:"location"`
	Datacenter       json.RawMessage   `json:"datacenter"`
	ServerType       json.RawMessage   `json:"server_type"`
	StartAfterCreate *bool             `json:"start_after_create"`
	Image            json.RawMessage   `json:"image"`
	PlacementGroup   *int64            `json:"placement_group"`
	SSHKeys          []json.RawMessage `json:"ssh_keys"`
	Volumes          []int64           `json:"volumes"`
	Networks         []int64           `json:"networks"`
	Firewalls        []struct {
		Firewall int64 `json:"firewall"`
	} `json:"firewalls"`
	UserData  string            `json:"user_data"`
	Labels    map[string]string `json:"labels"`
	Automount *bool             `json:"automount"`
	PublicNet *struct {
		EnableIPv4 *bool  `json:"enable_ipv4"`
		EnableIPv6 *bool  `json:"enable_ipv6"`
		IPv4       *int64 `json:"ipv4"`
		IPv6       *int64 `json:"ipv6"`
	} `json:"public_net"`
}

// serverPlan is a validated create request.
type serverPlan struct {
	name       string
	serverType serverType
	location   locationJSON
	image      resolvedImage
	publicKeys []string
	labels     map[string]string
	networks   []int64
	firewalls  []int64
	primaryIP  int64
	userData   string
}

func checkHostname(name string) error {
	if len(name) == 0 || len(name) > 253 {
		return errInvalidInput("name", "must be a valid hostname as per RFC 1123")
	}
	for _, label := range strings.Split(name, ".") {
		if !hostnameLabel.MatchString(label) {
			return errInvalidInput("name", "must be a valid hostname as per RFC 1123")
		}
	}
	return nil
}

func (s *Simulation) planServer(p *project, req createServerRequest) (serverPlan, error) {
	plan := serverPlan{name: req.Name, labels: req.Labels, userData: req.UserData, networks: slices.Compact(slices.Sorted(slices.Values(req.Networks)))}
	if err := checkUnsupportedServerFields(req); err != nil {
		return plan, err
	}
	if err := checkHostname(req.Name); err != nil {
		return plan, err
	}
	if slices.ContainsFunc(p.Servers, func(o *server) bool { return o.Name == req.Name }) {
		return plan, errUniqueness("name", "server name is already used")
	}
	if len(req.UserData) > maxUserData {
		return plan, errInvalidInput("user_data", "must be 32 KiB or smaller")
	}
	if err := checkLabels(req.Labels); err != nil {
		return plan, err
	}
	typeID, typeName := idOrName(req.ServerType)
	t, ok := findServerType(firstNonEmpty(typeName, strconv.FormatInt(typeID, 10)))
	if !ok {
		return plan, errInvalidInput("server_type", "server type %s not found", string(req.ServerType))
	}
	plan.serverType = t
	plan.location, _ = location(t.locations[0])
	if len(req.Location) > 0 {
		locID, locName := idOrName(req.Location)
		if plan.location, ok = location(firstNonEmpty(locName, strconv.FormatInt(locID, 10))); !ok {
			return plan, errInvalidInput("location", "location %s not found", string(req.Location))
		}
	}
	if !t.availableIn(plan.location.Name) {
		return plan, errCode(http.StatusUnprocessableEntity, "invalid_server_type", "server type %s is not available in location %s", t.name, plan.location.Name)
	}
	imageID, imageName := idOrName(req.Image)
	if plan.image, ok = findImage(p, imageID, imageName, t.architecture); !ok {
		return plan, errInvalidInput("image", "image %s not found for architecture %s", string(req.Image), t.architecture)
	}
	if plan.image.architecture != t.architecture {
		return plan, errInvalidInput("image", "image architecture %s does not match server type architecture %s", plan.image.architecture, t.architecture)
	}
	for _, raw := range req.SSHKeys {
		k, err := findSSHKey(p, raw)
		if err != nil {
			return plan, err
		}
		plan.publicKeys = append(plan.publicKeys, k.PublicKey)
	}
	if err := s.planServerNetworks(p, &plan); err != nil {
		return plan, err
	}
	for _, f := range req.Firewalls {
		if p.firewall(f.Firewall) == nil {
			return plan, errInvalidInput("firewalls", "firewall %d not found", f.Firewall)
		}
		plan.firewalls = append(plan.firewalls, f.Firewall)
	}
	if len(plan.firewalls) > maxFirewallsPerServer {
		return plan, errResourceLimit("firewalls_per_server", fmt.Sprintf("a server can have at most %d firewalls", maxFirewallsPerServer))
	}
	if req.PublicNet != nil && req.PublicNet.IPv4 != nil {
		ip := p.primaryIP(*req.PublicNet.IPv4)
		switch {
		case ip == nil:
			return plan, errInvalidInput("public_net", "primary IP %d not found", *req.PublicNet.IPv4)
		case ip.AssigneeID != 0:
			return plan, errCode(http.StatusUnprocessableEntity, "primary_ip_assigned", "primary IP %d is already assigned to a server", ip.ID)
		case ip.Location != plan.location.Name:
			return plan, errCode(http.StatusUnprocessableEntity, "primary_ip_datacenter_mismatch", "primary IP %d is in location %s, not %s", ip.ID, ip.Location, plan.location.Name)
		}
		plan.primaryIP = ip.ID
	}
	return plan, nil
}

// checkUnsupportedServerFields refuses create options that Hetzner accepts
// but the simulation cannot honor.
func checkUnsupportedServerFields(req createServerRequest) error {
	pub := req.PublicNet
	switch {
	case len(req.Datacenter) > 0:
		return errInvalidInput("datacenter", "datacenters were removed from the API; use location")
	case req.StartAfterCreate != nil && !*req.StartAfterCreate:
		return errUnsupported("start_after_create false is not supported by the simulation: servers start when they are created")
	case len(req.Volumes) > 0 || req.Automount != nil:
		return errUnsupported("volumes are not supported by the simulation")
	case req.PlacementGroup != nil:
		return errUnsupported("placement groups are not supported by the simulation")
	case pub == nil || pub.EnableIPv6 == nil || *pub.EnableIPv6 || pub.IPv6 != nil:
		return errUnsupported("IPv6 is not supported by the simulation: set public_net.enable_ipv6 to false")
	case pub.EnableIPv4 != nil && !*pub.EnableIPv4:
		return errUnsupported("servers without a public IPv4 address are not supported by the simulation: every server has one")
	case len(req.Networks) > 1:
		return errUnsupported("the simulation attaches a server to one network at most")
	}
	return nil
}

func (s *Simulation) planServerNetworks(p *project, plan *serverPlan) error {
	for _, id := range plan.networks {
		n := p.network(id)
		if n == nil {
			return errInvalidInput("networks", "network %d not found", id)
		}
		if !n.hasZone(plan.location.NetworkZone) {
			return errCode(http.StatusUnprocessableEntity, "no_subnet_available", "network %d has no subnet in network zone %s", id, plan.location.NetworkZone)
		}
	}
	return nil
}

func findSSHKey(p *project, raw json.RawMessage) (*sshKey, error) {
	id, name := idOrName(raw)
	for _, k := range p.SSHKeys {
		if (id != 0 && k.ID == id) || (name != "" && k.Name == name) {
			return k, nil
		}
	}
	return nil, errInvalidInput("ssh_keys", "SSH key %s not found", string(raw))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" && v != "0" {
			return v
		}
	}
	return ""
}

type provisioning struct {
	serverID     int64
	createAction int64
	startAction  int64
	spec         machines.Spec
}

func (s *Simulation) createServer(r *http.Request) (int, any, error) {
	var req createServerRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var body map[string]any
	var job provisioning
	err := s.store.Update(func(p *project) error {
		plan, err := s.planServer(p, req)
		if err != nil {
			return err
		}
		ip, err := s.primaryIPFor(p, plan)
		if err != nil {
			return err
		}
		srv := &server{
			ID: p.nextID(), Name: plan.name, Status: "initializing", ServerType: plan.serverType.name, Location: plan.location.Name,
			Image: plan.image.id, Labels: plan.labels, Created: s.now().UTC(), Networks: plan.networks,
		}
		p.Servers = append(p.Servers, srv)
		ip.AssigneeID = srv.ID
		for _, id := range plan.firewalls {
			p.firewall(id).applyToServer(srv.ID)
		}
		ref := resourceRef{ID: srv.ID, Type: "server"}
		create, start := s.startAction(p, "create_server", ref), s.startAction(p, "start_server", ref)
		job = provisioning{serverID: srv.ID, createAction: create.ID, startAction: start.ID, spec: s.machineSpec(p, srv, plan, ip.IP)}
		body = map[string]any{
			"server": s.renderServer(p, srv), "action": create.render(), "next_actions": []actionJSON{start.render()}, "root_password": nil,
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.provision(ctx, job) })
	return http.StatusCreated, body, nil
}

// primaryIPFor returns the primary IP the plan names, or a new one that is
// deleted with the server, as Hetzner creates when none is named.
func (s *Simulation) primaryIPFor(p *project, plan serverPlan) (*primaryIP, error) {
	if plan.primaryIP != 0 {
		return p.primaryIP(plan.primaryIP), nil
	}
	address, err := s.allocateAddress(p)
	if err != nil {
		return nil, err
	}
	id := p.nextID()
	ip := &primaryIP{ID: id, Name: "primary_ip-" + strconv.FormatInt(id, 10), IP: address, Location: plan.location.Name, AutoDelete: true, Created: s.now().UTC()}
	p.PrimaryIPs = append(p.PrimaryIPs, ip)
	return ip, nil
}

func (s *Simulation) machineSpec(p *project, srv *server, plan serverPlan, publicIP string) machines.Spec {
	id := strconv.FormatInt(srv.ID, 10)
	meta := metadataInput{
		server: *srv, location: plan.location, publicKeys: slices.Clone(plan.publicKeys), userData: plan.userData,
		networks: attachedNetworks(p, srv),
	}
	host, _, _ := strings.Cut(srv.Name, ".")
	return machines.Spec{
		Name:     fmt.Sprintf("stagely-sim-hz-%s-%s", p.UUID[:8], id),
		Hostname: host,
		Cloud:    cloudLabel, Account: p.UUID, Resource: id,
		Image:    plan.image.ref,
		PublicIP: publicIP,
		Seed: machines.Seed{
			InstanceID: id, PublicKeys: plan.publicKeys, UserData: plan.userData,
			Metadata: func(privateIPv4, publicIPv4 string) map[string]string {
				return serverMetadata(meta, privateIPv4, publicIPv4)
			},
		},
		Firewall: s.policyFor(p, srv),
	}
}

func (s *Simulation) provision(ctx context.Context, job provisioning) {
	m, err := s.machines.Create(ctx, job.spec)
	if err != nil {
		s.log.Error("create server machine", "server", job.serverID, "error", err)
		failure := &actionError{Code: "server_error", Message: "the server's machine could not be created"}
		s.finishAction(job.startAction, failure, nil)
		s.finishAction(job.createAction, failure, func(p *project) {
			if srv := p.server(job.serverID); srv != nil {
				srv.Status = "off"
			}
		})
		return
	}
	s.finishAction(job.startAction, nil, nil)
	s.finishAction(job.createAction, nil, func(p *project) {
		if srv := p.server(job.serverID); srv != nil {
			srv.Status, srv.Machine, srv.PrivateIP = "running", m.ID, m.PrivateIP
		}
	})
}

var serverSortFields = sortFields[serverJSON]{
	"id":      byID(func(s serverJSON) int64 { return s.ID }),
	"name":    byString(func(s serverJSON) string { return s.Name }),
	"created": byString(func(s serverJSON) string { return s.Created }),
}

func (s *Simulation) listServers(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	name, statuses := q.Get("name"), q["status"]
	var out []serverJSON
	s.store.Read(func(p *project) {
		for _, srv := range p.Servers {
			if (name == "" || srv.Name == name) && (len(statuses) == 0 || slices.Contains(statuses, srv.Status)) && keep(srv.Labels) {
				out = append(out, s.renderServer(p, srv))
			}
		}
	})
	return listResponse(r, "servers", out, serverSortFields)
}

func (s *Simulation) getServer(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "server")
	if err != nil {
		return 0, nil, err
	}
	var out *serverJSON
	s.store.Read(func(p *project) {
		if srv := p.server(id); srv != nil {
			j := s.renderServer(p, srv)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("server")
	}
	return http.StatusOK, map[string]any{"server": out}, nil
}

// updateServer renames a server or replaces its labels. A new name does not
// change the machine's hostname.
func (s *Simulation) updateServer(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "server")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Name   string             `json:"name"`
		Labels *map[string]string `json:"labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Name != "" {
		if err := checkHostname(req.Name); err != nil {
			return 0, nil, err
		}
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out serverJSON
	err = s.store.Update(func(p *project) error {
		srv := p.server(id)
		if srv == nil {
			return errNotFound("server")
		}
		if req.Name != "" && req.Name != srv.Name {
			if slices.ContainsFunc(p.Servers, func(o *server) bool { return o.Name == req.Name }) {
				return errUniqueness("name", "server name is already used")
			}
			srv.Name = req.Name
		}
		if req.Labels != nil {
			srv.Labels = *req.Labels
		}
		out = s.renderServer(p, srv)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusOK, map[string]any{"server": out}, nil
}

type destruction struct {
	serverID int64
	actionID int64
	machine  string
	status   string
}

// deleteServer starts deleting the server. The server answers GET until its
// machine is gone; then its delete_server action succeeds.
func (s *Simulation) deleteServer(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "server")
	if err != nil {
		return 0, nil, err
	}
	var job destruction
	var out actionJSON
	err = s.store.Update(func(p *project) error {
		srv := p.server(id)
		if srv == nil {
			return errNotFound("server")
		}
		ref := resourceRef{ID: id, Type: "server"}
		if p.running(ref) {
			return errLocked("server")
		}
		a := s.startAction(p, "delete_server", ref)
		job = destruction{serverID: id, actionID: a.ID, machine: srv.Machine, status: srv.Status}
		srv.Status = "deleting"
		out = a.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	return http.StatusOK, map[string]any{"action": out}, nil
}

func (s *Simulation) destroy(ctx context.Context, job destruction) {
	if job.machine != "" {
		if err := s.machines.Delete(ctx, job.machine); err != nil {
			s.log.Error("delete server machine", "server", job.serverID, "error", err)
			s.finishAction(job.actionID, &actionError{Code: "server_error", Message: "the server's machine could not be deleted"}, func(p *project) {
				if srv := p.server(job.serverID); srv != nil {
					srv.Status = job.status
				}
			})
			return
		}
	}
	s.finishAction(job.actionID, nil, func(p *project) { p.removeServer(job.serverID) })
	s.firewalls.forget(job.serverID)
}
