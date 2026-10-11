package vultr

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const registryNotFound = "Invalid container registry ID."

var registryNamePattern = regexp.MustCompile(`^[a-z0-9]+$`)

type registryPlan struct {
	VanityName   string `json:"vanity_name"`
	MaxStorageMB int    `json:"max_storage_mb"`
	MonthlyPrice int    `json:"monthly_price"`
}

// registryPlans are the Vultr Container Registry plans that Vultr's docs
// list, by plan key.
var registryPlans = map[string]registryPlan{
	"start_up":   {VanityName: "Start Up", MaxStorageMB: 10240, MonthlyPrice: 0},
	"business":   {VanityName: "Business", MaxStorageMB: 20480, MonthlyPrice: 5},
	"premium":    {VanityName: "Premium", MaxStorageMB: 51200, MonthlyPrice: 10},
	"enterprise": {VanityName: "Enterprise", MaxStorageMB: 1048576, MonthlyPrice: 20},
}

type storageJSON struct {
	Bytes     float64 `json:"bytes"`
	MB        float64 `json:"mb"`
	GB        float64 `json:"gb"`
	TB        float64 `json:"tb"`
	UpdatedAt string  `json:"updated_at"`
}

func storageOf(mb float64, updated string) storageJSON {
	return storageJSON{Bytes: mb * 1024 * 1024, MB: mb, GB: mb / 1024, TB: mb / 1024 / 1024, UpdatedAt: updated}
}

type registryJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URN     string `json:"urn"`
	Storage struct {
		Used    storageJSON `json:"used"`
		Allowed storageJSON `json:"allowed"`
	} `json:"storage"`
	DateCreated string           `json:"date_created"`
	Public      bool             `json:"public"`
	RootUser    registryUserJSON `json:"root_user"`
	Metadata    struct {
		Region       registryRegionJSON `json:"region"`
		Subscription struct {
			Billing struct {
				MonthlyPrice   float64 `json:"monthly_price"`
				PendingCharges float64 `json:"pending_charges"`
			} `json:"billing"`
		} `json:"subscription"`
	} `json:"metadata"`
}

type registryUserJSON struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Root      bool   `json:"root"`
	AddedAt   string `json:"added_at"`
	UpdatedAt string `json:"updated_at"`
}

type registryRegionJSON struct {
	ID         int               `json:"id"`
	Name       string            `json:"name"`
	URN        string            `json:"urn"`
	BaseURL    string            `json:"base_url"`
	Public     bool              `json:"public"`
	AddedAt    string            `json:"added_at"`
	UpdatedAt  string            `json:"updated_at"`
	DataCenter map[string]string `json:"data_center"`
}

// registryHost is the host name of a registry region, such as
// ewr.vultrcr.com, which names the registry's images and Docker logins.
func registryHost(region string) string {
	return region + ".vultrcr.com"
}

func (s *Simulation) renderRegistry(reg *registry) registryJSON {
	var out registryJSON
	created, updated := timestamp(reg.CreatedAt), timestamp(reg.UpdatedAt)
	p := registryPlans[reg.Plan]
	out.ID, out.Name, out.URN, out.DateCreated, out.Public = reg.ID, reg.Name, registryHost(reg.Region)+"/"+reg.Name, created, reg.Public
	out.Storage.Used, out.Storage.Allowed = storageOf(0, updated), storageOf(float64(p.MaxStorageMB), updated)
	out.RootUser = registryUserJSON{ID: 1, Username: s.cfg.registry.username, Password: s.cfg.registry.password, Root: true, AddedAt: created, UpdatedAt: updated}
	loc, _ := region(reg.Region)
	out.Metadata.Region = registryRegionJSON{
		ID: slices.IndexFunc(regions, func(r regionJSON) bool { return r.ID == reg.Region }) + 1, Name: reg.Region,
		URN: registryHost(reg.Region), BaseURL: "https://" + registryHost(reg.Region), Public: true, AddedAt: created, UpdatedAt: created,
		DataCenter: map[string]string{"name": loc.City, "site_code": strings.ToUpper(reg.Region), "country": loc.Country, "continent": loc.Continent},
	}
	out.Metadata.Subscription.Billing.MonthlyPrice = float64(p.MonthlyPrice)
	return out
}

