package stripe

import (
	"fmt"
	"slices"
	"strings"
)

func getByID[P interface {
	*T
	identified
}, T any](s *Simulation, req *apiRequest, kind string, items func(a *account) []P) (any, error) {
	id := req.http.PathValue("id")
	return s.read(req.info.expand, func(a *account) (any, error) {
		if obj := find(items(a), id); obj != nil {
			return obj, nil
		}
		return nil, noSuch(kind, id, "id")
	})
}

func customerFieldsFrom(p *params) customerFields {
	return customerFields{
		email:                p.optional("email"),
		name:                 p.optional("name"),
		description:          p.optional("description"),
		phone:                p.optional("phone"),
		metadata:             p.metadata("metadata"),
		defaultPaymentMethod: p.get("invoice_settings").optional("default_payment_method"),
		testClock:            p.str("test_clock"),
	}
}

func (s *Simulation) createCustomer(req *apiRequest) (any, error) {
	in := customerFieldsFrom(req.params)
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createCustomer(in) })
}

func (s *Simulation) updateCustomer(req *apiRequest) (any, error) {
	id, in := req.http.PathValue("id"), customerFieldsFrom(req.params)
	return s.mutate(req.info, func(t *tx) (any, error) { return t.updateCustomer(id, in) })
}

func (s *Simulation) getCustomer(req *apiRequest) (any, error) {
	return getByID(s, req, "customer", func(a *account) []*customer { return a.Customers })
}

func (s *Simulation) listCustomers(req *apiRequest) (any, error) {
	email, clock := req.params.str("email"), req.params.str("test_clock")
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Customers, func(c *customer) bool {
			return (email == "" || string(c.Email) == email) && (clock == "" || string(c.TestClock) == clock)
		}, req.params, "/v1/customers")
	})
}

func activeParam(p *params) (bool, error) {
	active, set, err := p.boolean("active")
	return active || !set, err
}

func (s *Simulation) createProduct(req *apiRequest) (any, error) {
	active, err := activeParam(req.params)
	if err != nil {
		return nil, err
	}
	in := productFields{
		name:        req.params.str("name"),
		description: req.params.str("description"),
		active:      active,
		metadata:    req.params.metadata("metadata"),
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createProduct(in) })
}

func (s *Simulation) getProduct(req *apiRequest) (any, error) {
	return getByID(s, req, "product", func(a *account) []*product { return a.Products })
}

func (s *Simulation) listProducts(req *apiRequest) (any, error) {
	active, filter, err := req.params.boolean("active")
	if err != nil {
		return nil, err
	}
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Products, func(p *product) bool { return !filter || p.Active == active }, req.params, "/v1/products")
	})
}

func (s *Simulation) createPrice(req *apiRequest) (any, error) {
	p := req.params
	amount, set, err := p.integer("unit_amount")
	if err != nil {
		return nil, err
	}
	if !set {
		return nil, missingParam("unit_amount")
	}
	intervalCount, _, err := p.get("recurring").integer("interval_count")
	if err != nil {
		return nil, err
	}
	active, err := activeParam(p)
	if err != nil {
		return nil, err
	}
	in := priceFields{
		product:       p.str("product"),
		productName:   p.get("product_data").str("name"),
		unitAmount:    amount,
		currency:      strings.ToLower(p.str("currency")),
		interval:      p.get("recurring").str("interval"),
		intervalCount: intervalCount,
		nickname:      p.str("nickname"),
		lookupKey:     p.str("lookup_key"),
		active:        active,
		metadata:      p.metadata("metadata"),
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createPrice(in) })
}

func (s *Simulation) getPrice(req *apiRequest) (any, error) {
	return getByID(s, req, "price", func(a *account) []*price { return a.Prices })
}

func (s *Simulation) listPrices(req *apiRequest) (any, error) {
	p := req.params
	active, filterActive, err := p.boolean("active")
	if err != nil {
		return nil, err
	}
	productID, priceType, lookupKeys := p.str("product"), p.str("type"), p.strings("lookup_keys")
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Prices, func(pr *price) bool {
			return (!filterActive || pr.Active == active) &&
				(productID == "" || pr.Product == productID) &&
				(priceType == "" || pr.Type == priceType) &&
				(len(lookupKeys) == 0 || slices.Contains(lookupKeys, string(pr.LookupKey)))
		}, p, "/v1/prices")
	})
}

