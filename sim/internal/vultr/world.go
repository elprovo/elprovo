package vultr

import (
	"slices"
	"time"
)

type account struct {
	ID             string           `json:"id"`
	Instances      []*instance      `json:"instances"`
	SSHKeys        []*sshKey        `json:"ssh_keys"`
	VPCs           []*vpc           `json:"vpcs"`
	FirewallGroups []*firewallGroup `json:"firewall_groups"`
	ReservedIPs    []*reservedIP    `json:"reserved_ips"`
	Snapshots      []*snapshot      `json:"snapshots"`
	Registries     []*registry      `json:"registries"`
}

type instance struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Hostname        string   `json:"hostname"`
	Region          string   `json:"region"`
	Plan            string   `json:"plan"`
	OSID            int      `json:"os_id"`
	SnapshotID      string   `json:"snapshot_id"`
	Tags            []string `json:"tags"`
	UserData        string   `json:"user_data"`
	VPCs            []string `json:"vpcs"`
	FirewallGroupID string   `json:"firewall_group_id"`
	// MainReservedIP is the reserved IP the instance was created with, which
	// is its main address.
	MainReservedIP string    `json:"main_reserved_ip"`
	Status         string    `json:"status"`
	PowerStatus    string    `json:"power_status"`
	ServerStatus   string    `json:"server_status"`
	Destroying     bool      `json:"destroying"`
	CreatedAt      time.Time `json:"created_at"`
	Machine        string    `json:"machine"`
	PrivateIP      string    `json:"private_ip"`
	PublicIP       string    `json:"public_ip"`
}

type sshKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SSHKey    string    `json:"ssh_key"`
	CreatedAt time.Time `json:"created_at"`
}

type vpc struct {
	ID          string    `json:"id"`
	Region      string    `json:"region"`
	Description string    `json:"description"`
	Subnet      string    `json:"subnet"`
	SubnetMask  int       `json:"subnet_mask"`
	CreatedAt   time.Time `json:"created_at"`
}

type firewallGroup struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Rules       []firewallRule `json:"rules"`
	LastRuleID  int            `json:"last_rule_id"`
	CreatedAt   time.Time      `json:"created_at"`
	ModifiedAt  time.Time      `json:"modified_at"`
}

type firewallRule struct {
	ID         int    `json:"id"`
	IPType     string `json:"ip_type"`
	Protocol   string `json:"protocol"`
	Port       string `json:"port"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Source     string `json:"source"`
	Notes      string `json:"notes"`
}

type reservedIP struct {
	ID         string `json:"id"`
	Region     string `json:"region"`
	Address    string `json:"address"`
	Label      string `json:"label"`
	InstanceID string `json:"instance_id"`
}

type snapshot struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Size        int64     `json:"size"`
	OSID        int       `json:"os_id"`
	CreatedAt   time.Time `json:"created_at"`
	ImageRef    string    `json:"image_ref"`
}

type registry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Region    string    `json:"region"`
	Plan      string    `json:"plan"`
	Public    bool      `json:"public"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func find[T any](items []*T, match func(*T) bool) *T {
	if i := slices.IndexFunc(items, match); i >= 0 {
		return items[i]
	}
	return nil
}

func (a *account) instance(id string) *instance {
	return find(a.Instances, func(i *instance) bool { return i.ID == id })
}

func (a *account) sshKey(id string) *sshKey {
	return find(a.SSHKeys, func(k *sshKey) bool { return k.ID == id })
}

func (a *account) vpc(id string) *vpc {
	return find(a.VPCs, func(v *vpc) bool { return v.ID == id })
}

func (a *account) firewallGroup(id string) *firewallGroup {
	return find(a.FirewallGroups, func(g *firewallGroup) bool { return g.ID == id })
}

func (a *account) reservedIP(id string) *reservedIP {
	return find(a.ReservedIPs, func(r *reservedIP) bool { return r.ID == id })
}

func (a *account) snapshot(id string) *snapshot {
	return find(a.Snapshots, func(s *snapshot) bool { return s.ID == id })
}

func (a *account) registry(id string) *registry {
	return find(a.Registries, func(r *registry) bool { return r.ID == id })
}

// extraAddresses returns the reserved IPs attached to the instance besides
// its main address, which its machine answers at as extra addresses.
func (a *account) extraAddresses(instanceID string) []string {
	main := ""
	if inst := a.instance(instanceID); inst != nil {
		main = inst.MainReservedIP
	}
	addrs := []string{}
	for _, r := range a.ReservedIPs {
		if r.InstanceID == instanceID && r.ID != main {
			addrs = append(addrs, r.Address)
		}
	}
	return addrs
}

// removeInstance forgets a deleted instance and detaches its reserved IPs.
func (a *account) removeInstance(id string) {
	a.Instances = slices.DeleteFunc(a.Instances, func(i *instance) bool { return i.ID == id })
	for _, r := range a.ReservedIPs {
		if r.InstanceID == id {
			r.InstanceID = ""
		}
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}