func (s *Simulation) createRegistry(r *http.Request) (int, any, error) {
	var req struct {
		Name   string `json:"name"`
		Public bool   `json:"public"`
		Region string `json:"region"`
		Plan   string `json:"plan"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if _, ok := registryPlans[req.Plan]; !ok {
		return 0, nil, errBadRequest("Invalid plan: use start_up, business, premium or enterprise.")
	}
	if _, ok := region(req.Region); !ok {
		return 0, nil, errBadRequest("Invalid region.")
	}
	if !registryNamePattern.MatchString(req.Name) {
		return 0, nil, errBadRequest("Invalid name: use lowercase letters and digits only.")
	}
	var out registryJSON
	err := s.store.Update(func(a *account) error {
		if slices.ContainsFunc(a.Registries, func(reg *registry) bool { return reg.Name == req.Name }) {
			return errBadRequest("A registry named %s already exists.", req.Name)
		}
		now := s.now()
		reg := &registry{ID: newUUID(), Name: req.Name, Region: req.Region, Plan: req.Plan, Public: req.Public, CreatedAt: now, UpdatedAt: now}
		a.Registries = append(a.Registries, reg)
		out = s.renderRegistry(reg)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, out, nil
}

func (s *Simulation) listRegistries(r *http.Request) (int, any, error) {
	var out []registryJSON
	s.store.Read(func(a *account) {
		for _, reg := range a.Registries {
			out = append(out, s.renderRegistry(reg))
		}
	})
	return listResponse(r, "registries", out)
}

func (s *Simulation) getRegistry(r *http.Request) (int, any, error) {
	var out *registryJSON
	s.store.Read(func(a *account) {
		if reg := a.registry(r.PathValue("id")); reg != nil {
			j := s.renderRegistry(reg)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(registryNotFound)
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) updateRegistry(r *http.Request) (int, any, error) {
	var req struct {
		Public *bool   `json:"public"`
		Plan   *string `json:"plan"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if _, ok := registryPlans[deref(req.Plan)]; req.Plan != nil && !ok {
		return 0, nil, errBadRequest("Invalid plan: use start_up, business, premium or enterprise.")
	}
	var out registryJSON
	err := s.store.Update(func(a *account) error {
		reg := a.registry(r.PathValue("id"))
		if reg == nil {
			return errNotFound(registryNotFound)
		}
		if req.Public != nil {
			reg.Public = *req.Public
		}
		if req.Plan != nil {
			reg.Plan = *req.Plan
		}
		reg.UpdatedAt = s.now()
		out = s.renderRegistry(reg)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Simulation) deleteRegistry(r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	err := s.store.Update(func(a *account) error {
		if a.registry(id) == nil {
			return errNotFound(registryNotFound)
		}
		a.Registries = slices.DeleteFunc(a.Registries, func(reg *registry) bool { return reg.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}

// dockerCredentials hands out the root user's credentials as a Docker config
// for the registry's host. expiry_seconds and read_write are validated but
// change nothing: the operator's registry checks one static user.
func (s *Simulation) dockerCredentials(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	if v := q.Get("expiry_seconds"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
			return 0, nil, &apiError{http.StatusUnprocessableEntity, "expiry_seconds must be a non-negative integer."}
		}
	}
	if v := q.Get("read_write"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return 0, nil, &apiError{http.StatusUnprocessableEntity, "read_write must be true or false."}
		}
	}
	var host string
	s.store.Read(func(a *account) {
		if reg := a.registry(r.PathValue("id")); reg != nil {
			host = registryHost(reg.Region)
		}
	})
	if host == "" {
		return 0, nil, errNotFound(registryNotFound)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(s.cfg.registry.username + ":" + s.cfg.registry.password))
	return http.StatusOK, map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}}, nil
}

func (s *Simulation) listRegistryPlans(*http.Request) (int, any, error) {
	return http.StatusOK, map[string]any{"plans": registryPlans}, nil
}
