package stripe

import (
	"fmt"
	"maps"
	"time"
)

const day = 24 * time.Hour

func (sub *subscription) interval() (string, int64) {
	r := sub.Items.Data[0].Price.Recurring
	return r.Interval, r.IntervalCount
}

func (sub *subscription) renews() bool {
	return sub.Status == "active" || sub.Status == "past_due"
}

func (t *tx) newSubscription(c *customer, items []lineItem, paymentMethodID string, metadata map[string]string) *subscription {
	start := t.clockNow(c.TestClock)
	sub := &subscription{
		ID:                   newID("sub"),
		Object:               "subscription",
		BillingCycleAnchor:   start,
		CollectionMethod:     "charge_automatically",
		Created:              start,
		Customer:             c.ID,
		DefaultPaymentMethod: nullString(paymentMethodID),
		Metadata:             maps.Clone(metadata),
		StartDate:            start,
		Status:               "incomplete",
		TestClock:            c.TestClock,
	}
	if sub.Metadata == nil {
		sub.Metadata = map[string]string{}
	}
	data := make([]subscriptionItem, 0, len(items))
	for _, li := range items {
		pr := find(t.acct.Prices, li.Price)
		data = append(data, subscriptionItem{
			ID:           newID("si"),
			Object:       "subscription_item",
			Created:      start,
			Metadata:     map[string]string{},
			Plan:         planOf(*pr),
			Price:        *pr,
			Quantity:     li.Quantity,
			Subscription: sub.ID,
			TaxRates:     []string{},
		})
	}
	sub.Items = subscriptionItems{Object: "list", Data: data, TotalCount: len(data), URL: "/v1/subscription_items?subscription=" + sub.ID}
	sub.Currency = data[0].Price.Currency
	interval, count := sub.interval()
	sub.setPeriod(start, nextBoundary(start, interval, count, start))
	t.acct.Subscriptions = append(t.acct.Subscriptions, sub)
	return sub
}

func (sub *subscription) setPeriod(start, end int64) {
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = start, end
	for i := range sub.Items.Data {
		sub.Items.Data[i].CurrentPeriodStart, sub.Items.Data[i].CurrentPeriodEnd = start, end
	}
}

func (t *tx) newInvoice(sub *subscription, c *customer, reason string, periodStart, periodEnd int64) *invoice {
	inv := &invoice{
		ID:               newID("in"),
		Object:           "invoice",
		AccountCountry:   "US",
		AccountName:      "Stagely",
		AutoAdvance:      true,
		BillingReason:    reason,
		CollectionMethod: "charge_automatically",
		Created:          t.clockNow(sub.TestClock),
		Currency:         sub.Currency,
		Customer:         c.ID,
		CustomerEmail:    c.Email,
		CustomerName:     c.Name,
		Issuer:           invoiceIssuer{Type: "self"},
		Metadata:         map[string]string{},
		Parent: invoiceParent{
			SubscriptionDetails: invoiceSubscriptionDetails{Metadata: maps.Clone(sub.Metadata), Subscription: sub.ID},
			Type:                "subscription_details",
		},
		Payments:     invoicePayments{Object: "list", Data: []invoicePayment{}, URL: "/v1/invoice_payments"},
		PeriodEnd:    periodEnd,
		PeriodStart:  periodStart,
		Status:       "draft",
		Subscription: nullString(sub.ID),
		TestClock:    sub.TestClock,
	}
	lines := make([]invoiceLine, 0, len(sub.Items.Data))
	for _, item := range sub.Items.Data {
		lines = append(lines, t.invoiceLine(inv, sub, item))
		inv.Subtotal += item.Price.UnitAmount * item.Quantity
	}
	inv.Lines = invoiceLines{Object: "list", Data: lines, TotalCount: len(lines), URL: "/v1/invoices/" + inv.ID + "/lines"}
	inv.SubtotalExcludingTax, inv.Total, inv.TotalExcludingTax = inv.Subtotal, inv.Subtotal, inv.Subtotal
	inv.AmountDue, inv.AmountRemaining = inv.Subtotal, inv.Subtotal
	t.acct.Invoices = append(t.acct.Invoices, inv)
	return inv
}