func checkoutFieldsFrom(p *params) (checkoutFields, error) {
	subscriptionData := p.get("subscription_data")
	in := checkoutFields{
		mode:                    p.str("mode"),
		customer:                p.str("customer"),
		customerEmail:           p.str("customer_email"),
		successURL:              p.str("success_url"),
		cancelURL:               p.str("cancel_url"),
		clientReferenceID:       p.str("client_reference_id"),
		metadata:                p.metadata("metadata").apply(nil),
		subscriptionMetadata:    subscriptionData.metadata("metadata").apply(nil),
		paymentMethodCollection: p.str("payment_method_collection"),
		missingPaymentMethod:    subscriptionData.get("trial_settings").get("end_behavior").str("missing_payment_method"),
	}
	expiresAt, _, err := p.integer("expires_at")
	if err != nil {
		return in, err
	}
	in.expiresAt = expiresAt
	trialDays, set, err := subscriptionData.integer("trial_period_days")
	if err != nil {
		return in, invalidInteger("subscription_data[trial_period_days]", subscriptionData.str("trial_period_days"))
	}
	if set {
		in.trialPeriodDays = &trialDays
	}
	for i, item := range p.get("line_items").list() {
		quantity, _, err := item.integer("quantity")
		if err != nil {
			return in, invalidInteger(fmt.Sprintf("line_items[%d][quantity]", i), item.str("quantity"))
		}
		in.lineItems = append(in.lineItems, lineItem{Price: item.str("price"), Quantity: quantity})
	}
	return in, nil
}

func (s *Simulation) createCheckoutSession(req *apiRequest) (any, error) {
	in, err := checkoutFieldsFrom(req.params)
	if err != nil {
		return nil, err
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createCheckoutSession(in) })
}

func (s *Simulation) getCheckoutSession(req *apiRequest) (any, error) {
	return getByID(s, req, "checkout.session", func(a *account) []*checkoutSession { return a.CheckoutSessions })
}

func (s *Simulation) listCheckoutSessions(req *apiRequest) (any, error) {
	p := req.params
	customerID, subscriptionID, status := p.str("customer"), p.str("subscription"), p.str("status")
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.CheckoutSessions, func(cs *checkoutSession) bool {
			return (customerID == "" || string(cs.Customer) == customerID) &&
				(subscriptionID == "" || string(cs.Subscription) == subscriptionID) &&
				(status == "" || cs.Status == status)
		}, p, "/v1/checkout/sessions")
	})
}

func (s *Simulation) expireCheckoutSession(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	return s.mutate(req.info, func(t *tx) (any, error) { return t.expireCheckoutSession(id) })
}

func (s *Simulation) getSubscription(req *apiRequest) (any, error) {
	return getByID(s, req, "subscription", func(a *account) []*subscription { return a.Subscriptions })
}

func (s *Simulation) listSubscriptions(req *apiRequest) (any, error) {
	p := req.params
	customerID, status, priceID := p.str("customer"), p.str("status"), p.str("price")
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Subscriptions, func(sub *subscription) bool {
			return (customerID == "" || sub.Customer == customerID) &&
				subscriptionStatusMatches(status, sub.Status) &&
				(priceID == "" || slices.ContainsFunc(sub.Items.Data, func(it subscriptionItem) bool { return it.Price.ID == priceID }))
		}, p, "/v1/subscriptions")
	})
}

func subscriptionStatusMatches(filter, status string) bool {
	switch filter {
	case "":
		return status != "canceled"
	case "all":
		return true
	case "ended":
		return status == "canceled"
	}
	return filter == status
}

func subscriptionFieldsFrom(p *params) (subscriptionFields, error) {
	in := subscriptionFields{metadata: p.metadata("metadata"), prorationBehavior: p.str("proration_behavior")}
	cancel, set, err := p.boolean("cancel_at_period_end")
	if err != nil {
		return in, err
	}
	if set {
		in.cancelAtPeriodEnd = &cancel
	}
	for i, item := range p.get("items").list() {
		u, err := itemUpdateFrom(item, fmt.Sprintf("items[%d]", i))
		if err != nil {
			return in, err
		}
		in.items = append(in.items, u)
	}
	return in, nil
}

func itemUpdateFrom(p *params, param string) (itemUpdate, error) {
	u := itemUpdate{id: p.str("id"), price: p.str("price")}
	quantity, set, err := p.integer("quantity")
	if err != nil {
		return u, invalidInteger(param+"[quantity]", p.str("quantity"))
	}
	if set {
		u.quantity = &quantity
	}
	if u.deleted, _, err = p.boolean("deleted"); err != nil {
		return u, invalidRequest(param+"[deleted]", "Invalid boolean: %s", p.str("deleted"))
	}
	return u, nil
}

func (s *Simulation) updateSubscription(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	in, err := subscriptionFieldsFrom(req.params)
	if err != nil {
		return nil, err
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.updateSubscription(id, in) })
}

func (s *Simulation) cancelSubscription(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	return s.mutate(req.info, func(t *tx) (any, error) { return t.cancelSubscriptionNow(id) })
}

func (s *Simulation) getInvoice(req *apiRequest) (any, error) {
	return getByID(s, req, "invoice", func(a *account) []*invoice { return a.Invoices })
}

