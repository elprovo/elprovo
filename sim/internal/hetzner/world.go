package hetzner

import (
	"slices"
	"time"
)

// project is one Hetzner Cloud project, the scope of every API token.
type project struct {
	UUID       string       `json:"uuid"`
	NextID     int64        `json:"next_id"`
	SSHKeys    []*sshKey    `json:"ssh_keys"`
	Servers    []*server    `json:"servers"`
	PrimaryIPs []*primaryIP `json:"primary_ips"`
	Firewalls  []*firewall  `json:"firewalls"`
	Networks   []*network   `json:"networks"`
	Images     []*image     `json:"images"`
	Actions    []*action    `json:"actions"`
}

type sshKey struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Fingerprint string            `json:"fingerprint"`
	PublicKey   string            `json:"public_key"`
	Labels      map[string]string `json:"labels"`
	Created     time.Time         `json:"created"`
}

// server is a server and the machine that runs it. Its public IPv4 address
// is the primary IP that names it as assignee; its firewalls are the ones
// that apply to it directly or through a label selector.
type server struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	ServerType string            `json:"server_type"`
	Location   string            `json:"location"`
	Image      int64             `json:"image"`
	Labels     map[string]string `json:"labels"`
	Created    time.Time         `json:"created"`
	Machine    string            `json:"machine"`
	PrivateIP  string            `json:"private_ip"`
	Networks   []int64           `json:"networks"`
}

type primaryIP struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	IP         string            `json:"ip"`
	Location   string            `json:"location"`
	AssigneeID int64             `json:"assignee_id"`
	AutoDelete bool              `json:"auto_delete"`
	Labels     map[string]string `json:"labels"`
	Created    time.Time         `json:"created"`
}

type firewall struct {
	ID        int64              `json:"id"`
	Name      string             `json:"name"`
	Labels    map[string]string  `json:"labels"`
	Rules     []firewallRule     `json:"rules"`
	AppliedTo []firewallResource `json:"applied_to"`
	Created   time.Time          `json:"created"`
}

type network struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	IPRange string            `json:"ip_range"`
	Subnets []subnet          `json:"subnets"`
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"created"`
}

type subnet struct {
	Type        string `json:"type"`
	IPRange     string `json:"ip_range"`
	NetworkZone string `json:"network_zone"`
	Gateway     string `json:"gateway"`
}

// image is a snapshot. Its Ref names the Docker image its machine was
// committed to.
type image struct {
	ID           int64             `json:"id"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Created      time.Time         `json:"created"`
	ImageSize    float64           `json:"image_size"`
	DiskSize     int               `json:"disk_size"`
	ServerID     int64             `json:"server_id"`
	ServerName   string            `json:"server_name"`
	Architecture string            `json:"architecture"`
	Ref          string            `json:"ref"`
}

type action struct {
	ID        int64         `json:"id"`
	Command   string        `json:"command"`
	Status    string        `json:"status"`
	Progress  int           `json:"progress"`
	Started   time.Time     `json:"started"`
	Finished  *time.Time    `json:"finished"`
	Resources []resourceRef `json:"resources"`
	Error     *actionError  `json:"error"`
}

type resourceRef struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type actionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *project) nextID() int64 {
	p.NextID++
	return p.NextID
}

func (p *project) sshKey(id int64) *sshKey {
	for _, k := range p.SSHKeys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func (p *project) server(id int64) *server {
	for _, s := range p.Servers {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (p *project) primaryIP(id int64) *primaryIP {
	for _, ip := range p.PrimaryIPs {
		if ip.ID == id {
			return ip
		}
	}
	return nil
}

// primaryIPOf returns the primary IP assigned to the server, or nil.
func (p *project) primaryIPOf(serverID int64) *primaryIP {
	for _, ip := range p.PrimaryIPs {
		if ip.AssigneeID == serverID {
			return ip
		}
	}
	return nil
}

func (p *project) firewall(id int64) *firewall {
	for _, f := range p.Firewalls {
		if f.ID == id {
			return f
		}
	}
	return nil
}

func (p *project) network(id int64) *network {
	for _, n := range p.Networks {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (p *project) image(id int64) *image {
	for _, img := range p.Images {
		if img.ID == id {
			return img
		}
	}
	return nil
}

func (p *project) action(id int64) *action {
	for _, a := range p.Actions {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// removeServer forgets a deleted server: its auto-delete primary IP goes
// with it, other primary IPs become unassigned, and firewalls stop naming
// it.
func (p *project) removeServer(id int64) {
	p.Servers = slices.DeleteFunc(p.Servers, func(s *server) bool { return s.ID == id })
	p.PrimaryIPs = slices.DeleteFunc(p.PrimaryIPs, func(ip *primaryIP) bool { return ip.AssigneeID == id && ip.AutoDelete })
	for _, ip := range p.PrimaryIPs {
		if ip.AssigneeID == id {
			ip.AssigneeID = 0
		}
	}
	for _, f := range p.Firewalls {
		f.AppliedTo = slices.DeleteFunc(f.AppliedTo, func(r firewallResource) bool { return r.Server != nil && r.Server.ID == id })
	}
}
