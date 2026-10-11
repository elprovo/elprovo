package stripe

import (
	"encoding/json"
	"net/http"
)

func (s *Simulation) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /_sim/objects", s.adminObjects)
	mux.HandleFunc("POST /_sim/customers/{id}/fail_next_payment", s.adminFailNextPayment)
	mux.HandleFunc("POST /_sim/events/{id}/redeliver", s.adminRedeliver)
	mux.HandleFunc("POST /_sim/checkout/sessions/{id}/complete", s.adminCompleteCheckout)
}

type objectDump struct {
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
	PortalSessions   []*portalSession   `json:"billing_portal_sessions"`
	TestClocks       []*testClock       `json:"test_clocks"`
}

func (s *Simulation) adminObjects(w http.ResponseWriter, _ *http.Request) {
	var body []byte
	var err error
	s.store.Read(func(a *account) {
		body, err = json.Marshal(objectDump{
			Customers:        a.Customers,
			Products:         a.Products,
			Prices:           a.Prices,
			CheckoutSessions: a.CheckoutSessions,
			Subscriptions:    a.Subscriptions,
			Invoices:         a.Invoices,
			PaymentIntents:   a.PaymentIntents,
			PaymentMethods:   a.PaymentMethods,
			Events:           a.Events,
			WebhookEndpoints: a.WebhookEndpoints,
			PortalSessions:   a.PortalSessions,
			TestClocks:       a.TestClocks,
		})
	})
	if err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

func (s *Simulation) adminFailNextPayment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.mutate(requestInfo{}, func(t *tx) (any, error) {
		if find(t.acct.Customers, id) == nil {
			return nil, noSuch("customer", id, "id")
		}
		t.acct.ArmedFailures[id] = true
		return nil, nil
	})
	if err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"customer": id, "fail_next_payment": true})
}

func (s *Simulation) adminRedeliver(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	eventID, endpointID := r.PathValue("id"), r.FormValue("endpoint")
	var deliveries []delivery
	var err error
	s.store.Read(func(a *account) {
		e := find(a.Events, eventID)
		if e == nil {
			err = noSuch("event", eventID, "id")
			return
		}
		if endpointID != "" && find(a.WebhookEndpoints, endpointID) == nil {
			err = noSuch("webhook endpoint", endpointID, "endpoint")
			return
		}
		for _, ep := range a.WebhookEndpoints {
			if (endpointID == "" || ep.ID == endpointID) && ep.receives(e.Type) {
				deliveries = append(deliveries, delivery{eventID: e.ID, endpointID: ep.ID})
			}
		}
	})
	if err != nil {
		s.writeAdminError(w, err)
		return
	}
	s.hooks.enqueue(deliveries...)
	writeJSON(w, http.StatusOK, map[string]any{"event": eventID, "deliveries": len(deliveries)})
}

func (s *Simulation) adminCompleteCheckout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	id := r.PathValue("id")
	entry := cardEntry{
		number: formValueOr(r, "card", "4242424242424242"),
		expiry: formValueOr(r, "expiry", "12/34"),
		cvc:    formValueOr(r, "cvc", "123"),
		name:   r.FormValue("name"),
		email:  r.FormValue("email"),
	}
	body, err := s.mutate(requestInfo{}, func(t *tx) (any, error) { return t.completeCheckout(id, entry) })
	if err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

func formValueOr(r *http.Request, key, fallback string) string {
	if v := r.FormValue(key); v != "" {
		return v
	}
	return fallback
}

func (s *Simulation) writeAdminError(w http.ResponseWriter, err error) {
	status, body := s.errorResponse(err)
	writeRaw(w, status, body)
}
