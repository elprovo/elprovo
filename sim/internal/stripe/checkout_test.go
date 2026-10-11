package stripe

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompletingCheckoutCreatesPaidSubscriptionAndEventsInOrder(t *testing.T) {
	receiver := newHookReceiver(t, 0)
	env := newTestEnv(t, withWebhook(receiver.URL))
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}, "client_reference_id": {"acct_42"}})
	if session["status"] != "open" || session["payment_status"] != "unpaid" {
		t.Fatalf("new session status, payment_status = %v, %v; want open, unpaid", session["status"], session["payment_status"])
	}
	sessionURL := session["url"].(string)
	if !strings.HasPrefix(sessionURL, env.web.URL+"/c/pay/cs_test_") {
		t.Fatalf("url = %q, want the hosted page on %s", sessionURL, env.web.URL)
	}

	resp := env.submitCheckout(sessionURL, "4242 4242 4242 4242", nil)

	if want := "https://app.example/billing?session=" + session["id"].(string); resp.status != http.StatusSeeOther || resp.location != want {
		t.Fatalf("redirect = %d %q, want 303 to %q", resp.status, resp.location, want)
	}
	completed := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), url.Values{
		"expand[]": {"subscription", "invoice.payment_intent", "customer"},
	})
	checks := map[string][2]any{
		"session status":          {dig(completed, "status"), "complete"},
		"session payment_status":  {dig(completed, "payment_status"), "paid"},
		"client_reference_id":     {dig(completed, "client_reference_id"), "acct_42"},
		"customer email":          {dig(completed, "customer", "email"), "ada@example.com"},
		"subscription status":     {dig(completed, "subscription", "status"), "active"},
		"cancel_at_period_end":    {dig(completed, "subscription", "cancel_at_period_end"), false},
		"invoice status":          {dig(completed, "invoice", "status"), "paid"},
		"invoice amount_paid":     {dig(completed, "invoice", "amount_paid"), float64(2000)},
		"invoice billing_reason":  {dig(completed, "invoice", "billing_reason"), "subscription_create"},
		"payment intent status":   {dig(completed, "invoice", "payment_intent", "status"), "succeeded"},
		"payment intent amount":   {dig(completed, "invoice", "payment_intent", "amount_received"), float64(2000)},
		"invoice subscription":    {dig(completed, "invoice", "parent", "subscription_details", "subscription"), dig(completed, "subscription", "id")},
		"subscription first item": {dig(completed, "subscription", "items", "data").([]any)[0].(map[string]any)["price"].(map[string]any)["id"], seedPriceID},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	start := time.Unix(int64(dig(completed, "subscription", "current_period_start").(float64)), 0).UTC()
	end := time.Unix(int64(dig(completed, "subscription", "current_period_end").(float64)), 0).UTC()
	if !end.Equal(start.AddDate(0, 1, 0)) {
		t.Errorf("period = %v to %v, want one month", start, end)
	}
	pm := env.ok(http.MethodGet, "/v1/payment_methods/"+dig(completed, "subscription", "default_payment_method").(string), nil)
	if dig(pm, "card", "last4") != "4242" || dig(pm, "card", "brand") != "visa" {
		t.Errorf("payment method card = %v, want visa ending 4242", pm["card"])
	}

	wantTypes := []string{
		"customer.created",
		"payment_method.attached",
		"customer.subscription.created",
		"invoice.created",
		"invoice.finalized",
		"payment_intent.created",
		"payment_intent.succeeded",
		"invoice.paid",
		"invoice.payment_succeeded",
		"customer.subscription.updated",
		"checkout.session.completed",
	}
	events := env.events()
	var gotTypes []string
	for _, ev := range events {
		gotTypes = append(gotTypes, ev["type"].(string))
	}
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("event types = %v\nwant %v", gotTypes, wantTypes)
	}
	if got := dig(events[2], "data", "object", "status"); got != "incomplete" {
		t.Errorf("customer.subscription.created shows status %v, want the incomplete snapshot", got)
	}
	if got := dig(events[9], "data", "previous_attributes", "status"); got != "incomplete" {
		t.Errorf("customer.subscription.updated previous_attributes.status = %v, want incomplete", got)
	}

	var delivered []string
	for _, hook := range receiver.waitFor(t, len(wantTypes)) {
		delivered = append(delivered, verifiedEvent(t, hook, hookSecret)["type"].(string))
	}
	slices.Sort(delivered)
	slices.Sort(wantTypes)
	if !slices.Equal(delivered, wantTypes) {
		t.Errorf("delivered event types = %v, want %v", delivered, wantTypes)
	}
}

