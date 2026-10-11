package gcp

import (
	"net/http"
	"slices"
	"strconv"
	"time"
)

// catalogCreated is when the simulated zones, machine types and public
// images came to be.
var catalogCreated = time.Date(2024, 4, 25, 0, 0, 0, 0, time.UTC)

type machineType struct {
	name     string
	cpus     int
	memoryMB int
	shared   bool
}

var machineTypes = []machineType{
	{"e2-micro", 2, 1024, true},
	{"e2-small", 2, 2048, true},
	{"e2-medium", 2, 4096, true},
	{"e2-standard-2", 2, 8192, false},
	{"e2-standard-4", 4, 16384, false},
	{"e2-standard-8", 8, 32768, false},
	{"n2-standard-2", 2, 8192, false},
	{"n2-standard-4", 4, 16384, false},
	{"n2-standard-8", 8, 32768, false},
}

var diskTypes = []string{"pd-standard", "pd-balanced", "pd-ssd"}

// publicImage is an image of a public image project. Every public image
// boots the machine image built from sim/vm.
type publicImage struct {
	project      string
	name         string
	family       string
	architecture string
	id           uint64
}

var publicImages = []publicImage{
	{"ubuntu-os-cloud", "ubuntu-2404-noble-amd64-v20260910", "ubuntu-2404-lts-amd64", "X86_64", 3162812478901234567},
	{"ubuntu-os-cloud", "ubuntu-2404-noble-arm64-v20260910", "ubuntu-2404-lts-arm64", "ARM64", 3162812478901234568},
}

const (
	publicImageSizeGB = 10
	defaultDiskType   = "pd-standard"
)

type catalog struct {
	zones   []string
	regions []string
}

func newCatalog(zones []string) catalog {
	c := catalog{zones: zones}
	for _, z := range zones {
		if r := regionOf(z); !slices.Contains(c.regions, r) {
			c.regions = append(c.regions, r)
		}
	}
	return c
}

func (c catalog) hasZone(zone string) bool     { return slices.Contains(c.zones, zone) }
func (c catalog) hasRegion(region string) bool { return slices.Contains(c.regions, region) }

func machineTypeNamed(name string) (machineType, bool) {
	i := slices.IndexFunc(machineTypes, func(m machineType) bool { return m.name == name })
	if i < 0 {
		return machineType{}, false
	}
	return machineTypes[i], true
}

func catalogID(kind, name string) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(kind + "/" + name) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h%9e18 + 1e17
}

type zoneJSON struct {
	Kind                  string   `json:"kind"`
	ID                    string   `json:"id"`
	CreationTimestamp     string   `json:"creationTimestamp"`
	Name                  string   `json:"name"`
	Description           string   `json:"description"`
	Status                string   `json:"status"`
	Region                string   `json:"region"`
	SelfLink              string   `json:"selfLink"`
	AvailableCPUPlatforms []string `json:"availableCpuPlatforms"`
	SupportsPzs           bool     `json:"supportsPzs"`
}

func (s *Simulation) renderZone(zone string) zoneJSON {
	return zoneJSON{
		Kind: "compute#zone", ID: idString(catalogID("zone", zone)), CreationTimestamp: timestamp(catalogCreated),
		Name: zone, Description: zone, Status: "UP", Region: s.link("regions", regionOf(zone)), SelfLink: s.link("zones", zone),
		AvailableCPUPlatforms: []string{"Intel Cascade Lake", "AMD Milan"},
	}
}

