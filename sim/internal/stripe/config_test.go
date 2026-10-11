package stripe

import (
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
