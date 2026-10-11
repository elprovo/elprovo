package hetzner

import (
	"net/http"
	"slices"
	"strconv"
	"time"
)

type locationJSON struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Country     string  `json:"country"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	NetworkZone string  `json:"network_zone"`
}

// locations are Hetzner's six locations, with the ids, names, descriptions
// and network zones the API reports. The coordinates are those of each
// location's city.
var locations = []locationJSON{
	{1, "fsn1", "Falkenstein DC Park 1", "DE", "Falkenstein", 50.47612, 12.370071, "eu-central"},
	{2, "nbg1", "Nuremberg DC Park 1", "DE", "Nuremberg", 49.452102, 11.076665, "eu-central"},
	{3, "hel1", "Helsinki DC Park 1", "FI", "Helsinki", 60.169855, 24.938379, "eu-central"},
	{4, "ash", "Ashburn, VA", "US", "Ashburn, VA", 39.045821, -77.487073, "us-east"},
	{5, "hil", "Hillsboro, OR", "US", "Hillsboro, OR", 45.54222, -122.951924, "us-west"},
	{6, "sin", "Singapore", "SG", "Singapore", 1.283333, 103.833333, "ap-southeast"},
}

// availabilityZones name the datacenter that the metadata service reports
// for each location.
var availabilityZones = map[string]string{
	"fsn1": "fsn1-dc14", "nbg1": "nbg1-dc3", "hel1": "hel1-dc2", "ash": "ash-dc1", "hil": "hil-dc1", "sin": "sin-dc1",
}

func location(idOrName string) (locationJSON, bool) {
	for _, l := range locations {
		if l.Name == idOrName || strconv.FormatInt(l.ID, 10) == idOrName {
			return l, true
		}
	}
	return locationJSON{}, false
}

type serverType struct {
	id           int64
	name         string
	description  string
	category     string
	cores        int
	memory       float64
	disk         int
	architecture string
	locations    []string
}

var euLocations = []string{"fsn1", "nbg1", "hel1"}

var allLocations = []string{"fsn1", "nbg1", "hel1", "ash", "hil", "sin"}

// serverTypes are Hetzner's shared-vCPU server types with the cores, memory
// (GB) and disk (GB) that hetzner.com lists. The ids are the simulation's.
var serverTypes = []serverType{
	{1, "cx23", "CX23", "cost_optimized", 2, 4, 40, "x86", euLocations},
	{2, "cx33", "CX33", "cost_optimized", 4, 8, 80, "x86", euLocations},
	{3, "cx43", "CX43", "cost_optimized", 8, 16, 160, "x86", euLocations},
	{4, "cx53", "CX53", "cost_optimized", 16, 32, 320, "x86", euLocations},
	{5, "cax11", "CAX11", "cost_optimized", 2, 4, 40, "arm", euLocations},
	{6, "cax21", "CAX21", "cost_optimized", 4, 8, 80, "arm", euLocations},
	{7, "cax31", "CAX31", "cost_optimized", 8, 16, 160, "arm", euLocations},
	{8, "cax41", "CAX41", "cost_optimized", 16, 32, 320, "arm", euLocations},
	{9, "cpx12", "CPX12", "regular_purpose", 1, 2, 40, "x86", allLocations},
	{10, "cpx22", "CPX22", "regular_purpose", 2, 4, 80, "x86", allLocations},
	{11, "cpx32", "CPX32", "regular_purpose", 4, 8, 160, "x86", allLocations},
	{12, "cpx42", "CPX42", "regular_purpose", 8, 16, 320, "x86", allLocations},
	{13, "cpx52", "CPX52", "regular_purpose", 12, 24, 480, "x86", allLocations},
	{14, "cpx62", "CPX62", "regular_purpose", 16, 32, 640, "x86", allLocations},
}

func findServerType(idOrName string) (serverType, bool) {
	for _, t := range serverTypes {
		if t.name == idOrName || strconv.FormatInt(t.id, 10) == idOrName {
			return t, true
		}
	}
	return serverType{}, false
}

type serverTypeLocationJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Deprecation *struct{} `json:"deprecation"`
	Recommended bool      `json:"recommended"`
	Available   bool      `json:"available"`
}

type serverTypeJSON struct {
	ID           int64                    `json:"id"`
	Name         string                   `json:"name"`
	Description  string                   `json:"description"`
	Cores        int                      `json:"cores"`
	Memory       float64                  `json:"memory"`
	Disk         int                      `json:"disk"`
	Deprecated   bool                     `json:"deprecated"`
	Prices       []struct{}               `json:"prices"`
	StorageType  string                   `json:"storage_type"`
	CPUType      string                   `json:"cpu_type"`
	Category     string                   `json:"category"`
	Architecture string                   `json:"architecture"`
	Deprecation  *struct{}                `json:"deprecation"`
	Locations    []serverTypeLocationJSON `json:"locations"`
}

func (t serverType) render() serverTypeJSON {
	out := serverTypeJSON{
		ID: t.id, Name: t.name, Description: t.description, Cores: t.cores, Memory: t.memory, Disk: t.disk,
		Prices: []struct{}{}, StorageType: "local", CPUType: "shared", Category: t.category, Architecture: t.architecture,
		Locations: []serverTypeLocationJSON{},
	}
	for _, name := range t.locations {
		l, _ := location(name)
		out.Locations = append(out.Locations, serverTypeLocationJSON{ID: l.ID, Name: l.Name, Recommended: true, Available: true})
	}
	return out
}

func (t serverType) availableIn(locationName string) bool {
	return slices.Contains(t.locations, locationName)
}

// systemImage is a Hetzner system image. Hetzner keeps one image per
// architecture under the same name; the ids are the simulation's.
type systemImage struct {
	id           int64
	name         string
	description  string
	osVersion    string
	architecture string
}

var systemImages = []systemImage{
	{100000001, "ubuntu-24.04", "Ubuntu 24.04", "24.04", "x86"},
	{100000002, "ubuntu-24.04", "Ubuntu 24.04", "24.04", "arm"},
}

var systemImagesCreated = time.Date(2024, time.April, 25, 0, 0, 0, 0, time.UTC)

func (s *Simulation) listLocations(r *http.Request) (int, any, error) {
	name := r.URL.Query().Get("name")
	var out []locationJSON
	for _, l := range locations {
		if name == "" || l.Name == name {
			out = append(out, l)
		}
	}
	return listResponse(r, "locations", out, sortFields[locationJSON]{
		"id":   byID(func(l locationJSON) int64 { return l.ID }),
		"name": byString(func(l locationJSON) string { return l.Name }),
	})
}

func (s *Simulation) getLocation(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "location")
	if err != nil {
		return 0, nil, err
	}
	l, ok := location(strconv.FormatInt(id, 10))
	if !ok {
		return 0, nil, errNotFound("location")
	}
	return http.StatusOK, map[string]any{"location": l}, nil
}

func (s *Simulation) listServerTypes(r *http.Request) (int, any, error) {
	name := r.URL.Query().Get("name")
	var out []serverTypeJSON
	for _, t := range serverTypes {
		if name == "" || t.name == name {
			out = append(out, t.render())
		}
	}
	return listResponse(r, "server_types", out, nil)
}

func (s *Simulation) getServerType(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "server type")
	if err != nil {
		return 0, nil, err
	}
	t, ok := findServerType(strconv.FormatInt(id, 10))
	if !ok {
		return 0, nil, errNotFound("server type")
	}
	return http.StatusOK, map[string]any{"server_type": t.render()}, nil
}