func TestDeclinedCardKeepsCheckoutOpenAndShowsDecline(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}})

	resp := env.submitCheckout(session["url"].(string), "4000 0000 0000 0002", nil)

	if resp.status != http.StatusOK || !strings.Contains(resp.body, "Your card was declined.") {
		t.Fatalf("status %d; want 200 and the decline message in the page", resp.status)
	}
	if got := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), nil)["status"]; got != "open" {
		t.Errorf("session status = %v, want open", got)
	}
	if n := len(env.events()); n != 0 {
		t.Errorf("events = %d, want none after a declined card", n)
	}
}

func TestExpiringCheckoutSessionEmitsExpiredEvent(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(nil)

	expired := env.ok(http.MethodPost, "/v1/checkout/sessions/"+session["id"].(string)+"/expire", nil)

	if expired["status"] != "expired" || expired["url"] != nil {
		t.Errorf("status, url = %v, %v; want expired, null", expired["status"], expired["url"])
	}
	if got := env.eventTypesSince(0); !slices.Equal(got, []string{"checkout.session.expired"}) {
		t.Errorf("events = %v, want [checkout.session.expired]", got)
	}
}

func TestAdminCompletesCheckoutWithChosenTestCard(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}})
	complete := env.web.URL + "/_sim/checkout/sessions/" + session["id"].(string) + "/complete"

	declined := env.postPage(complete, url.Values{"card": {"4000000000000002"}})
	if declined.status != http.StatusPaymentRequired {
		t.Fatalf("declining card answered %d, want 402", declined.status)
	}

	paid := env.postPage(complete, nil)
	if paid.status != http.StatusOK {
		t.Fatalf("default card answered %d, want 200", paid.status)
	}
	if got := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), nil)["status"]; got != "complete" {
		t.Errorf("session status = %v, want complete", got)
	}
}

func TestCheckoutWithTrialStartsTrialingSubscriptionWithFreeInvoice(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}, "subscription_data[trial_period_days]": {"14"}})
	if session["amount_total"] != float64(0) || session["payment_method_collection"] != "always" {
		t.Fatalf("amount_total, payment_method_collection = %v, %v; want 0 due today and always", session["amount_total"], session["payment_method_collection"])
	}
	page := env.getPage(session["url"].(string))
	for _, want := range []string{"14 days free", "then $20.00 per month", "Start trial", `name="cardNumber"`} {
		if !strings.Contains(page, want) {
			t.Errorf("checkout page lacks %q", want)
		}
	}

	env.submitCheckout(session["url"].(string), "4242424242424242", nil)

	completed := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), url.Values{"expand[]": {"subscription", "invoice"}})
	sub := completed["subscription"].(map[string]any)
	start := sub["start_date"].(float64)
	end := start + 14*24*60*60
	line := dig(completed, "invoice", "lines", "data").([]any)[0]
	checks := map[string][2]any{
		"session payment_status":   {completed["payment_status"], "paid"},
		"status":                   {sub["status"], "trialing"},
		"trial_start":              {sub["trial_start"], start},
		"trial_end":                {sub["trial_end"], end},
		"current_period_start":     {sub["current_period_start"], start},
		"current_period_end":       {sub["current_period_end"], end},
		"billing_cycle_anchor":     {sub["billing_cycle_anchor"], end},
		"missing_payment_method":   {dig(sub, "trial_settings", "end_behavior", "missing_payment_method"), "create_invoice"},
		"invoice billing_reason":   {dig(completed, "invoice", "billing_reason"), "subscription_create"},
		"invoice status":           {dig(completed, "invoice", "status"), "paid"},
		"invoice amount_due":       {dig(completed, "invoice", "amount_due"), float64(0)},
		"invoice payment_intent":   {dig(completed, "invoice", "payment_intent"), nil},
		"invoice line amount":      {dig(line, "amount"), float64(0)},
		"invoice line description": {dig(line, "description"), "Trial period for Team Stagelet"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	wantTypes := []string{
		"customer.created",
		"payment_method.attached",
		"customer.subscription.created",
		"invoice.created",
		"invoice.finalized",
		"invoice.paid",
		"invoice.payment_succeeded",
		"checkout.session.completed",
	}
	if got := env.eventTypesSince(0); !slices.Equal(got, wantTypes) {
		t.Errorf("event types = %v\nwant %v", got, wantTypes)
	}
}

func TestTrialCheckoutCollectingPaymentMethodIfRequiredCompletesWithoutCard(t *testing.T) {
	cases := []struct {
		name       string
		completion func(env *testEnv, session map[string]any) string
		wantStatus int
	}{
		{"hosted page", func(_ *testEnv, session map[string]any) string { return session["url"].(string) }, http.StatusSeeOther},
		{"admin API", func(env *testEnv, session map[string]any) string {
			return env.web.URL + "/_sim/checkout/sessions/" + session["id"].(string) + "/complete"
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			session := env.createSession(url.Values{
				"customer_email":                       {"ada@example.com"},
				"payment_method_collection":            {"if_required"},
				"subscription_data[trial_period_days]": {"14"},
			})

			if status := env.postPage(tc.completion(env, session), nil).status; status != tc.wantStatus {
				t.Fatalf("completion without card details answered %d, want %d", status, tc.wantStatus)
			}

			completed := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), url.Values{"expand[]": {"subscription", "customer"}})
			if completed["payment_method_collection"] != "if_required" || completed["status"] != "complete" {
				t.Errorf("session payment_method_collection, status = %v, %v; want if_required, complete", completed["payment_method_collection"], completed["status"])
			}
			if dig(completed, "subscription", "status") != "trialing" || dig(completed, "subscription", "default_payment_method") != nil {
				t.Errorf("subscription status, default_payment_method = %v, %v; want trialing without a payment method",
					dig(completed, "subscription", "status"), dig(completed, "subscription", "default_payment_method"))
			}
			if got := dig(completed, "customer", "email"); got != "ada@example.com" {
				t.Errorf("customer email = %v, want ada@example.com", got)
			}
			if slices.Contains(env.eventTypesSince(0), "payment_method.attached") {
				t.Errorf("events include payment_method.attached, want no payment method")
			}
		})
	}
}

