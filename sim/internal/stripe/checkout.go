package stripe

import (
	"cmp"
	"fmt"
	"maps"
	"strings"
	"time"
)

const (
	checkoutMinLifetime = 30 * time.Minute
	checkoutMaxLifetime = 24 * time.Hour
	maxTrialPeriodDays  = 730
)

type checkoutFields struct {
	mode                    string
	customer                string
	customerEmail           string
	successURL              string
	cancelURL               string
	clientReferenceID       string
	lineItems               []lineItem
	metadata                map[string]string
	subscriptionMetadata    map[string]string
	expiresAt               int64
	paymentMethodCollection string
	trialPeriodDays         *int64
	missingPaymentMethod    string
}

func (t *tx) createCheckoutSession(in checkoutFields) (*checkoutSession, error) {
	if in.expiresAt == 0 {
		in.expiresAt = t.now.Add(checkoutMaxLifetime).Unix()
	}
	in.paymentMethodCollection = cmp.Or(in.paymentMethodCollection, "always")
	in.missingPaymentMethod = cmp.Or(in.missingPaymentMethod, "create_invoice")
	if err := t.validateCheckout(in); err != nil {
		return nil, err
	}
	var trialDays int64
	if in.trialPeriodDays != nil {
		trialDays = *in.trialPeriodDays
	}
	var subtotal int64
	for _, li := range in.lineItems {
		subtotal += find(t.acct.Prices, li.Price).UnitAmount * li.Quantity
	}
	if trialDays > 0 {
		subtotal = 0
	}
	id := "cs_test_a1" + randomString(56)
	s := &checkoutSession{
		ID:                      id,
		Object:                  "checkout.session",
		AmountSubtotal:          subtotal,
		AmountTotal:             subtotal,
		CancelURL:               nullString(in.cancelURL),
		ClientReferenceID:       nullString(in.clientReferenceID),
		Created:                 t.now.Unix(),
		Currency:                find(t.acct.Prices, in.lineItems[0].Price).Currency,
		Customer:                nullString(in.customer),
		CustomerEmail:           nullString(in.customerEmail),
		ExpiresAt:               in.expiresAt,
		Metadata:                maps.Clone(in.metadata),
		Mode:                    in.mode,
		PaymentMethodCollection: in.paymentMethodCollection,
		PaymentMethodTypes:      []string{"card"},
		PaymentStatus:           "unpaid",
		Status:                  "open",
		SuccessURL:              nullString(in.successURL),
		UIMode:                  "hosted_page",
		URL:                     nullString(t.cfg.webURL + "/c/pay/" + id),
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	t.acct.CheckoutSessions = append(t.acct.CheckoutSessions, s)
	t.acct.CheckoutOrders[id] = checkoutOrder{
		LineItems:            in.lineItems,
		SubscriptionMetadata: in.subscriptionMetadata,
		TrialPeriodDays:      trialDays,
		MissingPaymentMethod: in.missingPaymentMethod,
	}
	return s, nil
}

func (t *tx) validateCheckout(in checkoutFields) error {
	switch {
	case in.mode == "":
		return missingParam("mode")
	case in.mode != "subscription":
		return invalidRequest("mode", "elprovo-sim supports only mode=subscription.")
	case in.customer != "" && in.customerEmail != "":
		return exclusiveParams("customer", "customer_email")
	case in.customer != "" && find(t.acct.Customers, in.customer) == nil:
		return noSuch("customer", in.customer, "customer")
	case len(in.lineItems) == 0:
		return missingParam("line_items")
	}
	if err := t.validateLineItems(in.lineItems); err != nil {
		return err
	}
	if err := validateTrialOptions(in); err != nil {
		return err
	}
	now := t.now.Unix()
	if in.expiresAt < now+int64(checkoutMinLifetime.Seconds()) || in.expiresAt > now+int64(checkoutMaxLifetime.Seconds()) {
		return invalidRequest("expires_at", "The `expires_at` timestamp must be between 30 minutes and 24 hours after Checkout Session creation.")
	}
	return nil
}

func validateTrialOptions(in checkoutFields) error {
	const daysParam = "subscription_data[trial_period_days]"
	const behaviorParam = "subscription_data[trial_settings][end_behavior][missing_payment_method]"
	switch {
	case in.paymentMethodCollection != "always" && in.paymentMethodCollection != "if_required":
		return invalidRequest("payment_method_collection", "Invalid payment_method_collection: must be one of always or if_required")
	case in.trialPeriodDays != nil && *in.trialPeriodDays < 1:
		return invalidRequest(daysParam, "This value must be greater than or equal to 1.")
	case in.trialPeriodDays != nil && *in.trialPeriodDays > maxTrialPeriodDays:
		return invalidRequest(daysParam, "This value must be less than or equal to %d.", maxTrialPeriodDays)
	}
	switch in.missingPaymentMethod {
	case "cancel", "create_invoice", "pause":
		return nil
	}
	return invalidRequest(behaviorParam, "Invalid %s: must be one of cancel, create_invoice, or pause", behaviorParam)
}

func (t *tx) validateLineItems(items []lineItem) error {
	var first *price
	for i, li := range items {
		param := fmt.Sprintf("line_items[%d]", i)
		if li.Price == "" {
			return missingParam(param + "[price]")
		}
		pr := find(t.acct.Prices, li.Price)
		if pr == nil {
			return noSuch("price", li.Price, param+"[price]")
		}
		if li.Quantity < 1 {
			return missingParam(param + "[quantity]")
		}
		if pr.Recurring == nil {
			return invalidRequest(param+"[price]", "elprovo-sim supports only recurring prices in `subscription` mode.")
		}
		if first == nil {
			first = pr
			continue
		}
		if !sameBillingTerms(*pr, *first) {
			return mixedPriceTerms(param + "[price]")
		}
	}
	return nil
}

func (t *tx) expireCheckoutSession(id string) (*checkoutSession, error) {
	s := find(t.acct.CheckoutSessions, id)
	if s == nil {
		return nil, noSuch("checkout.session", id, "id")
	}
	if s.Status != "open" {
		return nil, invalidRequest("", "Only Checkout Sessions with a status in [\"open\"] can be expired. This Checkout Session has a status of \"%s\".", s.Status)
	}
	t.expire(s)
	return s, nil
}

func (t *tx) expire(s *checkoutSession) {
	s.Status = "expired"
	s.URL = ""
	t.emit("checkout.session.expired", s)
}

func (t *tx) expireDueSessions() {
	for _, s := range t.acct.CheckoutSessions {
		if s.Status == "open" && s.ExpiresAt <= t.now.Unix() {
			t.expire(s)
		}
	}
}

func (t *tx) completeCheckout(id string, entry cardEntry) (*checkoutSession, error) {
	s := find(t.acct.CheckoutSessions, id)
	if s == nil {
		return nil, noSuch("checkout.session", id, "id")
	}
	if s.Status != "open" || s.ExpiresAt <= t.now.Unix() {
		return nil, invalidRequest("", "This Checkout Session is no longer active.")
	}
	c := find(t.acct.Customers, string(s.Customer))
	email := checkoutEmail(s, c, entry)
	if !strings.Contains(email, "@") {
		return nil, invalidRequest("email", "Your email address is incomplete.")
	}
	var card validCard
	var err error
	if s.collectsPaymentMethod() {
		if card, err = entry.validate(t.now); err != nil {
			return nil, err
		}
	}

	if c == nil {
		name := entry.name
		if c, err = t.createCustomer(customerFields{email: &email, name: &name}); err != nil {
			return nil, err
		}
	} else if c.Email == "" {
		before := snapshot(c)
		c.Email = nullString(email)
		t.emitUpdate("customer.updated", c, before)
	}
	var paymentMethodID string
	if s.collectsPaymentMethod() {
		paymentMethodID = t.attachCard(c, card, entry.name, email).ID
	}
	sub, inv := t.startSubscription(c, t.acct.CheckoutOrders[s.ID], paymentMethodID)

	s.Status = "complete"
	s.PaymentStatus = "paid"
	s.Customer = nullString(c.ID)
	s.Subscription = nullString(sub.ID)
	s.Invoice = nullString(inv.ID)
	s.CustomerDetails = &customerDetails{Email: nullString(email), Name: nullString(entry.name), TaxExempt: "none", TaxIDs: []string{}}
	s.URL = ""
	t.emit("checkout.session.completed", s)
	return s, nil
}

func (s *checkoutSession) collectsPaymentMethod() bool {
	return s.PaymentMethodCollection == "always" || s.AmountTotal > 0
}

func (t *tx) startSubscription(c *customer, order checkoutOrder, paymentMethodID string) (*subscription, *invoice) {
	sub := t.newSubscription(c, order, paymentMethodID)
	inv := t.newInvoice(sub, c, "subscription_create", sub.Created, sub.Created)
	sub.LatestInvoice = nullString(inv.ID)
	t.emit("customer.subscription.created", sub)
	if due, ok := t.trialReminderDue(sub); ok && due <= sub.Created {
		t.remindTrialEnd(sub)
	}
	t.emit("invoice.created", inv)
	t.finalizeInvoice(inv, c)
	t.collectInvoice(inv, c, paymentMethodID, false)
	if sub.Status == "incomplete" {
		t.setSubscriptionStatus(sub, "active")
	}
	return sub, inv
}

func checkoutEmail(s *checkoutSession, c *customer, entry cardEntry) string {
	switch {
	case c != nil && c.Email != "":
		return string(c.Email)
	case s.CustomerEmail != "":
		return string(s.CustomerEmail)
	}
	return strings.TrimSpace(entry.email)
}

func (t *tx) attachCard(c *customer, vc validCard, name, email string) *paymentMethod {
	pm := &paymentMethod{
		ID:             newID("pm"),
		Object:         "payment_method",
		AllowRedisplay: "unspecified",
		BillingDetails: billingDetails{Email: nullString(email), Name: nullString(name)},
		Card: card{
			Brand:        vc.brand,
			Checks:       cardChecks{CVCCheck: "pass"},
			Country:      "US",
			DisplayBrand: vc.brand,
			ExpMonth:     vc.expMonth,
			ExpYear:      vc.expYear,
			Fingerprint:  vc.fingerprint,
			Funding:      "credit",
			Last4:        vc.last4,
			Networks:     cardNetworks{Available: []string{vc.brand}},
		},
		Created:  t.clockNow(c.TestClock),
		Customer: nullString(c.ID),
		Metadata: map[string]string{},
		Type:     "card",
	}
	if vc.declinesRenewal {
		t.acct.RenewalDeclines[pm.ID] = true
	}
	t.acct.PaymentMethods = append(t.acct.PaymentMethods, pm)
	t.emit("payment_method.attached", pm)
	return pm
}

func successRedirect(s *checkoutSession) string {
	return strings.ReplaceAll(string(s.SuccessURL), "{CHECKOUT_SESSION_ID}", s.ID)
}
