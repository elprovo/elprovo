package digitalocean

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	registryNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	registryRegions     = []string{"nyc3", "sfo3", "ams3", "sgp1", "fra1"}
	registryTiers       = []tierJSON{
		{Name: "Starter", Slug: "starter", IncludedRepositories: 1, IncludedStorageBytes: 524288000, IncludedBandwidthBytes: 524288000},
		{Name: "Basic", Slug: "basic", IncludedRepositories: 5, IncludedStorageBytes: 5368709120, AllowStorageOverage: true, IncludedBandwidthBytes: 5368709120, MonthlyPriceInCents: 500},
		{Name: "Professional", Slug: "professional", IncludedStorageBytes: 107374182400, AllowStorageOverage: true, IncludedBandwidthBytes: 107374182400, MonthlyPriceInCents: 2000},
	}
)

type tierJSON struct {
	Name                   string `json:"name"`
	Slug                   string `json:"slug"`
	IncludedRepositories   int    `json:"included_repositories"`
	IncludedStorageBytes   int64  `json:"included_storage_bytes"`
	AllowStorageOverage    bool   `json:"allow_storage_overage"`
	IncludedBandwidthBytes int64  `json:"included_bandwidth_bytes"`
	MonthlyPriceInCents    int    `json:"monthly_price_in_cents"`
}

type subscriptionJSON struct {
	Tier      tierJSON `json:"tier"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type registryJSON struct {
	Name                       string           `json:"name"`
	Region                     string           `json:"region"`
	CreatedAt                  string           `json:"created_at"`
	StorageUsageBytes          int64            `json:"storage_usage_bytes"`
	StorageUsageBytesUpdatedAt string           `json:"storage_usage_bytes_updated_at"`
	Subscription               subscriptionJSON `json:"subscription"`
}

func tier(slug string) (tierJSON, bool) {
	i := slices.IndexFunc(registryTiers, func(t tierJSON) bool { return t.Slug == slug })
	if i < 0 {
		return tierJSON{}, false
	}
	return registryTiers[i], true
}

func renderSubscription(reg *registry) subscriptionJSON {
	t, _ := tier(reg.Tier)
	return subscriptionJSON{Tier: t, CreatedAt: timestamp(reg.CreatedAt), UpdatedAt: timestamp(reg.UpdatedAt)}
}

func renderRegistry(reg *registry) registryJSON {
	return registryJSON{
		Name: reg.Name, Region: reg.Region, CreatedAt: timestamp(reg.CreatedAt),
		StorageUsageBytesUpdatedAt: timestamp(reg.UpdatedAt), Subscription: renderSubscription(reg),
	}
}

func (s *Simulation) addRegistryRoutes(routes map[string]endpoint) {
	for pattern, h := range map[string]endpoint{
		"GET /v2/registry":                             s.getRegistry,
		"GET /v2/registries":                           s.listRegistries,
		"GET /v2/registries/{name}":                    s.getRegistry,
		"POST /v2/registry":                            s.createRegistry,
		"POST /v2/registries":                          s.createRegistry,
		"DELETE /v2/registry":                          s.deleteRegistry,
		"DELETE /v2/registries/{name}":                 s.deleteRegistry,
		"GET /v2/registry/subscription":                s.getSubscription,
		"GET /v2/registries/subscription":              s.getSubscription,
		"POST /v2/registry/subscription":               s.updateSubscription,
		"POST /v2/registries/subscription":             s.updateSubscription,
		"GET /v2/registry/options":                     s.registryOptions,
		"GET /v2/registries/options":                   s.registryOptions,
		"GET /v2/registry/docker-credentials":          s.dockerCredentials,
		"GET /v2/registries/{name}/docker-credentials": s.dockerCredentials,
		"GET /v2/registry/{name}/repositoriesV2":       s.listRepositories,
		"GET /v2/registries/{name}/repositoriesV2":     s.listRepositories,
		"GET /v2/registry/{name}/repositories":         s.listRepositories,
	} {
		routes[pattern] = h
	}
}

func currentRegistry(a *account, r *http.Request) *registry {
	if a.Registry == nil {
		return nil
	}
	if name := r.PathValue("name"); name != "" && name != a.Registry.Name {
		return nil
	}
	copied := *a.Registry
	return &copied
}

func (s *Simulation) withRegistry(r *http.Request) (*registry, error) {
	var reg *registry
	s.store.Read(func(a *account) { reg = currentRegistry(a, r) })
	if reg == nil {
		return nil, errNotFound()
	}
	return reg, nil
}

func (s *Simulation) getRegistry(r *http.Request) (int, any, error) {
	reg, err := s.withRegistry(r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"registry": renderRegistry(reg)}, nil
}

func (s *Simulation) listRegistries(r *http.Request) (int, any, error) {
	out := []registryJSON{}
	s.store.Read(func(a *account) {
		if a.Registry != nil {
			out = append(out, renderRegistry(a.Registry))
		}
	})
	return http.StatusOK, map[string]any{"registries": out}, nil
}

func (s *Simulation) createRegistry(r *http.Request) (int, any, error) {
	var req struct {
		Name                 string `json:"name"`
		SubscriptionTierSlug string `json:"subscription_tier_slug"`
		Region               string `json:"region"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if !registryNamePattern.MatchString(req.Name) {
		return 0, nil, errUnprocessable("name must be 1 to 63 lowercase letters, digits or '-'")
	}
	if _, ok := tier(req.SubscriptionTierSlug); !ok {
		return 0, nil, errUnprocessable("subscription_tier_slug must be starter, basic or professional")
	}
	if req.Region == "" {
		req.Region = registryRegions[0]
	}
	if !slices.Contains(registryRegions, req.Region) {
		return 0, nil, errUnprocessable("region must be one of %s", strings.Join(registryRegions, ", "))
	}
	var out registryJSON
	err := s.store.Update(func(a *account) error {
		if a.Registry != nil {
			return errConflict("the account already has a container registry")
		}
		now := s.now().UTC()
		a.Registry = &registry{Name: req.Name, Region: req.Region, Tier: req.SubscriptionTierSlug, CreatedAt: now, UpdatedAt: now}
		out = renderRegistry(a.Registry)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"registry": out}, nil
}

func (s *Simulation) deleteRegistry(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		if currentRegistry(a, r) == nil {
			return errNotFound()
		}
		a.Registry = nil
		return nil
	})
	return http.StatusNoContent, nil, err
}