type regionJSON struct {
	Kind              string   `json:"kind"`
	ID                string   `json:"id"`
	CreationTimestamp string   `json:"creationTimestamp"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Status            string   `json:"status"`
	Zones             []string `json:"zones"`
	Quotas            []any    `json:"quotas"`
	SelfLink          string   `json:"selfLink"`
	SupportsPzs       bool     `json:"supportsPzs"`
}

func (s *Simulation) renderRegion(region string) regionJSON {
	out := regionJSON{
		Kind: "compute#region", ID: idString(catalogID("region", region)), CreationTimestamp: timestamp(catalogCreated),
		Name: region, Description: region, Status: "UP", Zones: []string{}, Quotas: []any{}, SelfLink: s.link("regions", region),
	}
	for _, z := range s.catalog.zones {
		if regionOf(z) == region {
			out.Zones = append(out.Zones, s.link("zones", z))
		}
	}
	return out
}

func (s *Simulation) listZones(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []zoneJSON
	for _, z := range s.catalog.zones {
		out = append(out, s.renderZone(z))
	}
	return page(r, "compute#zoneList", s.link("zones"), out)
}

func (s *Simulation) getZone(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s.renderZone(zone), nil
}

func (s *Simulation) listRegions(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []regionJSON
	for _, region := range s.catalog.regions {
		out = append(out, s.renderRegion(region))
	}
	return page(r, "compute#regionList", s.link("regions"), out)
}

func (s *Simulation) getRegion(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s.renderRegion(region), nil
}

// zone returns the zone in the request path, checking the project too.
func (s *Simulation) zone(r *http.Request) (string, error) {
	if err := s.checkProject(r); err != nil {
		return "", err
	}
	zone := r.PathValue("zone")
	if !s.catalog.hasZone(zone) {
		return "", errNotFound("projects/" + s.cfg.project + "/zones/" + zone)
	}
	return zone, nil
}

// region returns the region in the request path, checking the project too.
func (s *Simulation) region(r *http.Request) (string, error) {
	if err := s.checkProject(r); err != nil {
		return "", err
	}
	region := r.PathValue("region")
	if !s.catalog.hasRegion(region) {
		return "", errNotFound("projects/" + s.cfg.project + "/regions/" + region)
	}
	return region, nil
}

type machineTypeJSON struct {
	Kind                         string `json:"kind"`
	ID                           string `json:"id"`
	CreationTimestamp            string `json:"creationTimestamp"`
	Name                         string `json:"name"`
	Description                  string `json:"description"`
	GuestCPUs                    int    `json:"guestCpus"`
	MemoryMB                     int    `json:"memoryMb"`
	ImageSpaceGB                 int    `json:"imageSpaceGb"`
	MaximumPersistentDisks       int    `json:"maximumPersistentDisks"`
	MaximumPersistentDisksSizeGB string `json:"maximumPersistentDisksSizeGb"`
	Zone                         string `json:"zone"`
	SelfLink                     string `json:"selfLink"`
	IsSharedCPU                  bool   `json:"isSharedCpu"`
	Architecture                 string `json:"architecture,omitempty"`
}

func (s *Simulation) renderMachineType(zone string, m machineType) machineTypeJSON {
	return machineTypeJSON{
		Kind: "compute#machineType", ID: idString(catalogID("machineType", m.name)), CreationTimestamp: timestamp(catalogCreated),
		Name: m.name, Description: strconv.Itoa(m.cpus) + " vCPUs, " + strconv.Itoa(m.memoryMB/1024) + " GB RAM",
		GuestCPUs: m.cpus, MemoryMB: m.memoryMB, MaximumPersistentDisks: 128, MaximumPersistentDisksSizeGB: "263168",
		Zone: zone, SelfLink: s.link("zones", zone, "machineTypes", m.name), IsSharedCPU: m.shared,
	}
}

func (s *Simulation) listMachineTypes(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []machineTypeJSON
	for _, m := range machineTypes {
		out = append(out, s.renderMachineType(zone, m))
	}
	return page(r, "compute#machineTypeList", s.link("zones", zone, "machineTypes"), out)
}

func (s *Simulation) getMachineType(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	m, ok := machineTypeNamed(r.PathValue("name"))
	if !ok {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/machineTypes/" + r.PathValue("name"))
	}
	return http.StatusOK, s.renderMachineType(zone, m), nil
}

type diskTypeJSON struct {
	Kind              string `json:"kind"`
	ID                string `json:"id"`
	CreationTimestamp string `json:"creationTimestamp"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	ValidDiskSize     string `json:"validDiskSize"`
	Zone              string `json:"zone"`
	SelfLink          string `json:"selfLink"`
	DefaultDiskSizeGB string `json:"defaultDiskSizeGb"`
}

func (s *Simulation) renderDiskType(zone, name string) diskTypeJSON {
	return diskTypeJSON{
		Kind: "compute#diskType", ID: idString(catalogID("diskType", name)), CreationTimestamp: timestamp(catalogCreated),
		Name: name, Description: name, ValidDiskSize: "10GB-65536GB", Zone: s.link("zones", zone),
		SelfLink: s.link("zones", zone, "diskTypes", name), DefaultDiskSizeGB: "100",
	}
}

func (s *Simulation) listDiskTypes(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []diskTypeJSON
	for _, name := range diskTypes {
		out = append(out, s.renderDiskType(zone, name))
	}
	return page(r, "compute#diskTypeList", s.link("zones", zone, "diskTypes"), out)
}

func (s *Simulation) getDiskType(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	if !slices.Contains(diskTypes, name) {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/diskTypes/" + name)
	}
	return http.StatusOK, s.renderDiskType(zone, name), nil
}

type metadataJSON struct {
	Kind        string         `json:"kind"`
	Fingerprint string         `json:"fingerprint"`
	Items       []metadataItem `json:"items,omitempty"`
}

type metadataItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Simulation) getProject(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var number int64
	s.store.Read(func(w *world) { number = w.ProjectNumber })
	return http.StatusOK, map[string]any{
		"kind": "compute#project", "id": strconv.FormatInt(number, 10), "creationTimestamp": timestamp(catalogCreated),
		"name": s.cfg.project, "commonInstanceMetadata": metadataJSON{Kind: "compute#metadata", Fingerprint: fingerprint("")},
		"quotas": []any{}, "selfLink": computeBase + "projects/" + s.cfg.project,
		"defaultServiceAccount": strconv.FormatInt(number, 10) + "-compute@developer.gserviceaccount.com",
		"xpnProjectStatus":      "UNSPECIFIED_XPN_PROJECT_STATUS", "defaultNetworkTier": "PREMIUM",
		"vmDnsSetting": "ZONAL_ONLY",
	}, nil
}
