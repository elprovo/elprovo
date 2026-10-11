package vultr

import (
	"net/http"
	"slices"
	"testing"
)

func TestRequestWithoutAnAcceptedKeyIsUnauthorized(t *testing.T) {
	e := newEnv(t)
	var body errorBody

	status, _ := e.callWithKey(http.MethodGet, "/v2/account", "WRONG", nil, &body)

	if status != http.StatusUnauthorized || body != (errorBody{"Invalid API token.", http.StatusUnauthorized}) {
		t.Fatalf("response = %d %+v", status, body)
	}
}

func TestUnknownPathIsNotFoundEvenWithoutAKey(t *testing.T) {
	e := newEnv(t)
	var body errorBody

	status, _ := e.callWithKey(http.MethodGet, "/v2/load-balancers", "", nil, &body)

	if status != http.StatusNotFound || body != (errorBody{"Not found.", http.StatusNotFound}) {
		t.Fatalf("response = %d %+v", status, body)
	}
}

func TestWrongMethodNamesTheAllowedOnes(t *testing.T) {
	e := newEnv(t)
	var body errorBody

	status, header := e.callWithKey(http.MethodPut, "/v2/instances", "", nil, &body)

	if status != http.StatusMethodNotAllowed || body.Error != "Method not allowed. Must be one of: GET, POST" || header.Get("Allow") != "GET, POST" {
		t.Fatalf("response = %d %+v, Allow %q", status, body, header.Get("Allow"))
	}
}

func TestMalformedBodyIsABadRequest(t *testing.T) {
	e := newEnv(t)
	var body errorBody

	if status := e.call(http.MethodPost, "/v2/ssh-keys", "not an object", &body); status != http.StatusBadRequest || body.Status != http.StatusBadRequest {
		t.Fatalf("response = %d %+v", status, body)
	}
}

func TestBurstOverTheRateLimitIsRejectedWithRetryAfter(t *testing.T) {
	e := newEnv(t, func(c *config) { c.rateLimit = 2 })
	e.call(http.MethodGet, "/v2/account", nil, nil)
	e.call(http.MethodGet, "/v2/account", nil, nil)
	var body errorBody

	status, header := e.callWithKey(http.MethodGet, "/v2/account", testAPIKey, nil, &body)

	if status != http.StatusTooManyRequests || body.Status != http.StatusTooManyRequests || header.Get("Retry-After") != "1" {
		t.Fatalf("third request in a second = %d %+v, Retry-After %q", status, body, header.Get("Retry-After"))
	}
}

func TestListsArePagedWithCursors(t *testing.T) {
	e := newEnv(t)
	type page struct {
		Regions []regionJSON `json:"regions"`
		Meta    meta         `json:"meta"`
	}
	var first, second, back page

	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=20", nil, &first)
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=20&cursor="+first.Meta.Links.Next, nil, &second)
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=20&cursor="+second.Meta.Links.Prev, nil, &back)

	if len(first.Regions) != 20 || first.Meta.Total != len(regions) || first.Meta.Links.Next == "" || first.Meta.Links.Prev != "" {
		t.Fatalf("first page = %d regions, meta %+v", len(first.Regions), first.Meta)
	}
	if len(second.Regions) != len(regions)-20 || second.Regions[0].ID != regions[20].ID || second.Meta.Links.Next != "" || second.Meta.Links.Prev == "" {
		t.Fatalf("second page = %d regions from %s, meta %+v", len(second.Regions), second.Regions[0].ID, second.Meta)
	}
	if back.Regions[0].ID != first.Regions[0].ID {
		t.Fatalf("prev cursor led to %s, want the first page", back.Regions[0].ID)
	}
}

func TestInvalidPageSizeOrCursorMeansTheDefaults(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Regions []regionJSON `json:"regions"`
	}

	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=abc&cursor=garbage", nil, &got)

	if len(got.Regions) != len(regions) || got.Regions[0].ID != "ams" {
		t.Fatalf("regions = %d from %s, want all from the first", len(got.Regions), got.Regions[0].ID)
	}
}

func TestCatalogDescribesTheSimulatedCloud(t *testing.T) {
	e := newEnv(t)
	var plans struct {
		Plans []planJSON `json:"plans"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/plans?type=vc2", nil, &plans)
	i := slices.IndexFunc(plans.Plans, func(p planJSON) bool { return p.ID == "vc2-2c-4gb" })
	if i < 0 || plans.Plans[i].RAM != 4096 || plans.Plans[i].VCPUCount != 2 || !slices.Contains(plans.Plans[i].Locations, "fra") {
		t.Fatalf("plans = %+v", plans.Plans)
	}
	var gpus struct {
		Plans []planJSON `json:"plans"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/plans?type=vcg", nil, &gpus)
	if len(gpus.Plans) != 0 {
		t.Fatalf("GPU plans = %+v, want none", gpus.Plans)
	}
	var os struct {
		OS []osJSON `json:"os"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/os", nil, &os)
	if len(os.OS) != 1 || os.OS[0] != (osJSON{2284, "Ubuntu 24.04 LTS x64", "x64", "ubuntu"}) {
		t.Fatalf("os = %+v", os.OS)
	}
}