func (s *Simulation) listInvoices(req *apiRequest) (any, error) {
	p := req.params
	customerID, subscriptionID, status := p.str("customer"), p.str("subscription"), p.str("status")
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Invoices, func(inv *invoice) bool {
			return (customerID == "" || inv.Customer == customerID) &&
				(subscriptionID == "" || string(inv.Subscription) == subscriptionID) &&
				(status == "" || inv.Status == status)
		}, p, "/v1/invoices")
	})
}

func (s *Simulation) getPaymentIntent(req *apiRequest) (any, error) {
	return getByID(s, req, "payment_intent", func(a *account) []*paymentIntent { return a.PaymentIntents })
}

func (s *Simulation) getPaymentMethod(req *apiRequest) (any, error) {
	return getByID(s, req, "PaymentMethod", func(a *account) []*paymentMethod { return a.PaymentMethods })
}

func (s *Simulation) createPortalSession(req *apiRequest) (any, error) {
	customerID, returnURL := req.params.str("customer"), req.params.str("return_url")
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createPortalSession(customerID, returnURL) })
}

func (s *Simulation) createTestClock(req *apiRequest) (any, error) {
	frozen, _, err := req.params.integer("frozen_time")
	if err != nil {
		return nil, err
	}
	name := req.params.str("name")
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createTestClock(frozen, name) })
}

func (s *Simulation) getTestClock(req *apiRequest) (any, error) {
	return getByID(s, req, "test_clock", func(a *account) []*testClock { return a.TestClocks })
}

func (s *Simulation) listTestClocks(req *apiRequest) (any, error) {
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.TestClocks, nil, req.params, "/v1/test_helpers/test_clocks")
	})
}

func (s *Simulation) advanceTestClock(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	frozen, _, err := req.params.integer("frozen_time")
	if err != nil {
		return nil, err
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.advanceTestClock(id, frozen) })
}

func (s *Simulation) getEvent(req *apiRequest) (any, error) {
	return getByID(s, req, "event", func(a *account) []*event { return a.Events })
}

func (s *Simulation) listEvents(req *apiRequest) (any, error) {
	patterns := req.params.strings("types")
	if t := req.params.str("type"); t != "" {
		patterns = append(patterns, t)
	}
	return s.read(req.info.expand, func(a *account) (any, error) {
		return paginate(a.Events, func(e *event) bool {
			return len(patterns) == 0 || slices.ContainsFunc(patterns, func(p string) bool { return eventTypeMatches(p, e.Type) })
		}, req.params, "/v1/events")
	})
}

func eventTypeMatches(pattern, eventType string) bool {
	if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
		return strings.HasPrefix(eventType, prefix)
	}
	return pattern == eventType
}

func endpointFieldsFrom(p *params) (endpointFields, error) {
	in := endpointFields{
		url:           p.optional("url"),
		enabledEvents: p.strings("enabled_events"),
		description:   p.optional("description"),
		apiVersion:    p.str("api_version"),
		metadata:      p.metadata("metadata"),
	}
	disabled, set, err := p.boolean("disabled")
	if set {
		in.disabled = &disabled
	}
	return in, err
}

func (s *Simulation) createWebhookEndpoint(req *apiRequest) (any, error) {
	in, err := endpointFieldsFrom(req.params)
	if err != nil {
		return nil, err
	}
	return s.mutate(req.info, func(t *tx) (any, error) { return t.createWebhookEndpoint(in) })
}

func (s *Simulation) updateWebhookEndpoint(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	in, err := endpointFieldsFrom(req.params)
	if err != nil {
		return nil, err
	}
	return s.mutate(req.info, func(t *tx) (any, error) {
		ep, err := t.updateWebhookEndpoint(id, in)
		if err != nil {
			return nil, err
		}
		return ep.withoutSecret(), nil
	})
}

func (s *Simulation) deleteWebhookEndpoint(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	return s.mutate(req.info, func(t *tx) (any, error) { return t.deleteWebhookEndpoint(id) })
}

func (s *Simulation) getWebhookEndpoint(req *apiRequest) (any, error) {
	id := req.http.PathValue("id")
	return s.read(nil, func(a *account) (any, error) {
		if ep := find(a.WebhookEndpoints, id); ep != nil {
			return ep.withoutSecret(), nil
		}
		return nil, noSuch("webhook endpoint", id, "id")
	})
}

func (s *Simulation) listWebhookEndpoints(req *apiRequest) (any, error) {
	return s.read(nil, func(a *account) (any, error) {
		page, err := paginate(a.WebhookEndpoints, nil, req.params, "/v1/webhook_endpoints")
		for i, item := range page.Data {
			page.Data[i] = item.(*webhookEndpoint).withoutSecret()
		}
		return page, err
	})
}
