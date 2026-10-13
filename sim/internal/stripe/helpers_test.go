package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testKey    = "sk_test_unit"
	hookSecret = "whsec_unit_test_secret"
	waitLimit  = 5 * time.Second
)

type testEnv struct {
	t   *testing.T
	sim *Simulation
	api *httptest.Server
	web *httptest.Server
}

func newTestEnv(t *testing.T, configure ...func(*config)) *testEnv {
	t.Helper()
	web := httptest.NewUnstartedServer(nil)
	cfg := config{
		webURL:          "http://" + web.Listener.Addr().String(),
		secretKeys:      []string{testKey},
		apiVersion:      defaultAPIVersion,
		plan:            planConfig{name: "Team Preview", amount: 2000, currency: "usd", interval: "month"},
		deliveryRetries: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
	}
	for _, c := range configure {
		c(&cfg)
	}
	sim, err := newSimulation(cfg, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newSimulation: %v", err)
	}
	web.Config.Handler = sim.webHandler()
	web.Start()
	t.Cleanup(web.Close)
	api := httptest.NewServer(sim.apiHandler())
	t.Cleanup(api.Close)

	var wg sync.WaitGroup
	wg.Go(func() { sim.hooks.run(t.Context()) })
	t.Cleanup(wg.Wait)
	return &testEnv{t: t, sim: sim, api: api, web: web}
}

func withWebhook(rawURL string) func(*config) {
	return func(c *config) { c.webhooks = append(c.webhooks, webhookConfig{url: rawURL, secret: hookSecret}) }
}

type apiResponse struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *testEnv) request(method, path string, form url.Values, header http.Header) apiResponse {
	e.t.Helper()
	target := e.api.URL + path
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else if len(form) > 0 {
		target += "?" + form.Encode()
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, target, body)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := apiResponse{status: resp.StatusCode, header: resp.Header}
	if err := json.NewDecoder(resp.Body).Decode(&out.body); err != nil {
		e.t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return out
}

func (e *testEnv) ok(method, path string, form url.Values) map[string]any {
	e.t.Helper()
	resp := e.request(method, path, form, nil)
	if resp.status != http.StatusOK {
		e.t.Fatalf("%s %s: status %d, body %v", method, path, resp.status, resp.body)
	}
	return resp.body
}

func (e *testEnv) createSession(form url.Values) map[string]any {
	e.t.Helper()
	base := url.Values{
		"mode":                    {"subscription"},
		"line_items[0][price]":    {seedPriceID},
		"line_items[0][quantity]": {"1"},
		"success_url":             {"https://app.example/billing?session={CHECKOUT_SESSION_ID}"},
		"cancel_url":              {"https://app.example/billing"},
	}
	for k, v := range form {
		base[k] = v
	}
	return e.ok(http.MethodPost, "/v1/checkout/sessions", base)
}

type pageResponse struct {
	status   int
	location string
	body     string
}

func (e *testEnv) submitCheckout(sessionURL, card string, extra url.Values) pageResponse {
	e.t.Helper()
	form := url.Values{"cardNumber": {card}, "cardExpiry": {"12 / 34"}, "cardCvc": {"123"}, "billingName": {"Ada Lovelace"}}
	for k, v := range extra {
		form[k] = v
	}
	return e.postPage(sessionURL, form)
}

func (e *testEnv) postPage(pageURL string, form url.Values) pageResponse {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, pageURL, strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", pageURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("POST %s: read body: %v", pageURL, err)
	}
	return pageResponse{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(body)}
}

func (e *testEnv) subscribe(customerForm url.Values, card string) (customerID, subscriptionID string) {
	e.t.Helper()
	c := e.ok(http.MethodPost, "/v1/customers", customerForm)
	customerID = c["id"].(string)
	session := e.createSession(url.Values{"customer": {customerID}})
	resp := e.submitCheckout(session["url"].(string), card, nil)
	if resp.status != http.StatusSeeOther {
		e.t.Fatalf("checkout answered %d, want a redirect to success_url", resp.status)
	}
	completed := e.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), nil)
	return customerID, completed["subscription"].(string)
}

func (e *testEnv) events() []map[string]any {
	e.t.Helper()
	page := e.ok(http.MethodGet, "/v1/events", url.Values{"limit": {"100"}})
	data := page["data"].([]any)
	oldestFirst := make([]map[string]any, len(data))
	for i, item := range data {
		oldestFirst[len(data)-1-i] = item.(map[string]any)
	}
	return oldestFirst
}

func (e *testEnv) eventTypesSince(n int) []string {
	e.t.Helper()
	var types []string
	for _, ev := range e.events()[n:] {
		types = append(types, ev["type"].(string))
	}
	return types
}

func dig(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

type receivedHook struct {
	signature string
	body      []byte
}

type hookReceiver struct {
	*httptest.Server
	mu       sync.Mutex
	failures int
	got      []receivedHook
	arrived  chan struct{}
}

func newHookReceiver(t *testing.T, failures int) *hookReceiver {
	t.Helper()
	h := &hookReceiver{failures: failures, arrived: make(chan struct{}, 1000)}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, receivedHook{signature: r.Header.Get("Stripe-Signature"), body: body})
		failing := h.failures > 0
		if failing {
			h.failures--
		}
		h.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
		}
		h.arrived <- struct{}{}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookReceiver) waitFor(t *testing.T, n int) []receivedHook {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		h.mu.Lock()
		if len(h.got) >= n {
			got := append([]receivedHook(nil), h.got...)
			h.mu.Unlock()
			return got
		}
		h.mu.Unlock()
		select {
		case <-h.arrived:
		case <-deadline:
			t.Fatalf("received %d webhook requests, want %d", len(h.got), n)
		}
	}
}

func verifiedEvent(t *testing.T, hook receivedHook, secret string) map[string]any {
	t.Helper()
	fields := map[string]string{}
	for part := range strings.SplitSeq(hook.signature, ",") {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fields["t"] + "."))
	mac.Write(hook.body)
	if want := hex.EncodeToString(mac.Sum(nil)); !hmac.Equal([]byte(fields["v1"]), []byte(want)) {
		t.Fatalf("Stripe-Signature %q does not match HMAC-SHA256 of the body with the endpoint secret", hook.signature)
	}
	var e map[string]any
	if err := json.Unmarshal(hook.body, &e); err != nil {
		t.Fatalf("decode webhook body: %v", err)
	}
	return e
}
