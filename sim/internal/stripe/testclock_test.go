package stripe

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"
)

var clockStart = time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)

func (e *testEnv) clockSubscription(card string) (clockID, customerID, subID string) {
	e.t.Helper()
	clock := e.ok(http.MethodPost, "/v1/test_helpers/test_clocks", url.Values{"frozen_time": {unix(clockStart)}})
	clockID = clock["id"].(string)
	customerID, subID = e.subscribe(url.Values{"email": {"ada@example.com"}, "test_clock": {clockID}}, card)
	return clockID, customerID, subID
}

func (e *testEnv) advance(clockID string, to time.Time) map[string]any {
	e.t.Helper()
	return e.ok(http.MethodPost, "/v1/test_helpers/test_clocks/"+clockID+"/advance", url.Values{"frozen_time": {unix(to)}})
}

func (e *testEnv) latestInvoice(subID string) map[string]any {
	e.t.Helper()
	sub := e.ok(http.MethodGet, "/v1/subscriptions/"+subID, url.Values{"expand[]": {"latest_invoice.payment_intent"}})
	return sub["latest_invoice"].(map[string]any)
}

func unix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

func TestAdvancingClockPastPeriodEndRenewsWithPaidInvoice(t *testing.T) {
	env := newTestEnv(t)
	clockID, _, subID := env.clockSubscription("4242424242424242")
	sub := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if sub["current_period_start"] != float64(clockStart.Unix()) {
		t.Fatalf("current_period_start = %v, want the clock's frozen time %d", sub["current_period_start"], clockStart.Unix())
	}
	firstInvoice := sub["latest_invoice"]
	seen := len(env.events())

	advanced := env.advance(clockID, clockStart.AddDate(0, 1, 1))

	if advanced["status"] != "advancing" {
		t.Errorf("advance status = %v, want advancing", advanced["status"])
	}
	if got := env.ok(http.MethodGet, "/v1/test_helpers/test_clocks/"+clockID, nil)["status"]; got != "ready" {
		t.Errorf("clock status after advance = %v, want ready", got)
	}
	sub = env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	wantStart, wantEnd := clockStart.AddDate(0, 1, 0).Unix(), clockStart.AddDate(0, 2, 0).Unix()
	if sub["status"] != "active" || sub["current_period_start"] != float64(wantStart) || sub["current_period_end"] != float64(wantEnd) {
		t.Errorf("subscription status=%v period=%v..%v; want active %d..%d",
			sub["status"], sub["current_period_start"], sub["current_period_end"], wantStart, wantEnd)
	}
	inv := env.latestInvoice(subID)
	if inv["id"] == firstInvoice || inv["billing_reason"] != "subscription_cycle" || inv["status"] != "paid" || inv["amount_paid"] != float64(2000) {
		t.Errorf("renewal invoice = %v %v %v paid %v; want a new paid subscription_cycle invoice for 2000",
			inv["id"], inv["billing_reason"], inv["status"], inv["amount_paid"])
	}
	wantEvents := []string{
		"test_helpers.test_clock.advancing",
		"invoice.created",
		"customer.subscription.updated",
		"invoice.finalized",
		"payment_intent.created",
		"payment_intent.succeeded",
		"invoice.paid",
		"invoice.payment_succeeded",
		"test_helpers.test_clock.ready",
	}
	if got := env.eventTypesSince(seen); !slices.Equal(got, wantEvents) {
		t.Errorf("events = %v\nwant %v", got, wantEvents)
	}
}

func TestRenewalWithDecliningCardGoesPastDueThenEndsAfterRetries(t *testing.T) {
	cases := []struct {
		name       string
		markUnpaid bool
		wantStatus string
		wantEvent  string
	}{
		{"cancels by default", false, "canceled", "customer.subscription.deleted"},
		{"marks unpaid when configured", true, "unpaid", "customer.subscription.updated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, func(c *config) { c.markUnpaid = tc.markUnpaid })
			clockID, _, subID := env.clockSubscription("4000000000000341")
			periodEnd := clockStart.AddDate(0, 1, 0)

			env.advance(clockID, periodEnd.Add(time.Hour))

			sub := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
			inv := env.latestInvoice(subID)
			if sub["status"] != "past_due" || inv["status"] != "open" || inv["attempt_count"] != float64(1) {
				t.Fatalf("after first failure: subscription %v, invoice %v with %v attempts; want past_due, open, 1",
					sub["status"], inv["status"], inv["attempt_count"])
			}
			if want := float64(periodEnd.AddDate(0, 0, 3).Unix()); inv["next_payment_attempt"] != want {
				t.Errorf("next_payment_attempt = %v, want three days after the failure (%v)", inv["next_payment_attempt"], want)
			}
			if got := dig(inv, "payment_intent", "last_payment_error", "decline_code"); got != "generic_decline" {
				t.Errorf("payment intent decline_code = %v, want generic_decline", got)
			}

			seen := len(env.events())
			env.advance(clockID, periodEnd.AddDate(0, 0, 8))

			sub = env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
			if sub["status"] != tc.wantStatus {
				t.Errorf("after every retry failed: status = %v, want %s", sub["status"], tc.wantStatus)
			}
			if got := env.latestInvoice(subID)["attempt_count"]; got != float64(4) {
				t.Errorf("attempt_count = %v, want 4", got)
			}
			if !slices.Contains(env.eventTypesSince(seen), tc.wantEvent) {
				t.Errorf("events lack %s", tc.wantEvent)
			}
		})
	}
}

func TestArmedFailureFailsNextRenewalAndRetryRecovers(t *testing.T) {
	env := newTestEnv(t)
	clockID, customerID, subID := env.clockSubscription("4242424242424242")
	resp := env.postPage(env.web.URL+"/_sim/customers/"+customerID+"/fail_next_payment", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("arming a failure answered %d, want 200", resp.status)
	}
	periodEnd := clockStart.AddDate(0, 1, 0)

	env.advance(clockID, periodEnd.Add(time.Hour))
	if got := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)["status"]; got != "past_due" {
		t.Fatalf("after armed failure: status = %v, want past_due", got)
	}

	env.advance(clockID, periodEnd.AddDate(0, 0, 4))
	if got := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)["status"]; got != "active" {
		t.Errorf("after the retry succeeded: status = %v, want active", got)
	}
	if got := env.latestInvoice(subID)["status"]; got != "paid" {
		t.Errorf("renewal invoice status = %v, want paid", got)
	}
}

func TestCancelAtPeriodEndEndsSubscriptionAtPeriodEnd(t *testing.T) {
	env := newTestEnv(t)
	clockID, _, subID := env.clockSubscription("4242424242424242")
	env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{"cancel_at_period_end": {"true"}})
	periodEnd := clockStart.AddDate(0, 1, 0)
	seen := len(env.events())

	env.advance(clockID, periodEnd.Add(time.Hour))

	sub := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if sub["status"] != "canceled" || sub["ended_at"] != float64(periodEnd.Unix()) {
		t.Errorf("status=%v ended_at=%v; want canceled at %d", sub["status"], sub["ended_at"], periodEnd.Unix())
	}
	want := []string{"test_helpers.test_clock.advancing", "customer.subscription.deleted", "test_helpers.test_clock.ready"}
	if got := env.eventTypesSince(seen); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}
