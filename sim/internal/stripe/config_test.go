package stripe

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWebhookFlagValues(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"url and secret", "url=http://localhost:8080/hooks/stripe,secret=whsec_abc", false},
		{"missing secret", "url=http://localhost:8080/hooks/stripe", true},
		{"secret without whsec_ prefix", "url=http://localhost:8080/hooks/stripe,secret=abc", true},
		{"relative url", "url=/hooks/stripe,secret=whsec_abc", true},
		{"unknown field", "url=http://localhost:8080/hooks/stripe,secret=whsec_abc,events=all", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWebhookFlag(tc.value)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseWebhookFlag(%q) error = %v, want error %v", tc.value, err, tc.wantErr)
			}
		})
	}
}

func TestFlagsBuildNothingUnlessEnabled(t *testing.T) {
	sim, err := buildFromArgs(t, t.TempDir())
	if sim != nil || err != nil {
		t.Fatalf("build = %v, %v; want nil, nil without -stripe", sim, err)
	}
}

func TestRestartKeepsObjectsAndFollowsWebhookFlags(t *testing.T) {
	dir := t.TempDir()
	first, err := buildFromArgs(t, dir, "-stripe", "-stripe-webhook", "url=http://localhost:9/old,secret=whsec_old")
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	created := callAPI(t, first, http.MethodPost, "/v1/customers", url.Values{"email": {"ada@example.com"}})
	callAPI(t, first, http.MethodPost, "/v1/webhook_endpoints", url.Values{"url": {"http://localhost:9/api"}, "enabled_events[]": {"*"}})

	second, err := buildFromArgs(t, dir, "-stripe", "-stripe-webhook", "url=http://localhost:9/new,secret=whsec_new")
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if got := callAPI(t, second, http.MethodGet, "/v1/customers/"+created["id"].(string), nil)["email"]; got != "ada@example.com" {
		t.Errorf("customer email after restart = %v, want ada@example.com", got)
	}
	prices := callAPI(t, second, http.MethodGet, "/v1/prices", url.Values{"lookup_keys[]": {seedLookupKey}})["data"].([]any)
	if len(prices) != 1 || dig(prices[0], "id") != seedPriceID {
		t.Errorf("seeded prices after restart = %v, want only %s", prices, seedPriceID)
	}
	var urls []string
	for _, ep := range callAPI(t, second, http.MethodGet, "/v1/webhook_endpoints", nil)["data"].([]any) {
		urls = append(urls, dig(ep, "url").(string))
	}
	slices.Sort(urls)
	if want := []string{"http://localhost:9/api", "http://localhost:9/new"}; !slices.Equal(urls, want) {
		t.Errorf("webhook endpoints after restart = %v, want %v: the API endpoint and only the current flag", urls, want)
	}
}

func buildFromArgs(t *testing.T, dataDir string, args ...string) (*Simulation, error) {
	t.Helper()
	fs := flag.NewFlagSet("stagely-sim", flag.ContinueOnError)
	build := Flags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return build(dataDir, slog.New(slog.DiscardHandler))
}

func callAPI(t *testing.T, sim *Simulation, method, path string, form url.Values) map[string]any {
	t.Helper()
	server := httptest.NewServer(sim.apiHandler())
	t.Cleanup(server.Close)
	env := &testEnv{t: t, sim: sim, api: server}
	resp := env.request(method, path, form, http.Header{"Authorization": {"Bearer " + defaultSecretKey}})
	if resp.status != http.StatusOK {
		t.Fatalf("%s %s: status %d, body %v", method, path, resp.status, resp.body)
	}
	return resp.body
}

const testCatalog = `[
  {"lookup_key": "team_monthly", "product": "Team", "amount": 2000, "currency": "usd", "interval": "month"},
  {"lookup_key": "team_yearly", "product": "Team", "amount": 20000, "currency": "usd", "interval": "year"},
  {"lookup_key": "business-monthly", "product": "Business", "amount": 9900, "currency": "usd", "interval": "month"}
]`

func writeCatalog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return path
}

func TestCatalogFileSeedsExactlyItsPrices(t *testing.T) {
	sim, err := buildFromArgs(t, t.TempDir(), "-stripe", "-stripe-catalog", writeCatalog(t, testCatalog))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var got []string
	for _, pr := range callAPI(t, sim, http.MethodGet, "/v1/prices", url.Values{"active": {"true"}})["data"].([]any) {
		got = append(got, fmt.Sprintf("%v %v %v %v %v %v", dig(pr, "id"), dig(pr, "lookup_key"), dig(pr, "product"),
			dig(pr, "currency"), dig(pr, "recurring", "interval"), dig(pr, "unit_amount")))
	}
	slices.Sort(got)
	want := []string{
		"price_business_monthly business-monthly prod_business usd month 9900",
		"price_team_monthly team_monthly prod_team usd month 2000",
		"price_team_yearly team_yearly prod_team usd year 20000",
	}
	if !slices.Equal(got, want) {
		t.Errorf("active prices =\n%v\nwant\n%v", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var products []string
	for _, p := range callAPI(t, sim, http.MethodGet, "/v1/products", nil)["data"].([]any) {
		products = append(products, dig(p, "name").(string))
	}
	slices.Sort(products)
	if !slices.Equal(products, []string{"Business", "Team"}) {
		t.Errorf("products = %v, want one per product name: [Business Team]", products)
	}
}

func TestRestartWithCatalogRetiresTheDefaultPrice(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildFromArgs(t, dir, "-stripe"); err != nil {
		t.Fatalf("first build: %v", err)
	}

	sim, err := buildFromArgs(t, dir, "-stripe", "-stripe-catalog", writeCatalog(t, testCatalog))
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if prices := callAPI(t, sim, http.MethodGet, "/v1/prices", url.Values{"lookup_keys[]": {seedLookupKey}})["data"].([]any); len(prices) != 0 {
		t.Errorf("prices with lookup key %s = %v, want none", seedLookupKey, prices)
	}
	if pr := callAPI(t, sim, http.MethodGet, "/v1/prices/"+seedPriceID, nil); pr["active"] != false {
		t.Errorf("%s active = %v, want false", seedPriceID, pr["active"])
	}
}

func TestInvalidCatalogFileStopsStart(t *testing.T) {
	cases := []struct {
		name    string
		catalog string
		wantErr string
	}{
		{"empty list", `[]`, "at least one price"},
		{"repeated lookup key", `[{"lookup_key":"a","product":"P","amount":1,"currency":"usd","interval":"month"},
			{"lookup_key":"a","product":"P","amount":2,"currency":"usd","interval":"year"}]`, `lookup_key "a" is used twice`},
		{"uppercase currency", `[{"lookup_key":"a","product":"P","amount":1,"currency":"USD","interval":"month"}]`, `currency "USD"`},
		{"unknown interval", `[{"lookup_key":"a","product":"P","amount":1,"currency":"usd","interval":"fortnight"}]`, `interval "fortnight"`},
		{"negative amount", `[{"lookup_key":"a","product":"P","amount":-1,"currency":"usd","interval":"month"}]`, "must not be negative"},
		{"missing amount", `[{"lookup_key":"a","product":"P","currency":"usd","interval":"month"}]`, "amount is required"},
		{"unknown field", `[{"lookup_key":"a","product":"P","amount":1,"currency":"usd","interval":"month","trial":7}]`, `unknown field "trial"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sim, err := buildFromArgs(t, t.TempDir(), "-stripe", "-stripe-catalog", writeCatalog(t, tc.catalog))

			if sim != nil || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("build = %v, %v; want an error that contains %q", sim, err, tc.wantErr)
			}
		})
	}
}
