package hetzner

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a clock that moves only when a test advances it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRequestWithoutAKnownTokenIsUnauthorized(t *testing.T) {
	e := newEnv(t)

	status, _, data := e.do(http.MethodGet, "/v1/servers", strings.Repeat("x", tokenLength), nil)

	var got errorBody
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if status != http.StatusUnauthorized || got.Error.Code != "unauthorized" || got.Error.Message == "" {
		t.Fatalf("answer = %d %s; want 401 with code unauthorized", status, data)
	}
}

func TestEveryAnswerCarriesTheRateLimit(t *testing.T) {
	e := newEnv(t)

	_, header, _ := e.do(http.MethodGet, "/v1/locations", testToken, nil)

	if header.Get("RateLimit-Limit") != "3600" || header.Get("RateLimit-Remaining") != "3599" {
		t.Fatalf("rate limit headers = %q / %q, want 3600 / 3599", header.Get("RateLimit-Limit"), header.Get("RateLimit-Remaining"))
	}
	if reset, err := strconv.ParseInt(header.Get("RateLimit-Reset"), 10, 64); err != nil || reset < time.Now().Unix() {
		t.Fatalf("RateLimit-Reset = %q, want a UNIX time in the future", header.Get("RateLimit-Reset"))
	}
}

func TestExhaustedBudgetAnswersTooManyRequests(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e := newEnv(t, func(s *Simulation) { s.now, s.limiter = clock.Now, newRateLimiter(2) })

	e.call(http.MethodGet, "/v1/locations", nil, nil)
	e.call(http.MethodGet, "/v1/locations", nil, nil)
	status, code := e.errorCode(http.MethodGet, "/v1/locations", nil)
	if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
		t.Fatalf("third request = %d %s, want 429 rate_limit_exceeded", status, code)
	}

	clock.Advance(time.Second)
	if status := e.call(http.MethodGet, "/v1/locations", nil, nil); status != http.StatusOK {
		t.Fatalf("request after a second = %d, want 200: the budget refills by one request a second", status)
	}
}

func TestListsArePaginatedWithMetaAndLinkHeader(t *testing.T) {
	e := newEnv(t)

	status, header, data := e.do(http.MethodGet, "/v1/server_types?per_page=5&page=2", testToken, nil)

	var got struct {
		ServerTypes []serverTypeJSON `json:"server_types"`
		Meta        struct {
			Pagination paginationJSON `json:"pagination"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &got); err != nil || status != http.StatusOK {
		t.Fatalf("list = %d %s", status, data)
	}
	p := got.Meta.Pagination
	if len(got.ServerTypes) != 5 || p.Page != 2 || p.PerPage != 5 || p.TotalEntries != 14 || p.LastPage != 3 ||
		p.PreviousPage == nil || *p.PreviousPage != 1 || p.NextPage == nil || *p.NextPage != 3 {
		t.Fatalf("page 2 of 14 server types by 5 = %d items, pagination %+v", len(got.ServerTypes), p)
	}
	link := header.Get("Link")
	for _, want := range []string{`page=1&per_page=5>; rel="prev"`, `page=3&per_page=5>; rel="next"`, `rel="last"`} {
		if !strings.Contains(link, want) {
			t.Fatalf("Link = %q, want it to contain %q", link, want)
		}
	}
}

func TestPerPageIsCappedAtFifty(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Meta struct {
			Pagination paginationJSON `json:"pagination"`
		} `json:"meta"`
	}

	e.mustCall(http.StatusOK, http.MethodGet, "/v1/locations?per_page=500", nil, &got)

	if got.Meta.Pagination.PerPage != 50 || got.Meta.Pagination.PreviousPage != nil || got.Meta.Pagination.NextPage != nil {
		t.Fatalf("pagination = %+v, want per_page 50 and no previous or next page", got.Meta.Pagination)
	}
}

func TestListsSortByTheGivenFields(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Locations []locationJSON `json:"locations"`
	}

	e.mustCall(http.StatusOK, http.MethodGet, "/v1/locations?sort=name:desc", nil, &got)

	if len(got.Locations) != 6 || got.Locations[0].Name != "sin" || got.Locations[5].Name != "ash" {
		t.Fatalf("locations by name descending = %v", got.Locations)
	}
	if status, code := e.errorCode(http.MethodGet, "/v1/locations?sort=city", nil); status != http.StatusUnprocessableEntity || code != "invalid_input" {
		t.Fatalf("sort by an unknown field = %d %s, want 422 invalid_input", status, code)
	}
}

func TestUnknownPathsAndMethodsAnswerAsTheAPIDoes(t *testing.T) {
	e := newEnv(t)

	if status, code := e.errorCode(http.MethodGet, "/v1/load_balancers", nil); status != http.StatusNotFound || code != "not_found" {
		t.Fatalf("unknown path = %d %s, want 404 not_found", status, code)
	}
	if status, code := e.errorCode(http.MethodDelete, "/v1/locations", nil); status != http.StatusMethodNotAllowed || code != "method_not_allowed" {
		t.Fatalf("DELETE on a list = %d %s, want 405 method_not_allowed", status, code)
	}
}

func TestMalformedJSONIsAJSONError(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.server.URL+"/v1/ssh_keys", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got errorBody
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if resp.StatusCode != http.StatusBadRequest || got.Error.Code != "json_error" {
		t.Fatalf("malformed body = %d %s, want 400 json_error", resp.StatusCode, got.Error.Code)
	}
}

func TestLocationsAndServerTypesFollowTheCatalog(t *testing.T) {
	e := newEnv(t)
	var location struct {
		Location locationJSON `json:"location"`
	}
	var types struct {
		ServerTypes []serverTypeJSON `json:"server_types"`
	}

	e.mustCall(http.StatusOK, http.MethodGet, "/v1/locations/4", nil, &location)
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/server_types?name=cax11", nil, &types)

	if location.Location.Name != "ash" || location.Location.NetworkZone != "us-east" {
		t.Fatalf("location 4 = %+v, want ash in us-east", location.Location)
	}
	if len(types.ServerTypes) != 1 || types.ServerTypes[0].Architecture != "arm" || len(types.ServerTypes[0].Locations) != 3 {
		t.Fatalf("cax11 = %+v, want one arm type offered in three EU locations", types.ServerTypes)
	}
}