func (t *tx) invoiceLine(inv *invoice, sub *subscription, item subscriptionItem) invoiceLine {
	productName := item.Price.Product
	if p := find(t.acct.Products, item.Price.Product); p != nil {
		productName = p.Name
	}
	amount := item.Price.UnitAmount * item.Quantity
	return invoiceLine{
		ID:       newID("il"),
		Object:   "line_item",
		Amount:   amount,
		Currency: inv.Currency,
		Description: fmt.Sprintf("%d × %s (at %s / %s)", item.Quantity, productName,
			formatMoney(item.Price.UnitAmount, item.Price.Currency), item.Price.Recurring.Interval),
		Discountable: true,
		Invoice:      inv.ID,
		Metadata:     maps.Clone(sub.Metadata),
		Parent: lineParent{
			SubscriptionItemDetails: subscriptionItemDetails{Subscription: sub.ID, SubscriptionItem: item.ID},
			Type:                    "subscription_item_details",
		},
		Period: linePeriod{Start: sub.CurrentPeriodStart, End: sub.CurrentPeriodEnd},
		Pricing: linePricing{
			PriceDetails:      priceDetails{Price: item.Price.ID, Product: item.Price.Product},
			Type:              "price_details",
			UnitAmountDecimal: item.Price.UnitAmountDecimal,
		},
		Quantity:         item.Quantity,
		Subscription:     sub.ID,
		SubscriptionItem: item.ID,
		Subtotal:         amount,
		Type:             "subscription",
	}
}

func (t *tx) finalizeInvoice(inv *invoice, c *customer) {
	at := nullTime(t.clockNow(inv.TestClock))
	inv.Status = "open"
	inv.Number = nullString(fmt.Sprintf("%s-%04d", c.InvoicePrefix, c.NextInvoiceSequence))
	c.NextInvoiceSequence++
	inv.EffectiveAt, inv.StatusTransitions.FinalizedAt, inv.WebhooksDeliveredAt = at, at, at
	inv.HostedInvoiceURL = nullString(t.cfg.webURL + "/i/" + inv.ID)
	t.emit("invoice.finalized", inv)
}

func (t *tx) collectInvoice(inv *invoice, c *customer, paymentMethodID string, automatic bool) bool {
	at := t.clockNow(inv.TestClock)
	pi := t.invoicePaymentIntent(inv, c, paymentMethodID, at)
	pi.PaymentMethod = nullString(paymentMethodID)
	inv.Attempted = true
	inv.AttemptCount++
	if !t.chargeSucceeds(c.ID, paymentMethodID, automatic) {
		t.markPaymentFailed(inv, c, pi)
		return false
	}
	t.markPaid(inv, c, pi, at)
	return true
}

func (t *tx) chargeSucceeds(customerID, paymentMethodID string, automatic bool) bool {
	if paymentMethodID == "" {
		return false
	}
	if !automatic {
		return true
	}
	if t.acct.ArmedFailures[customerID] {
		delete(t.acct.ArmedFailures, customerID)
		return false
	}
	return !t.acct.RenewalDeclines[paymentMethodID]
}

func (t *tx) invoicePaymentIntent(inv *invoice, c *customer, paymentMethodID string, at int64) *paymentIntent {
	if pi := find(t.acct.PaymentIntents, string(inv.PaymentIntent)); pi != nil {
		return pi
	}
	description := "Subscription update"
	if inv.BillingReason == "subscription_create" {
		description = "Subscription creation"
	}
	id := newID("pi")
	pi := &paymentIntent{
		ID:                 id,
		Object:             "payment_intent",
		Amount:             inv.AmountDue,
		CaptureMethod:      "automatic",
		ClientSecret:       id + "_secret_" + randomString(24),
		ConfirmationMethod: "automatic",
		Created:            at,
		Currency:           inv.Currency,
		Customer:           nullString(c.ID),
		Description:        nullString(description),
		Invoice:            nullString(inv.ID),
		Metadata:           map[string]string{},
		PaymentMethod:      nullString(paymentMethodID),
		PaymentMethodTypes: []string{"card"},
		Status:             "requires_confirmation",
	}
	t.acct.PaymentIntents = append(t.acct.PaymentIntents, pi)
	inv.PaymentIntent = nullString(pi.ID)
	inv.Payments.Data = append(inv.Payments.Data, invoicePayment{
		ID:              newID("inpay"),
		Object:          "invoice_payment",
		AmountRequested: inv.AmountDue,
		Created:         at,
		Currency:        inv.Currency,
		Invoice:         inv.ID,
		IsDefault:       true,
		Payment:         invoicePaymentRef{PaymentIntent: pi.ID, Type: "payment_intent"},
		Status:          "open",
	})
	inv.Payments.TotalCount = len(inv.Payments.Data)
	t.emit("payment_intent.created", pi)
	return pi
}