func TestTrialCheckoutPageWithoutPaymentDueHasNoCardFields(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"payment_method_collection": {"if_required"}, "subscription_data[trial_period_days]": {"14"}})

	page := env.getPage(session["url"].(string))

	if strings.Contains(page, `name="cardNumber"`) || !strings.Contains(page, "Start trial") {
		t.Errorf("checkout page shows card fields or lacks Start trial; want an email-only trial form")
	}
}

func TestCollectingPaymentMethodIfRequiredStillNeedsCardWhenPaymentIsDue(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}, "payment_method_collection": {"if_required"}})

	resp := env.postPage(session["url"].(string), nil)

	if resp.status != http.StatusOK || !strings.Contains(resp.body, "Your card number is incomplete.") {
		t.Fatalf("status %d; want 200 and the incomplete card number message", resp.status)
	}
	if got := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), nil)["status"]; got != "open" {
		t.Errorf("session status = %v, want open", got)
	}
}

func TestTrialShorterThanThreeDaysSendsTrialWillEndAtSignup(t *testing.T) {
	env := newTestEnv(t)
	session := env.createSession(url.Values{"customer_email": {"ada@example.com"}, "subscription_data[trial_period_days]": {"2"}})

	env.submitCheckout(session["url"].(string), "4242424242424242", nil)

	types := env.eventTypesSince(0)
	created := slices.Index(types, "customer.subscription.created")
	if created < 0 || created+1 >= len(types) || types[created+1] != "customer.subscription.trial_will_end" {
		t.Errorf("events = %v, want customer.subscription.trial_will_end right after customer.subscription.created", types)
	}
}

func TestCheckoutRejectsInvalidTrialOptions(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		name      string
		form      url.Values
		wantParam string
	}{
		{"zero trial days", url.Values{"subscription_data[trial_period_days]": {"0"}}, "subscription_data[trial_period_days]"},
		{"more than 730 trial days", url.Values{"subscription_data[trial_period_days]": {"731"}}, "subscription_data[trial_period_days]"},
		{"trial days not an integer", url.Values{"subscription_data[trial_period_days]": {"two"}}, "subscription_data[trial_period_days]"},
		{
			"unknown missing payment method behavior",
			url.Values{"subscription_data[trial_settings][end_behavior][missing_payment_method]": {"delete"}},
			"subscription_data[trial_settings][end_behavior][missing_payment_method]",
		},
		{"unknown payment method collection", url.Values{"payment_method_collection": {"never"}}, "payment_method_collection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"mode":                    {"subscription"},
				"line_items[0][price]":    {seedPriceID},
				"line_items[0][quantity]": {"1"},
			}
			for k, v := range tc.form {
				form[k] = v
			}

			resp := env.request(http.MethodPost, "/v1/checkout/sessions", form, nil)

			if resp.status != http.StatusBadRequest || dig(resp.body, "error", "param") != tc.wantParam {
				t.Errorf("status %d, body %v; want 400 for param %s", resp.status, resp.body, tc.wantParam)
			}
		})
	}
}