func (s *Simulation) getSubscription(r *http.Request) (int, any, error) {
	reg, err := s.withRegistry(r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"subscription": renderSubscription(reg)}, nil
}

func (s *Simulation) updateSubscription(r *http.Request) (int, any, error) {
	var req struct {
		TierSlug string `json:"tier_slug"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if _, ok := tier(req.TierSlug); !ok {
		return 0, nil, errUnprocessable("tier_slug must be starter, basic or professional")
	}
	var out subscriptionJSON
	err := s.store.Update(func(a *account) error {
		if a.Registry == nil {
			return errNotFound()
		}
		a.Registry.Tier, a.Registry.UpdatedAt = req.TierSlug, s.now().UTC()
		out = renderSubscription(a.Registry)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"subscription": out}, nil
}

func (s *Simulation) registryOptions(*http.Request) (int, any, error) {
	type option struct {
		tierJSON
		Eligible bool `json:"eligible"`
	}
	tiers := make([]option, len(registryTiers))
	for i, t := range registryTiers {
		tiers[i] = option{t, true}
	}
	return http.StatusOK, map[string]any{"options": map[string]any{"available_regions": registryRegions, "subscription_tiers": tiers}}, nil
}

func (s *Simulation) dockerCredentials(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	if v := q.Get("read_write"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return 0, nil, errUnprocessable("read_write must be true or false")
		}
	}
	if v := q.Get("expiry_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return 0, nil, errUnprocessable("expiry_seconds must be a non-negative integer")
		}
	}
	if _, err := s.withRegistry(r); err != nil {
		return 0, nil, err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(s.cfg.registry.username + ":" + s.cfg.registry.password))
	return http.StatusOK, map[string]any{"auths": map[string]any{s.cfg.registry.endpoint: map[string]string{"auth": auth}}}, nil
}

type repositoryJSON struct {
	RegistryName  string `json:"registry_name"`
	Name          string `json:"name"`
	TagCount      int    `json:"tag_count"`
	ManifestCount int    `json:"manifest_count"`
}

func (s *Simulation) listRepositories(r *http.Request) (int, any, error) {
	reg, err := s.withRegistry(r)
	if err != nil {
		return 0, nil, err
	}
	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if err := s.registryGet(r, "/v2/_catalog?n=10000", &catalog, nil); err != nil {
		return 0, nil, err
	}
	var repos []repositoryJSON
	for _, full := range catalog.Repositories {
		name, ok := strings.CutPrefix(full, reg.Name+"/")
		if !ok {
			continue
		}
		var tags struct {
			Tags []string `json:"tags"`
		}
		if err := s.registryGet(r, "/v2/"+full+"/tags/list", &tags, nil); err != nil {
			return 0, nil, err
		}
		digests := map[string]bool{}
		for _, t := range tags.Tags {
			var digest string
			if err := s.registryGet(r, "/v2/"+full+"/manifests/"+t, nil, &digest); err != nil {
				return 0, nil, err
			}
			digests[digest] = true
		}
		repos = append(repos, repositoryJSON{RegistryName: reg.Name, Name: name, TagCount: len(tags.Tags), ManifestCount: len(digests)})
	}
	return listResponse(r, "repositories", repos)
}

func (s *Simulation) registryGet(r *http.Request, path string, out any, digest *string) error {
	method := http.MethodGet
	if digest != nil {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.cfg.registry.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.cfg.registry.username, s.cfg.registry.password)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, "+
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := s.registryHTTP.Do(req)
	if err != nil {
		return &apiError{http.StatusInternalServerError, "server_error", fmt.Sprintf("the container registry at %s did not answer: %v", s.cfg.registry.apiURL, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &apiError{http.StatusInternalServerError, "server_error", fmt.Sprintf("the container registry answered %s to %s", resp.Status, path)}
	}
	if digest != nil {
		*digest = resp.Header.Get("Docker-Content-Digest")
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
