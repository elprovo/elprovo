package digitalocean

import (
	"slices"
	"time"
)

var regionNames = map[string]string{
	"ams2": "Amsterdam 2", "ams3": "Amsterdam 3", "atl1": "Atlanta 1", "blr1": "Bangalore 1",
	"fra1": "Frankfurt 1", "lon1": "London 1", "nyc1": "New York 1", "nyc2": "New York 2",
	"nyc3": "New York 3", "sfo1": "San Francisco 1", "sfo2": "San Francisco 2", "sfo3": "San Francisco 3",
	"sgp1": "Singapore 1", "syd1": "Sydney 1", "tor1": "Toronto 1",
}

var regionFeatures = []string{"private_networking", "backups", "ipv6", "metadata", "install_agent", "storage", "image_transfer"}

var sizes = []sizeJSON{
	{Slug: "s-1vcpu-512mb-10gb", Memory: 512, VCPUs: 1, Disk: 10, Transfer: 0.5, PriceMonthly: 4, PriceHourly: 0.00595},
	{Slug: "s-1vcpu-1gb", Memory: 1024, VCPUs: 1, Disk: 25, Transfer: 1, PriceMonthly: 6, PriceHourly: 0.00893},
	{Slug: "s-1vcpu-2gb", Memory: 2048, VCPUs: 1, Disk: 50, Transfer: 2, PriceMonthly: 12, PriceHourly: 0.01786},
	{Slug: "s-2vcpu-2gb", Memory: 2048, VCPUs: 2, Disk: 60, Transfer: 3, PriceMonthly: 18, PriceHourly: 0.02679},
	{Slug: "s-2vcpu-4gb", Memory: 4096, VCPUs: 2, Disk: 80, Transfer: 4, PriceMonthly: 24, PriceHourly: 0.03571},
	{Slug: "s-4vcpu-8gb", Memory: 8192, VCPUs: 4, Disk: 160, Transfer: 5, PriceMonthly: 48, PriceHourly: 0.07143},
	{Slug: "s-8vcpu-16gb", Memory: 16384, VCPUs: 8, Disk: 320, Transfer: 6, PriceMonthly: 96, PriceHourly: 0.14286},
}

var ubuntuImage = imageJSON{
	ID: 166000000, Name: "24.04 (LTS) x64", Type: "base", Distribution: "Ubuntu",
	Public: true, MinDiskSize: 7, SizeGigabytes: 0.6, CreatedAt: "2024-04-25T00:00:00Z",
	Description: "Ubuntu 24.04 (LTS) x64", Tags: []string{}, Status: "available", ErrorMessage: "",
}

type regionJSON struct {
	Name      string   `json:"name"`
	Slug      string   `json:"slug"`
	Features  []string `json:"features"`
	Available bool     `json:"available"`
	Sizes     []string `json:"sizes"`
}

type sizeJSON struct {
	Slug         string     `json:"slug"`
	Memory       int        `json:"memory"`
	VCPUs        int        `json:"vcpus"`
	Disk         int        `json:"disk"`
	Transfer     float64    `json:"transfer"`
	PriceMonthly float64    `json:"price_monthly"`
	PriceHourly  float64    `json:"price_hourly"`
	Regions      []string   `json:"regions"`
	Available    bool       `json:"available"`
	Description  string     `json:"description"`
	DiskInfo     []diskInfo `json:"disk_info"`
}

type diskInfo struct {
	Type string `json:"type"`
	Size struct {
		Amount int    `json:"amount"`
		Unit   string `json:"unit"`
	} `json:"size"`
}

type imageJSON struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Distribution  string   `json:"distribution"`
	Slug          *string  `json:"slug"`
	Public        bool     `json:"public"`
	Regions       []string `json:"regions"`
	MinDiskSize   int      `json:"min_disk_size"`
	SizeGigabytes float64  `json:"size_gigabytes"`
	CreatedAt     string   `json:"created_at"`
	Description   string   `json:"description"`
	Tags          []string `json:"tags"`
	Status        string   `json:"status"`
	ErrorMessage  string   `json:"error_message"`
}

type catalog struct {
	regions []string
}

func (c catalog) hasRegion(slug string) bool { return slices.Contains(c.regions, slug) }

func (c catalog) region(slug string) regionJSON {
	name := regionNames[slug]
	if name == "" {
		name = slug
	}
	return regionJSON{Name: name, Slug: slug, Features: regionFeatures, Available: true, Sizes: sizeSlugs()}
}

func (c catalog) allRegions() []regionJSON {
	out := make([]regionJSON, len(c.regions))
	for i, slug := range c.regions {
		out[i] = c.region(slug)
	}
	return out
}

func (c catalog) size(slug string) (sizeJSON, bool) {
	for _, s := range sizes {
		if s.Slug == slug {
			return c.fill(s), true
		}
	}
	return sizeJSON{}, false
}

func (c catalog) allSizes() []sizeJSON {
	out := make([]sizeJSON, len(sizes))
	for i, s := range sizes {
		out[i] = c.fill(s)
	}
	return out
}

func (c catalog) fill(s sizeJSON) sizeJSON {
	s.Regions, s.Available, s.Description = c.regions, true, "Basic"
	d := diskInfo{Type: "local"}
	d.Size.Amount, d.Size.Unit = s.Disk, "gib"
	s.DiskInfo = []diskInfo{d}
	return s
}

func (c catalog) distributionImage() imageJSON {
	img := ubuntuImage
	slug := "ubuntu-24-04-x64"
	img.Slug, img.Regions = &slug, c.regions
	return img
}

func sizeSlugs() []string {
	out := make([]string, len(sizes))
	for i, s := range sizes {
		out[i] = s.Slug
	}
	return out
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
