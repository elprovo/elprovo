package stripe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	maxRequestBody       = 1 << 20
	maxIdempotencyKeyLen = 255
)

type apiRequest struct {
	http   *http.Request
	params *params
	info   requestInfo
}

type apiHandler func(req *apiRequest) (any, error)

func (s *Simulation) apiHandler() http.Handler {
	mux := http.NewServeMux()
	routes := map[string]apiHandler{
		"POST /v1/customers":      s.createCustomer,
		"GET /v1/customers":       s.listCustomers,
		"GET /v1/customers/{id}":  s.getCustomer,
		"POST /v1/customers/{id}": s.updateCustomer,

		"POST /v1/products":     s.createProduct,
		"GET /v1/products":      s.listProducts,
		"GET /v1/products/{id}": s.getProduct,
		"POST /v1/prices":       s.createPrice,
		"GET /v1/prices":        s.listPrices,
		"GET /v1/prices/{id}":   s.getPrice,

		"POST /v1/checkout/sessions":             s.createCheckoutSession,
		"GET /v1/checkout/sessions":              s.listCheckoutSessions,
		"GET /v1/checkout/sessions/{id}":         s.getCheckoutSession,
		"POST /v1/checkout/sessions/{id}/expire": s.expireCheckoutSession,

		"GET /v1/subscriptions":         s.listSubscriptions,
		"GET /v1/subscriptions/{id}":    s.getSubscription,
		"POST /v1/subscriptions/{id}":   s.updateSubscription,
		"DELETE /v1/subscriptions/{id}": s.cancelSubscription,

		"GET /v1/invoices":             s.listInvoices,
		"GET /v1/invoices/{id}":        s.getInvoice,
		"GET /v1/payment_intents/{id}": s.getPaymentIntent,
		"GET /v1/payment_methods/{id}": s.getPaymentMethod,

		"POST /v1/billing_portal/sessions": s.createPortalSession,

		"POST /v1/test_helpers/test_clocks":              s.createTestClock,
		"GET /v1/test_helpers/test_clocks":               s.listTestClocks,
		"GET /v1/test_helpers/test_clocks/{id}":          s.getTestClock,
		"POST /v1/test_helpers/test_clocks/{id}/advance": s.advanceTestClock,

		"GET /v1/events":      s.listEvents,
		"GET /v1/events/{id}": s.getEvent,

		"POST /v1/webhook_endpoints":        s.createWebhookEndpoint,
		"GET /v1/webhook_endpoints":         s.listWebhookEndpoints,
		"GET /v1/webhook_endpoints/{id}":    s.getWebhookEndpoint,
		"POST /v1/webhook_endpoints/{id}":   s.updateWebhookEndpoint,
		"DELETE /v1/webhook_endpoints/{id}": s.deleteWebhookEndpoint,
	}
	for pattern, h := range routes {
		mux.Handle(pattern, s.apiEndpoint(h))
	}
	mux.Handle("/", s.apiEndpoint(unrecognizedURL))
	return s.authenticate(mux)
}

func unrecognizedURL(req *apiRequest) (any, error) {
	return nil, newAPIError(http.StatusNotFound, "invalid_request_error", "", "",
		fmt.Sprintf("Unrecognized request URL (%s: %s). Please see https://stripe.com/docs or we can help at https://support.stripe.com/.",
			req.http.Method, req.http.URL.Path))
}

func (s *Simulation) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := apiKey(r)
		if key != "" && s.cfg.acceptsKey(key) {
			next.ServeHTTP(w, r)
			return
		}
		message := "You did not provide an API key. You need to provide your API key in the Authorization header, " +
			"using Bearer auth (e.g. 'Authorization: Bearer YOUR_SECRET_KEY'). " +
			"See https://stripe.com/docs/api#authentication for details, or we can help at https://support.stripe.com/."
		if key != "" {
			message = "Invalid API Key provided: " + redactKey(key)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Stripe"`)
		writeJSON(w, http.StatusUnauthorized, errorBody(newAPIError(http.StatusUnauthorized, "invalid_request_error", "", "", message)))
	})
}

