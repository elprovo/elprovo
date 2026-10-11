package digitalocean

import (
	"slices"
	"strconv"
	"time"
)

type account struct {
	UUID        string        `json:"uuid"`
	NextID      int64         `json:"next_id"`
	Keys        []*sshKey     `json:"ssh_keys"`
	Droplets    []*droplet    `json:"droplets"`
	Actions     []*action     `json:"actions"`
	Tags        []*tag        `json:"tags"`
	VPCs        []*vpc        `json:"vpcs"`
	Firewalls   []*firewall   `json:"firewalls"`
	Snapshots   []*snapshot   `json:"snapshots"`
	ReservedIPs []*reservedIP `json:"reserved_ips"`
	Registry    *registry     `json:"registry,omitempty"`
}

type sshKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

type droplet struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Region      string    `json:"region"`
	Size        string    `json:"size"`
	Image       imageJSON `json:"image"`
	Tags        []string  `json:"tags"`
	VPCUUID     string    `json:"vpc_uuid"`
	Features    []string  `json:"features"`
	Status      string    `json:"status"`
	Locked      bool      `json:"locked"`
	CreatedAt   time.Time `json:"created_at"`
	Machine     string    `json:"machine"`
	PrivateIP   string    `json:"private_ip"`
	PublicIP    string    `json:"public_ip"`
	SnapshotIDs []int64   `json:"snapshot_ids"`
}

type action struct {
	ID           int64      `json:"id"`
	Status       string     `json:"status"`
	Type         string     `json:"type"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	ResourceID   int64      `json:"resource_id"`
	ResourceType string     `json:"resource_type"`
	Region       string     `json:"region"`
}

type tag struct {
	Name string `json:"name"`
}

type vpc struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Region      string    `json:"region"`
	IPRange     string    `json:"ip_range"`
	Default     bool      `json:"default"`
	CreatedAt   time.Time `json:"created_at"`
}

type firewall struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	InboundRules  []inboundRule  `json:"inbound_rules"`
	OutboundRules []outboundRule `json:"outbound_rules"`
	DropletIDs    []int64        `json:"droplet_ids"`
	Tags          []string       `json:"tags"`
	CreatedAt     time.Time      `json:"created_at"`
}

type snapshot struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Region        string    `json:"region"`
	CreatedAt     time.Time `json:"created_at"`
	DropletID     int64     `json:"droplet_id"`
	MinDiskSize   int       `json:"min_disk_size"`
	SizeGigabytes float64   `json:"size_gigabytes"`
	Tags          []string  `json:"tags"`
	ImageRef      string    `json:"image_ref"`
}

type registry struct {
	Name      string    `json:"name"`
	Region    string    `json:"region"`
	Tier      string    `json:"tier"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (a *account) nextID() int64 {
	a.NextID++
	return a.NextID
}

func (a *account) droplet(id int64) *droplet {
	for _, d := range a.Droplets {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (a *account) dropletByPath(value string) *droplet {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return a.droplet(id)
}

func (a *account) removeDroplet(id int64) {
	a.Droplets = slices.DeleteFunc(a.Droplets, func(d *droplet) bool { return d.ID == id })
	for _, r := range a.ReservedIPs {
		if r.DropletID == id {
			r.DropletID = 0
		}
	}
	for _, f := range a.Firewalls {
		f.DropletIDs = slices.DeleteFunc(f.DropletIDs, func(v int64) bool { return v == id })
	}
}

func (a *account) action(id int64) *action {
	for _, act := range a.Actions {
		if act.ID == id {
			return act
		}
	}
	return nil
}

func (a *account) tag(name string) *tag {
	for _, t := range a.Tags {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (a *account) vpc(id string) *vpc {
	for _, v := range a.VPCs {
		if v.ID == id {
			return v
		}
	}
	return nil
}

func (a *account) defaultVPC(region string) *vpc {
	for _, v := range a.VPCs {
		if v.Region == region && v.Default {
			return v
		}
	}
	return nil
}

func (a *account) firewall(id string) *firewall {
	for _, f := range a.Firewalls {
		if f.ID == id {
			return f
		}
	}
	return nil
}

func (a *account) snapshot(id int64) *snapshot {
	for _, s := range a.Snapshots {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (a *account) dropletsTagged(name string) []*droplet {
	var out []*droplet
	for _, d := range a.Droplets {
		if slices.Contains(d.Tags, name) {
			out = append(out, d)
		}
	}
	return out
}