func (t *tx) markPaid(inv *invoice, c *customer, pi *paymentIntent, at int64) {
	pi.Status = "succeeded"
	pi.AmountReceived = pi.Amount
	pi.LastPaymentError = nil
	t.emit("payment_intent.succeeded", pi)

	inv.AmountPaid, inv.AmountRemaining = inv.AmountDue, 0
	inv.Paid = true
	inv.Status = "paid"
	inv.NextPaymentAttempt = 0
	inv.StatusTransitions.PaidAt = nullTime(at)
	for i := range inv.Payments.Data {
		p := &inv.Payments.Data[i]
		p.Status, p.AmountPaid, p.StatusTransitions.PaidAt = "paid", inv.AmountDue, nullTime(at)
	}
	t.emit("invoice.paid", inv)
	t.emit("invoice.payment_succeeded", inv)
	t.setDelinquent(c, false)
}

func (t *tx) markPaymentFailed(inv *invoice, c *customer, pi *paymentIntent) {
	pi.Status = "requires_payment_method"
	pi.LastPaymentError = &cardError{
		Code:        "card_declined",
		DeclineCode: "generic_decline",
		DocURL:      "https://docs.stripe.com/error-codes/card-declined",
		Message:     declinedMessage,
		Type:        "card_error",
	}
	if pm := find(t.acct.PaymentMethods, string(pi.PaymentMethod)); pm != nil {
		failed := *pm
		pi.LastPaymentError.PaymentMethod = &failed
	}
	t.emit("payment_intent.payment_failed", pi)

	inv.NextPaymentAttempt = 0
	if delay, ok := retryDelay(inv.AttemptCount); ok {
		inv.NextPaymentAttempt = nullTime(time.Unix(int64(inv.StatusTransitions.FinalizedAt), 0).Add(delay).Unix())
	}
	t.emit("invoice.payment_failed", inv)
	t.setDelinquent(c, true)
}

func retryDelay(attemptCount int64) (time.Duration, bool) {
	switch attemptCount {
	case 1:
		return 3 * day, true
	case 2:
		return 5 * day, true
	case 3:
		return 7 * day, true
	}
	return 0, false
}

func (t *tx) setDelinquent(c *customer, delinquent bool) {
	if c.Delinquent == delinquent {
		return
	}
	before := snapshot(c)
	c.Delinquent = delinquent
	t.emitUpdate("customer.updated", c, before)
}

func defaultPaymentMethod(sub *subscription, c *customer) string {
	if sub.DefaultPaymentMethod != "" {
		return string(sub.DefaultPaymentMethod)
	}
	return string(c.InvoiceSettings.DefaultPaymentMethod)
}

func (t *tx) endPeriod(sub *subscription) {
	if sub.CancelAtPeriodEnd {
		t.cancelSubscription(sub, sub.CurrentPeriodEnd, "")
		return
	}
	t.renewSubscription(sub)
}

func (t *tx) renewSubscription(sub *subscription) {
	c := find(t.acct.Customers, sub.Customer)
	before := snapshot(sub)
	oldStart, oldEnd := sub.CurrentPeriodStart, sub.CurrentPeriodEnd
	interval, count := sub.interval()
	sub.setPeriod(oldEnd, nextBoundary(sub.BillingCycleAnchor, interval, count, oldEnd))
	inv := t.newInvoice(sub, c, "subscription_cycle", oldStart, oldEnd)
	t.emit("invoice.created", inv)
	sub.LatestInvoice = nullString(inv.ID)
	t.emitUpdate("customer.subscription.updated", sub, before)
	t.finalizeInvoice(inv, c)
	t.settleRenewal(sub, inv, c)
}

func (t *tx) retryInvoice(inv *invoice) {
	sub := find(t.acct.Subscriptions, string(inv.Subscription))
	c := find(t.acct.Customers, inv.Customer)
	t.settleRenewal(sub, inv, c)
}

