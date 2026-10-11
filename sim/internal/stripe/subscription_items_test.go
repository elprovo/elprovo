package stripe

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

func (e *testEnv) createPrice(form url.Values) string {
	e.t.Helper()
	base := url.Values{
		"product":             {seedProductID},
		"currency":            {"usd"},
		"unit_amount":         {"5000"},
		"recurring[interval]": {"month"},
	}
	for k, v := range form {
		base[k] = v
	}
	return e.ok(http.MethodPost, "/v1/prices", base)["id"].(string)
}

func items(sub map[string]any) []map[string]any {
	var out []map[string]any
	for _, item := range dig(sub, "items", "data").([]any) {
		out = append(out, item.(map[string]any))
	}
	return out
}

func TestUpdatingItemPriceAndQuantityKeepsBillingPeriod(t *testing.T) {
	env := newTestEnv(t)
	_, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	plusID := env.createPrice(nil)
	before := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	itemID := items(before)[0]["id"].(string)
	seen := len(env.events())

	sub := env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{
		"items[0][id]":       {itemID},
		"items[0][price]":    {plusID},
		"items[0][quantity]": {"3"},
	})

	got := items(sub)
	if len(got) != 1 || got[0]["id"] != itemID || dig(got[0], "price", "id") != plusID || dig(got[0], "plan", "id") != plusID || got[0]["quantity"] != float64(3) {
		t.Fatalf("items = %v, want item %s on %s with quantity 3", got, itemID, plusID)
	}
	for _, field := range []string{"current_period_start", "current_period_end", "billing_cycle_anchor", "latest_invoice"} {
		if sub[field] != before[field] {
			t.Errorf("%s = %v, want it unchanged (%v)", field, sub[field], before[field])
		}
	}
	if got := env.eventTypesSince(seen); !slices.Equal(got, []string{"customer.subscription.updated"}) {
		t.Fatalf("events = %v, want one customer.subscription.updated", got)
	}
	previous, _ := dig(env.events()[seen], "data", "previous_attributes", "items", "data").([]any)
	if len(previous) != 1 || dig(previous[0], "price", "id") != seedPriceID || dig(previous[0], "quantity") != float64(1) {
		t.Errorf("previous_attributes.items.data = %v, want the item on %s with quantity 1", previous, seedPriceID)
	}
	listed := env.ok(http.MethodGet, "/v1/subscriptions", url.Values{"price": {plusID}})["data"].([]any)
	if len(listed) != 1 || dig(listed[0], "id") != subID {
		t.Errorf("subscriptions listed for price %s = %v, want %s", plusID, listed, subID)
	}
}

func TestChangingItemPriceWithoutQuantityResetsQuantityToOne(t *testing.T) {
	env := newTestEnv(t)
	_, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	itemID := items(env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil))[0]["id"].(string)
	env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{"items[0][id]": {itemID}, "items[0][quantity]": {"3"}})

	sub := env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{
		"items[0][id]":    {itemID},
		"items[0][price]": {env.createPrice(nil)},
	})

	if got := items(sub)[0]["quantity"]; got != float64(1) {
		t.Errorf("quantity after a price change without quantity = %v, want 1", got)
	}
}

func TestItemsWithoutIDAreAddedAndDeletedItemsAreRemoved(t *testing.T) {
	env := newTestEnv(t)
	_, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	original := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	originalID := items(original)[0]["id"].(string)
	addonID := env.createPrice(url.Values{"unit_amount": {"500"}})

	added := items(env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{"items[0][price]": {addonID}}))
	if len(added) != 2 || dig(added[1], "price", "id") != addonID || added[1]["quantity"] != float64(1) {
		t.Fatalf("items after adding = %v, want the original item and %s with quantity 1", added, addonID)
	}
	if added[1]["current_period_end"] != original["current_period_end"] {
		t.Errorf("new item current_period_end = %v, want the subscription's %v", added[1]["current_period_end"], original["current_period_end"])
	}

	removed := items(env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{
		"items[0][id]":      {originalID},
		"items[0][deleted]": {"true"},
	}))
	if len(removed) != 1 || removed[0]["id"] != added[1]["id"] {
		t.Errorf("items after deleting %s = %v, want only the added item", originalID, removed)
	}
}

func TestChangingIntervalStartsNewPeriodAndInvoicesNow(t *testing.T) {
	env := newTestEnv(t)
	clockID, _, subID := env.clockSubscription("4242424242424242")
	yearlyID := env.createPrice(url.Values{"unit_amount": {"20000"}, "recurring[interval]": {"year"}})
	itemID := items(env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil))[0]["id"].(string)
	changedAt := clockStart.AddDate(0, 0, 10)
	env.advance(clockID, changedAt)
	seen := len(env.events())

	sub := env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{
		"items[0][id]":       {itemID},
		"items[0][price]":    {yearlyID},
		"proration_behavior": {"none"},
	})

	wantStart, wantEnd := float64(changedAt.Unix()), float64(changedAt.AddDate(1, 0, 0).Unix())
	if sub["billing_cycle_anchor"] != wantStart || sub["current_period_start"] != wantStart || sub["current_period_end"] != wantEnd {
		t.Errorf("anchor=%v period=%v..%v; want %v and %v..%v",
			sub["billing_cycle_anchor"], sub["current_period_start"], sub["current_period_end"], wantStart, wantStart, wantEnd)
	}
	if item := items(sub)[0]; item["current_period_end"] != wantEnd {
		t.Errorf("item current_period_end = %v, want %v", item["current_period_end"], wantEnd)
	}
	inv := env.latestInvoice(subID)
	if inv["billing_reason"] != "subscription_update" || inv["status"] != "paid" || inv["amount_paid"] != float64(20000) {
		t.Errorf("invoice = %v %v paid %v; want a paid subscription_update invoice for 20000", inv["billing_reason"], inv["status"], inv["amount_paid"])
	}
	wantEvents := []string{
		"invoice.created",
		"customer.subscription.updated",
		"invoice.finalized",
		"payment_intent.created",
		"payment_intent.succeeded",
		"invoice.paid",
		"invoice.payment_succeeded",
	}
	if got := env.eventTypesSince(seen); !slices.Equal(got, wantEvents) {
		t.Errorf("events = %v\nwant %v", got, wantEvents)
	}
}

