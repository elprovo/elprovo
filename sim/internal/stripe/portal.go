package stripe

import "net/http"

const portalConfigurationID = "bpc_default"

func (t *tx) createPortalSession(customerID, returnURL string) (*portalSession, error) {
	if customerID == "" {
		return nil, missingParam("customer")
	}
	if find(t.acct.Customers, customerID) == nil {
		return nil, noSuch("customer", customerID, "customer")
	}
	secret := "test_" + randomString(40)
	ps := &portalSession{
		ID:            newID("bps"),
		Object:        "billing_portal.session",
		Configuration: portalConfigurationID,
		Created:       t.now.Unix(),
		Customer:      customerID,
		ReturnURL:     nullString(returnURL),
		URL:           t.cfg.webURL + "/p/session/" + secret,
	}
	t.acct.PortalSessions = append(t.acct.PortalSessions, ps)
	t.acct.PortalSecrets[secret] = ps.ID
	t.emit("billing_portal.session.created", ps)
	return ps, nil
}

func (a *account) portalSession(secret string) *portalSession {
	return find(a.PortalSessions, a.PortalSecrets[secret])
}

func portalNotFound() *apiError {
	return newAPIError(http.StatusNotFound, "invalid_request_error", "resource_missing", "", "This portal session is not valid.")
}

func (t *tx) portalSetCancelAtPeriodEnd(secret, subscriptionID string, cancel bool) error {
	ps := t.acct.portalSession(secret)
	if ps == nil {
		return portalNotFound()
	}
	sub := find(t.acct.Subscriptions, subscriptionID)
	if sub == nil || sub.Customer != ps.Customer || sub.Status == "canceled" {
		return noSuch("subscription", subscriptionID, "subscription")
	}
	_, err := t.updateSubscription(sub.ID, subscriptionFields{cancelAtPeriodEnd: &cancel})
	return err
}

func (t *tx) portalReplaceCard(secret string, entry cardEntry) error {
	ps := t.acct.portalSession(secret)
	if ps == nil {
		return portalNotFound()
	}
	c := find(t.acct.Customers, ps.Customer)
	vc, err := entry.validate(t.now)
	if err != nil {
		return err
	}
	pm := t.attachCard(c, vc, entry.name, string(c.Email))
	before := snapshot(c)
	c.InvoiceSettings.DefaultPaymentMethod = nullString(pm.ID)
	t.emitUpdate("customer.updated", c, before)
	for _, sub := range t.acct.Subscriptions {
		if sub.Customer != c.ID || sub.Status == "canceled" {
			continue
		}
		before := snapshot(sub)
		sub.DefaultPaymentMethod = nullString(pm.ID)
		t.emitUpdate("customer.subscription.updated", sub, before)
	}
	return nil
}
