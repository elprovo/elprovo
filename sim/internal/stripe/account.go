package stripe

import (
	"crypto/rand"
	"strings"
	"time"
)

const (
	eventRetention       = 30 * 24 * time.Hour
	idempotencyRetention = 24 * time.Hour
)

type account struct {
	Customers        []*customer        `json:"customers"`
	Products         []*product         `json:"products"`
	Prices           []*price           `json:"prices"`
	CheckoutSessions []*checkoutSession `json:"checkout_sessions"`
	Subscriptions    []*subscription    `json:"subscriptions"`
	Invoices         []*invoice         `json:"invoices"`
	PaymentIntents   []*paymentIntent   `json:"payment_intents"`
	PaymentMethods   []*paymentMethod   `json:"payment_methods"`
	Events           []*event           `json:"events"`
	WebhookEndpoints []*webhookEndpoint `json:"webhook_endpoints"`
	PortalSessions   []*portalSession   `json:"portal_sessions"`
	TestClocks       []*testClock       `json:"test_clocks"`

	CheckoutOrders  map[string]checkoutOrder `json:"checkout_orders"`
	PortalSecrets   map[string]string        `json:"portal_secrets"`
	RenewalDeclines map[string]bool          `json:"renewal_declines"`
	ArmedFailures   map[string]bool          `json:"armed_failures"`
	TrialReminders  map[string]bool          `json:"trial_reminders"`
	IdempotencyKeys []*idempotencyRecord     `json:"idempotency_keys"`
}

type checkoutOrder struct {
	LineItems            []lineItem        `json:"line_items"`
	SubscriptionMetadata map[string]string `json:"subscription_metadata"`
	TrialPeriodDays      int64             `json:"trial_period_days"`
	MissingPaymentMethod string            `json:"missing_payment_method"`
}

type lineItem struct {
	Price    string `json:"price"`
	Quantity int64  `json:"quantity"`
}

type idempotencyRecord struct {
	Key      string `json:"key"`
	Endpoint string `json:"endpoint"`
	Params   string `json:"params"`
	Status   int    `json:"status"`
	Body     []byte `json:"body"`
	Created  int64  `json:"created"`
}

func (a *account) ensureMaps() {
	if a.CheckoutOrders == nil {
		a.CheckoutOrders = map[string]checkoutOrder{}
	}
	if a.PortalSecrets == nil {
		a.PortalSecrets = map[string]string{}
	}
	if a.RenewalDeclines == nil {
		a.RenewalDeclines = map[string]bool{}
	}
	if a.ArmedFailures == nil {
		a.ArmedFailures = map[string]bool{}
	}
	if a.TrialReminders == nil {
		a.TrialReminders = map[string]bool{}
	}
}

type identified interface{ objectID() string }

func (o *customer) objectID() string        { return o.ID }
func (o *product) objectID() string         { return o.ID }
func (o *price) objectID() string           { return o.ID }
func (o *checkoutSession) objectID() string { return o.ID }
func (o *subscription) objectID() string    { return o.ID }
func (o *invoice) objectID() string         { return o.ID }
func (o *paymentIntent) objectID() string   { return o.ID }
func (o *paymentMethod) objectID() string   { return o.ID }
func (o *event) objectID() string           { return o.ID }
func (o *webhookEndpoint) objectID() string { return o.ID }
func (o *portalSession) objectID() string   { return o.ID }
func (o *testClock) objectID() string       { return o.ID }

func find[P identified](items []P, id string) P {
	for _, item := range items {
		if item.objectID() == id {
			return item
		}
	}
	var none P
	return none
}

func (a *account) objectByID(id string) (any, bool) {
	prefix, _, _ := strings.Cut(id, "_")
	var obj identified
	switch prefix {
	case "cus":
		obj = nilIfMissing(find(a.Customers, id))
	case "prod":
		obj = nilIfMissing(find(a.Products, id))
	case "price":
		obj = nilIfMissing(find(a.Prices, id))
	case "cs":
		obj = nilIfMissing(find(a.CheckoutSessions, id))
	case "sub":
		obj = nilIfMissing(find(a.Subscriptions, id))
	case "in":
		obj = nilIfMissing(find(a.Invoices, id))
	case "pi":
		obj = nilIfMissing(find(a.PaymentIntents, id))
	case "pm":
		obj = nilIfMissing(find(a.PaymentMethods, id))
	case "clock":
		obj = nilIfMissing(find(a.TestClocks, id))
	}
	return obj, obj != nil
}

func nilIfMissing[P interface {
	*T
	identified
}, T any](p P) identified {
	if p == nil {
		return nil
	}
	return p
}

func (a *account) pruneIdempotencyKeys(now time.Time) {
	cutoff := now.Add(-idempotencyRetention).Unix()
	kept := a.IdempotencyKeys[:0]
	for _, r := range a.IdempotencyKeys {
		if r.Created >= cutoff {
			kept = append(kept, r)
		}
	}
	a.IdempotencyKeys = kept
}

func (a *account) idempotencyRecord(key string, now time.Time) *idempotencyRecord {
	cutoff := now.Add(-idempotencyRetention).Unix()
	for _, r := range a.IdempotencyKeys {
		if r.Key == key && r.Created >= cutoff {
			return r
		}
	}
	return nil
}

func (a *account) pruneEvents(now time.Time) {
	cutoff := now.Add(-eventRetention).Unix()
	i := 0
	for i < len(a.Events) && a.Events[i].Created < cutoff {
		i++
	}
	a.Events = a.Events[i:]
}

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

func newID(prefix string) string {
	return prefix + "_" + randomString(24)
}