func TestChangingIntervalDuringTrialKeepsTrialAndBillsNewIntervalAtTrialEnd(t *testing.T) {
	env := newTestEnv(t)
	clockID, _, subID := env.clockTrial(nil, "4242424242424242")
	yearlyID := env.createPrice(url.Values{"unit_amount": {"20000"}, "recurring[interval]": {"year"}})
	before := env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)

	sub := env.ok(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{
		"items[0][id]":    {items(before)[0]["id"].(string)},
		"items[0][price]": {yearlyID},
	})

	if sub["status"] != "trialing" || sub["current_period_end"] != float64(trialEnd.Unix()) || sub["latest_invoice"] != before["latest_invoice"] {
		t.Fatalf("status=%v period end=%v latest_invoice=%v; want trialing until %d with no new invoice",
			sub["status"], sub["current_period_end"], sub["latest_invoice"], trialEnd.Unix())
	}
	env.advance(clockID, trialEnd.Add(time.Hour))
	sub = env.ok(http.MethodGet, "/v1/subscriptions/"+subID, nil)
	if want := float64(trialEnd.AddDate(1, 0, 0).Unix()); sub["status"] != "active" || sub["current_period_end"] != want {
		t.Errorf("after the trial: status=%v period end=%v; want active until %v", sub["status"], sub["current_period_end"], want)
	}
	if got := env.latestInvoice(subID)["amount_paid"]; got != float64(20000) {
		t.Errorf("first paid invoice amount_paid = %v, want 20000 for the yearly price", got)
	}
}

func TestInvalidItemUpdatesAreRejectedWithoutChanges(t *testing.T) {
	env := newTestEnv(t)
	_, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	path := "/v1/subscriptions/" + subID
	itemID := items(env.ok(http.MethodGet, path, nil))[0]["id"].(string)
	inactiveID := env.createPrice(url.Values{"active": {"false"}})
	oneTimeID := env.createPrice(url.Values{"recurring[interval]": {""}})
	euroID := env.createPrice(url.Values{"currency": {"eur"}})
	yearlyID := env.createPrice(url.Values{"recurring[interval]": {"year"}})
	cases := []struct {
		name      string
		form      url.Values
		wantCode  string
		wantParam string
	}{
		{"unknown item", url.Values{"items[0][id]": {"si_missing"}, "items[0][quantity]": {"2"}}, "resource_missing", "items[0][id]"},
		{"unknown price", url.Values{"items[0][id]": {itemID}, "items[0][price]": {"price_missing"}}, "resource_missing", "items[0][price]"},
		{"inactive price", url.Values{"items[0][price]": {inactiveID}}, "", "items[0][price]"},
		{"one-time price", url.Values{"items[0][price]": {oneTimeID}}, "", "items[0][price]"},
		{"negative quantity", url.Values{"items[0][id]": {itemID}, "items[0][quantity]": {"-1"}}, "", "items[0][quantity]"},
		{"delete without id", url.Values{"items[0][deleted]": {"true"}}, "parameter_missing", "items[0][id]"},
		{"new item without price", url.Values{"items[0][quantity]": {"2"}}, "parameter_missing", "items[0][price]"},
		{"every item removed", url.Values{"items[0][id]": {itemID}, "items[0][deleted]": {"true"}}, "", "items"},
		{"other currency", url.Values{"items[0][price]": {euroID}}, "", "items"},
		{"other interval", url.Values{"items[0][price]": {yearlyID}}, "", "items"},
		{"unknown proration behavior", url.Values{"items[0][id]": {itemID}, "proration_behavior": {"later"}}, "", "proration_behavior"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := len(env.events())

			resp := env.request(http.MethodPost, path, tc.form, nil)

			if resp.status != http.StatusBadRequest || dig(resp.body, "error", "type") != "invalid_request_error" {
				t.Fatalf("status %d, body %v; want 400 invalid_request_error", resp.status, resp.body)
			}
			if got := dig(resp.body, "error", "param"); got != tc.wantParam {
				t.Errorf("error.param = %v, want %s", got, tc.wantParam)
			}
			if got, _ := dig(resp.body, "error", "code").(string); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
			if got := items(env.ok(http.MethodGet, path, nil)); len(got) != 1 || got[0]["id"] != itemID || dig(got[0], "price", "id") != seedPriceID {
				t.Errorf("items after the rejected update = %v, want the original item", got)
			}
			if n := len(env.events()) - seen; n != 0 {
				t.Errorf("rejected update recorded %d events, want none", n)
			}
		})
	}
}

func TestCanceledSubscriptionCannotChangeItems(t *testing.T) {
	env := newTestEnv(t)
	_, subID := env.subscribe(url.Values{"email": {"ada@example.com"}}, "4242424242424242")
	env.ok(http.MethodDelete, "/v1/subscriptions/"+subID, nil)

	resp := env.request(http.MethodPost, "/v1/subscriptions/"+subID, url.Values{"items[0][price]": {env.createPrice(nil)}}, nil)

	if resp.status != http.StatusBadRequest || dig(resp.body, "error", "param") != "items" {
		t.Errorf("status %d, body %v; want 400 for param items", resp.status, resp.body)
	}
}
