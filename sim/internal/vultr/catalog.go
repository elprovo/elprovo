package vultr

import (
	"net/http"
	"slices"
)

// ubuntuOS is the only operating system the simulation boots: the machine
// image is Ubuntu 24.04.
var ubuntuOS = osJSON{ID: 2284, Name: "Ubuntu 24.04 LTS x64", Arch: "x64", Family: "ubuntu"}

type osJSON struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Family string `json:"family"`
}

type regionJSON struct {
	ID           string   `json:"id"`
	City         string   `json:"city"`
	Country      string   `json:"country"`
	Continent    string   `json:"continent"`
	Options      []string `json:"options"`
	Connectivity []string `json:"connectivity"`
}

// regions are Vultr's regions as GET /v2/regions listed them on 2026-10-11.
var regions = []regionJSON{
	{ID: "ams", City: "Amsterdam", Country: "NL", Continent: "Europe"},
	{ID: "atl", City: "Atlanta", Country: "US", Continent: "North America"},
	{ID: "blr", City: "Bangalore", Country: "IN", Continent: "Asia"},
	{ID: "bom", City: "Mumbai", Country: "IN", Continent: "Asia"},
	{ID: "cdg", City: "Paris", Country: "FR", Continent: "Europe"},
	{ID: "del", City: "Delhi NCR", Country: "IN", Continent: "Asia"},
	{ID: "dfw", City: "Dallas", Country: "US", Continent: "North America"},
	{ID: "ewr", City: "New Jersey", Country: "US", Continent: "North America"},
	{ID: "fra", City: "Frankfurt", Country: "DE", Continent: "Europe"},
	{ID: "hnl", City: "Honolulu", Country: "US", Continent: "North America"},
	{ID: "icn", City: "Seoul", Country: "KR", Continent: "Asia"},
	{ID: "itm", City: "Osaka", Country: "JP", Continent: "Asia"},
	{ID: "jnb", City: "Johannesburg", Country: "ZA", Continent: "Africa"},
	{ID: "lax", City: "Los Angeles", Country: "US", Continent: "North America"},
	{ID: "lhr", City: "London", Country: "GB", Continent: "Europe"},
	{ID: "mad", City: "Madrid", Country: "ES", Continent: "Europe"},
	{ID: "man", City: "Manchester", Country: "GB", Continent: "Europe"},
	{ID: "mel", City: "Melbourne", Country: "AU", Continent: "Australia"},
	{ID: "mex", City: "Mexico City", Country: "MX", Continent: "North America"},
	{ID: "mia", City: "Miami", Country: "US", Continent: "North America"},
	{ID: "mxp", City: "Milan", Country: "IT", Continent: "Europe"},
	{ID: "nrt", City: "Tokyo", Country: "JP", Continent: "Asia"},
	{ID: "ord", City: "Chicago", Country: "US", Continent: "North America"},
	{ID: "sao", City: "São Paulo", Country: "BR", Continent: "South America"},
	{ID: "scl", City: "Santiago", Country: "CL", Continent: "South America"},
	{ID: "sea", City: "Seattle", Country: "US", Continent: "North America"},
	{ID: "sgp", City: "Singapore", Country: "SG", Continent: "Asia"},
	{ID: "sjc", City: "Silicon Valley", Country: "US", Continent: "North America"},
	{ID: "sto", City: "Stockholm", Country: "SE", Continent: "Europe"},
	{ID: "syd", City: "Sydney", Country: "AU", Continent: "Australia"},
	{ID: "tlv", City: "Tel Aviv", Country: "IL", Continent: "Asia"},
	{ID: "waw", City: "Warsaw", Country: "PL", Continent: "Europe"},
	{ID: "yto", City: "Toronto", Country: "CA", Continent: "North America"},
}

type planJSON struct {
	ID          string   `json:"id"`
	VCPUCount   int      `json:"vcpu_count"`
	RAM         int      `json:"ram"`
	Disk        int      `json:"disk"`
	DiskCount   int      `json:"disk_count"`
	Bandwidth   int      `json:"bandwidth"`
	MonthlyCost float64  `json:"monthly_cost"`
	HourlyCost  float64  `json:"hourly_cost"`
	Type        string   `json:"type"`
	Locations   []string `json:"locations"`
}

