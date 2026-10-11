package digitalocean

import (
	"net/http"
	"strconv"
	"testing"
)

func TestRequestWithoutAnAcceptedTokenIsUnauthorized(t *testing.T) {
	e := newEnv(t)
	var body errorBody
	status, header := e.callWithHeaders(http.MethodGet, "/v2/account", "dop_v1_wrong", nil, &body)

	if status != http.StatusUnauthorized || body.ID != "unauthorized" || body.Message != "Unable to authenticate you." {
		t.Fatalf("response = %d %+v", status, body)
	}
	if body.RequestID == "" || header.Get("X-Request-Id") != body.RequestID {
		t.Fatalf("request id %q in body, %q in header; want the same non-empty id", body.RequestID, header.Get("X-Request-Id"))
	}
}

func TestResponsesCarryRateLimitHeaders(t *testing.T) {
	e := newEnv(t)
	e.call(http.MethodGet, "/v2/regions", nil, nil)
	_, header := e.callWithHeaders(http.MethodGet, "/v2/regions", testToken, nil, nil)

	if header.Get("Ratelimit-Limit") != "5000" || header.Get("Ratelimit-Remaining") != "4998" {
		t.Fatalf("headers = limit %q remaining %q, want 5000 and 4998", header.Get("Ratelimit-Limit"), header.Get("Ratelimit-Remaining"))
	}
	if reset, err := strconv.ParseInt(header.Get("Ratelimit-Reset"), 10, 64); err != nil || reset <= 0 {
		t.Fatalf("Ratelimit-Reset = %q, want a Unix time", header.Get("Ratelimit-Reset"))
	}
}

func TestBurstOverTheMinuteLimitIsRejectedWithRetryAfter(t *testing.T) {
	e := newEnv(t, func(c *config) { c.limits.perMinute = 2 })
	e.call(http.MethodGet, "/v2/account", nil, nil)
	e.call(http.MethodGet, "/v2/account", nil, nil)

	var body errorBody
	status, header := e.callWithHeaders(http.MethodGet, "/v2/account", testToken, nil, &body)
	if status != http.StatusTooManyRequests || body.ID != "too_many_requests" {
		t.Fatalf("third request = %d %+v, want 429 too_many_requests", status, body)
	}
	if header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

func TestUnknownPathIsNotFoundInDigitalOceansShape(t *testing.T) {
	e := newEnv(t)
	var body errorBody
	if status := e.call(http.MethodGet, "/v2/load_balancers", nil, &body); status != http.StatusNotFound || body.ID != "not_found" {
		t.Fatalf("response = %d %+v", status, body)
	}
}

func TestMalformedBodyIsABadRequest(t *testing.T) {
	e := newEnv(t)
	var body errorBody
	if status := e.call(http.MethodPost, "/v2/tags", "not an object", &body); status != http.StatusBadRequest || body.ID != "bad_request" {
		t.Fatalf("response = %d %+v", status, body)
	}
}

func TestListsArePagedWithLinksAndTotal(t *testing.T) {
	e := newEnv(t, func(c *config) { c.regions = []string{"nyc1", "nyc3", "fra1"} })
	var first, second struct {
		Regions []regionJSON `json:"regions"`
		Links   links        `json:"links"`
		Meta    meta         `json:"meta"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=2", nil, &first)
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/regions?per_page=2&page=2", nil, &second)

	if len(first.Regions) != 2 || first.Meta.Total != 3 || first.Links.Pages == nil {
		t.Fatalf("first page = %+v", first)
	}
	if first.Links.Pages.Next != e.server.URL+"/v2/regions?page=2&per_page=2" || first.Links.Pages.Prev != "" {
		t.Fatalf("first page links = %+v", first.Links.Pages)
	}
	if len(second.Regions) != 1 || second.Regions[0].Slug != "fra1" || second.Links.Pages.Next != "" || second.Links.Pages.Prev == "" {
		t.Fatalf("second page = %+v", second)
	}
}

func TestAccountRegionsSizesAndImagesDescribeTheSimulatedCloud(t *testing.T) {
	e := newEnv(t)
	var account struct {
		Account struct {
			UUID         string `json:"uuid"`
			DropletLimit int    `json:"droplet_limit"`
			Status       string `json:"status"`
		} `json:"account"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/account", nil, &account)
	if account.Account.UUID == "" || account.Account.DropletLimit != dropletLimit || account.Account.Status != "active" {
		t.Fatalf("account = %+v", account)
	}
	var sizes struct {
		Sizes []sizeJSON `json:"sizes"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/sizes?per_page=200", nil, &sizes)
	var found bool
	for _, s := range sizes.Sizes {
		found = found || (s.Slug == "s-2vcpu-4gb" && s.Memory == 4096 && s.VCPUs == 2 && s.PriceMonthly == 24)
	}
	if !found {
		t.Fatalf("sizes lack s-2vcpu-4gb: %+v", sizes.Sizes)
	}
	var image struct {
		Image imageJSON `json:"image"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/images/ubuntu-24-04-x64", nil, &image)
	if image.Image.Distribution != "Ubuntu" || *image.Image.Slug != "ubuntu-24-04-x64" || image.Image.Status != "available" {
		t.Fatalf("image = %+v", image.Image)
	}
	var distributions struct {
		Images []imageJSON `json:"images"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/images?type=distribution", nil, &distributions)
	if len(distributions.Images) != 1 {
		t.Fatalf("distribution images = %+v, want one", distributions.Images)
	}
}