func (t *tx) settleRenewal(sub *subscription, inv *invoice, c *customer) {
	if t.collectInvoice(inv, c, defaultPaymentMethod(sub, c), true) {
		if sub.Status != "active" && string(sub.LatestInvoice) == inv.ID {
			t.setSubscriptionStatus(sub, "active")
		}
		return
	}
	switch {
	case inv.NextPaymentAttempt != 0:
		if sub.Status == "active" {
			t.setSubscriptionStatus(sub, "past_due")
		}
	case t.cfg.markUnpaid:
		t.setSubscriptionStatus(sub, "unpaid")
	default:
		t.cancelSubscription(sub, t.clockNow(sub.TestClock), "payment_failed")
	}
}

func (t *tx) setSubscriptionStatus(sub *subscription, status string) {
	before := snapshot(sub)
	sub.Status = status
	t.emitUpdate("customer.subscription.updated", sub, before)
}

func (t *tx) cancelSubscription(sub *subscription, at int64, reason string) {
	sub.Status = "canceled"
	sub.EndedAt = nullTime(at)
	if sub.CanceledAt == 0 {
		sub.CanceledAt = nullTime(at)
	}
	if reason != "" {
		sub.CancellationDetails.Reason = nullString(reason)
	}
	for _, inv := range t.acct.Invoices {
		if string(inv.Subscription) == sub.ID && inv.Status == "open" {
			inv.NextPaymentAttempt = 0
			inv.AutoAdvance = false
		}
	}
	t.emit("customer.subscription.deleted", sub)
}

type subscriptionFields struct {
	cancelAtPeriodEnd *bool
	metadata          metadataUpdate
}

func (t *tx) updateSubscription(id string, in subscriptionFields) (*subscription, error) {
	sub := find(t.acct.Subscriptions, id)
	if sub == nil {
		return nil, noSuch("subscription", id, "id")
	}
	if sub.Status == "canceled" && in.cancelAtPeriodEnd != nil {
		return nil, invalidRequest("cancel_at_period_end", "A canceled subscription can only update its cancellation_details and metadata.")
	}
	before := snapshot(sub)
	sub.Metadata = in.metadata.apply(sub.Metadata)
	if in.cancelAtPeriodEnd != nil {
		sub.setCancelAtPeriodEnd(*in.cancelAtPeriodEnd, t.clockNow(sub.TestClock))
	}
	t.emitUpdate("customer.subscription.updated", sub, before)
	return sub, nil
}

func (sub *subscription) setCancelAtPeriodEnd(cancel bool, now int64) {
	if sub.CancelAtPeriodEnd == cancel {
		return
	}
	sub.CancelAtPeriodEnd = cancel
	if cancel {
		sub.CancelAt = nullTime(sub.CurrentPeriodEnd)
		sub.CanceledAt = nullTime(now)
		sub.CancellationDetails.Reason = "cancellation_requested"
		return
	}
	sub.CancelAt, sub.CanceledAt, sub.CancellationDetails.Reason = 0, 0, ""
}

func (t *tx) cancelSubscriptionNow(id string) (*subscription, error) {
	sub := find(t.acct.Subscriptions, id)
	if sub == nil {
		return nil, noSuch("subscription", id, "id")
	}
	if sub.Status == "canceled" {
		return nil, invalidRequest("", "This subscription has already been canceled.")
	}
	t.cancelSubscription(sub, t.clockNow(sub.TestClock), "cancellation_requested")
	return sub, nil
}

func periodBoundary(anchor time.Time, interval string, count, n int64) time.Time {
	k := int(count * n)
	switch interval {
	case "day":
		return anchor.AddDate(0, 0, k)
	case "week":
		return anchor.AddDate(0, 0, 7*k)
	case "year":
		return addMonthsClamped(anchor, 12*k)
	default:
		return addMonthsClamped(anchor, k)
	}
}

func addMonthsClamped(t time.Time, months int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	lastDay := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(t.Day(), lastDay), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

func nextBoundary(anchorUnix int64, interval string, count, after int64) int64 {
	anchor := time.Unix(anchorUnix, 0).UTC()
	for n := int64(1); ; n++ {
		if b := periodBoundary(anchor, interval, count, n).Unix(); b > after {
			return b
		}
	}
}