func apiKey(r *http.Request) string {
	if user, _, ok := r.BasicAuth(); ok {
		return user
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

func redactKey(key string) string {
	const shown = 4
	if len(key) <= len("sk_test_")+shown {
		return strings.Repeat("*", len(key))
	}
	prefix := key[:len("sk_test_")]
	return prefix + strings.Repeat("*", len(key)-len(prefix)-shown) + key[len(key)-shown:]
}

func (s *Simulation) apiEndpoint(h apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := "req_" + randomString(14)
		w.Header().Set("Request-Id", requestID)
		w.Header().Set("Stripe-Version", s.versionOf(r))
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(invalidRequest("", "Invalid request body: %v", err)))
			return
		}
		p := parseParams(r.Form)
		req := &apiRequest{http: r, params: p, info: requestInfo{id: requestID, expand: p.strings("expand")}}
		if r.Method == http.MethodPost {
			req.info.idempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		}
		status, body, replayed := s.respond(req, h)
		if req.info.idempotencyKey != "" {
			w.Header().Set("Idempotency-Key", req.info.idempotencyKey)
		}
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeRaw(w, status, body)
	})
}

func (s *Simulation) versionOf(r *http.Request) string {
	if v := r.Header.Get("Stripe-Version"); v != "" {
		return v
	}
	return s.cfg.apiVersion
}

func (s *Simulation) respond(req *apiRequest, h apiHandler) (status int, body []byte, replayed bool) {
	if err := validateExpand(req.info.expand); err != nil {
		status, body = s.errorResponse(err)
		return status, body, false
	}
	key := req.info.idempotencyKey
	if key == "" {
		status, body = s.execute(req, h)
		return status, body, false
	}
	if len(key) > maxIdempotencyKeyLen {
		status, body = s.errorResponse(invalidRequest("", "Idempotency keys can be at most %d characters long.", maxIdempotencyKeyLen))
		return status, body, false
	}
	if !s.inflight.acquire(key) {
		status, body = s.errorResponse(idempotencyError(http.StatusConflict, "idempotency_key_in_use", fmt.Sprintf(
			"There is currently another in-progress request using this Idempotent Key (that probably means you submitted twice, "+
				"and the other request is still going through): %s. Please try again later.", key)))
		return status, body, false
	}
	defer s.inflight.release(key)

	endpoint := req.http.Method + " " + req.http.URL.Path
	fingerprint := req.http.Form.Encode()
	if prior, conflict := s.priorResponse(key, endpoint, fingerprint); conflict != nil {
		status, body = s.errorResponse(conflict)
		return status, body, false
	} else if prior != nil {
		return prior.Status, prior.Body, true
	}
	status, body = s.execute(req, h)
	if savesIdempotentResult(status) {
		s.saveIdempotentResult(&idempotencyRecord{Key: key, Endpoint: endpoint, Params: fingerprint, Status: status, Body: body})
	}
	return status, body, false
}

func (s *Simulation) priorResponse(key, endpoint, fingerprint string) (*idempotencyRecord, error) {
	var prior *idempotencyRecord
	s.store.Read(func(a *account) {
		if r := a.idempotencyRecord(key, s.now()); r != nil {
			copied := *r
			prior = &copied
		}
	})
	switch {
	case prior == nil:
		return nil, nil
	case prior.Endpoint != endpoint:
		return nil, idempotencyError(http.StatusBadRequest, "", fmt.Sprintf(
			"Keys for idempotent requests can only be used for the same endpoint they were first used for ('%s' vs '%s'). "+
				"Try using a key other than '%s' if you meant to execute a different request.", prior.Endpoint, endpoint, key))
	case prior.Params != fingerprint:
		return nil, idempotencyError(http.StatusBadRequest, "", fmt.Sprintf(
			"Keys for idempotent requests can only be used with the same parameters they were first used with. "+
				"Try using a key other than '%s' if you meant to execute a different request.", key))
	}
	return prior, nil
}

func savesIdempotentResult(status int) bool {
	return status < http.StatusBadRequest || status == http.StatusPaymentRequired || status >= http.StatusInternalServerError
}

func (s *Simulation) saveIdempotentResult(record *idempotencyRecord) {
	now := s.now()
	record.Created = now.Unix()
	err := s.store.Update(func(a *account) error {
		a.pruneIdempotencyKeys(now)
		a.IdempotencyKeys = append(a.IdempotencyKeys, record)
		return nil
	})
	if err != nil {
		s.log.Error("save idempotent result", "error", err)
	}
}

func (s *Simulation) execute(req *apiRequest, h apiHandler) (int, []byte) {
	out, err := h(req)
	if err != nil {
		return s.errorResponse(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return s.errorResponse(err)
	}
	return http.StatusOK, body
}

func (s *Simulation) errorResponse(err error) (int, []byte) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		s.log.Error("stripe api request failed", "error", err)
		apiErr = newAPIError(http.StatusInternalServerError, "api_error", "", "", "An unknown error occurred.")
	}
	body, _ := json.Marshal(errorBody(apiErr))
	return apiErr.status, body
}

func errorBody(e *apiError) map[string]*apiError {
	return map[string]*apiError{"error": e}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