// plans are the Cloud Compute (vc2) plans that GET /v2/plans listed for most
// regions on 2026-10-11. Here every region offers each of them.
var plans = []planJSON{
	{ID: "vc2-1c-0.5gb", VCPUCount: 1, RAM: 512, Disk: 10, Bandwidth: 512, MonthlyCost: 3.5, HourlyCost: 0.005},
	{ID: "vc2-1c-1gb", VCPUCount: 1, RAM: 1024, Disk: 25, Bandwidth: 1024, MonthlyCost: 5, HourlyCost: 0.007},
	{ID: "vc2-1c-2gb", VCPUCount: 1, RAM: 2048, Disk: 55, Bandwidth: 2048, MonthlyCost: 10, HourlyCost: 0.014},
	{ID: "vc2-2c-2gb", VCPUCount: 2, RAM: 2048, Disk: 65, Bandwidth: 3072, MonthlyCost: 15, HourlyCost: 0.021},
	{ID: "vc2-2c-4gb", VCPUCount: 2, RAM: 4096, Disk: 80, Bandwidth: 3072, MonthlyCost: 20, HourlyCost: 0.027},
	{ID: "vc2-4c-8gb", VCPUCount: 4, RAM: 8192, Disk: 160, Bandwidth: 4096, MonthlyCost: 40, HourlyCost: 0.055},
	{ID: "vc2-6c-16gb", VCPUCount: 6, RAM: 16384, Disk: 320, Bandwidth: 5120, MonthlyCost: 80, HourlyCost: 0.11},
	{ID: "vc2-8c-32gb", VCPUCount: 8, RAM: 32768, Disk: 640, Bandwidth: 6144, MonthlyCost: 160, HourlyCost: 0.219},
	{ID: "vc2-16c-64gb", VCPUCount: 16, RAM: 65536, Disk: 1280, Bandwidth: 10240, MonthlyCost: 320, HourlyCost: 0.438},
	{ID: "vc2-24c-96gb", VCPUCount: 24, RAM: 98304, Disk: 1600, Bandwidth: 15360, MonthlyCost: 640, HourlyCost: 0.877},
}

func region(id string) (regionJSON, bool) {
	i := slices.IndexFunc(regions, func(r regionJSON) bool { return r.ID == id })
	if i < 0 {
		return regionJSON{}, false
	}
	return withRegionDefaults(regions[i]), true
}

func withRegionDefaults(r regionJSON) regionJSON {
	r.Options = []string{"ddos_protection", "block_storage_storage_opt", "load_balancers", "kubernetes"}
	r.Connectivity = []string{"public_ip", "nat_gateway"}
	return r
}

func plan(id string) (planJSON, bool) {
	i := slices.IndexFunc(plans, func(p planJSON) bool { return p.ID == id })
	if i < 0 {
		return planJSON{}, false
	}
	return withPlanDefaults(plans[i]), true
}

func withPlanDefaults(p planJSON) planJSON {
	p.DiskCount, p.Type = 1, "vc2"
	for _, r := range regions {
		p.Locations = append(p.Locations, r.ID)
	}
	return p
}

func (s *Simulation) getAccount(*http.Request) (int, any, error) {
	return http.StatusOK, map[string]any{"account": map[string]any{
		"name": "stagely-sim", "email": "sim@stagely.local",
		"acls":    []string{"manage_users", "subscriptions_view", "subscriptions", "billing", "provisioning", "firewall"},
		"balance": 0, "pending_charges": 0, "last_payment_date": "", "last_payment_amount": 0,
	}}, nil
}

func (s *Simulation) listRegions(r *http.Request) (int, any, error) {
	out := make([]regionJSON, len(regions))
	for i, reg := range regions {
		out[i] = withRegionDefaults(reg)
	}
	return listResponse(r, "regions", out)
}

// listPlans lists the vc2 plans. The type filter names a plan family, and
// families other than vc2 and all have no plans here.
func (s *Simulation) listPlans(r *http.Request) (int, any, error) {
	out := []planJSON{}
	if t := r.URL.Query().Get("type"); t == "" || t == "all" || t == "vc2" {
		for _, p := range plans {
			out = append(out, withPlanDefaults(p))
		}
	}
	return listResponse(r, "plans", out)
}

func (s *Simulation) listOS(r *http.Request) (int, any, error) {
	return listResponse(r, "os", []osJSON{ubuntuOS})
}
