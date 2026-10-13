package stripe

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestPortalCancelsAtPeriodEndAndResumes(t *testing.T) {
	env := newTestEnv(t)
	customerID, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	ps := env.ok(http.MethodPost, "/v1/billing_portal/sessions", url.Values{
		"customer":   {customerID},
		"return_url": {"https://app.example/settings"},
	})
	portalURL := ps["url"].(string)
	if ps["object"] != "billing_portal.session" || !strings.HasPrefix(portalURL, env.web.URL+"/p/session/") {
		t.Fatalf("portal session = %v, want a billing_portal.session on the hosted pages", ps)
	}
	page := env.getPage(portalURL)
	for _, want := range []string{"Team Preview", "Your plan renews on", "Visa •••• 4242", "Return to app.example"} {
		if !strings.Contains(page, want) {
			t.Errorf("portal page lacks %q", want)
		}
	}

	seen := len(env.events())
	if resp := env.postPage(portalURL+"/cancel", url.Values{"subscription": {subID}}); resp.status != http.StatusSeeOther {
		t.Fatalf("cancel answered %d, want 303 back to the portal", resp.status)
	}
	sub := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if sub["cancel_at_period_end"] != true || sub["cancel_at"] != sub["current_period_end"] || sub["status"] != "active" {
		t.Errorf("after cancel: cancel_at_period_end=%v cancel_at=%v status=%v; want true, period end, active",
			sub["cancel_at_period_end"], sub["cancel_at"], sub["status"])
	}
	assertSingleUpdate(t, env.events()[seen:], "cancel_at_period_end", false)

	seen = len(env.events())
	env.postPage(portalURL+"/renew", url.Values{"subscription": {subID}})
	sub = env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if sub["cancel_at_period_end"] != false || sub["cancel_at"] != nil {
		t.Errorf("after renew: cancel_at_period_end=%v cancel_at=%v; want false, null", sub["cancel_at_period_end"], sub["cancel_at"])
	}
	assertSingleUpdate(t, env.events()[seen:], "cancel_at_period_end", true)
}

func TestPortalReplacesPaymentMethod(t *testing.T) {
	env := newTestEnv(t)
	customerID, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4000000000000341")
	ps := env.ok(http.MethodPost, "/v1/billing_portal/sessions", url.Values{"customer": {customerID}})
	cardForm := func(number string) url.Values {
		return url.Values{"cardNumber": {number}, "cardExpiry": {"10/35"}, "cardCvc": {"321"}}
	}

	declined := env.postPage(ps["url"].(string)+"/payment_method", cardForm("4000000000000002"))
	if !strings.Contains(declined.body, "Your card was declined.") {
		t.Fatalf("declined card page lacks the decline message")
	}

	env.postPage(ps["url"].(string)+"/payment_method", cardForm("4242424242424242"))
	c := env.ok(http.MethodGet, "/v1/customers/"+customerID, url.Values{"expand[]": {"invoice_settings.default_payment_method"}})
	if got := dig(c, "invoice_settings", "default_payment_method", "card", "last4"); got != "4242" {
		t.Errorf("customer default card last4 = %v, want 4242", got)
	}
	sub := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if sub["default_payment_method"] != dig(c, "invoice_settings", "default_payment_method", "id") {
		t.Errorf("subscription default_payment_method = %v, want the new card", sub["default_payment_method"])
	}
}

func assertSingleUpdate(t *testing.T, events []map[string]any, field string, previous any) {
	t.Helper()
	var types []string
	for _, e := range events {
		types = append(types, e["type"].(string))
	}
	if !slices.Equal(types, []string{"customer.subscription.updated"}) {
		t.Fatalf("events = %v, want one customer.subscription.updated", types)
	}
	if got := dig(events[0], "data", "previous_attributes", field); got != previous {
		t.Errorf("previous_attributes.%s = %v, want %v", field, got, previous)
	}
}

func (e *testEnv) getPage(pageURL string) string {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, pageURL, nil)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", pageURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s: status %d", pageURL, resp.StatusCode)
	}
	return string(body)
}
