package stripe

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWebhookDeliveryIsRetriedUntilEndpointAccepts(t *testing.T) {
	receiver := newHookReceiver(t, 2)
	env := newTestEnv(t, withWebhook(receiver.URL))

	created := env.ok(http.MethodPost, "/v1/customers", url.Values{"email": {"ada@example.com"}})

	hooks := receiver.waitFor(t, 3)
	for i, hook := range hooks {
		e := verifiedEvent(t, hook, hookSecret)
		if e["type"] != "customer.created" || dig(e, "data", "object", "id") != created["id"] {
			t.Errorf("attempt %d delivered %v for %v, want customer.created for %v", i+1, e["type"], dig(e, "data", "object", "id"), created["id"])
		}
		if e["api_version"] != defaultAPIVersion {
			t.Errorf("attempt %d api_version = %v, want %s", i+1, e["api_version"], defaultAPIVersion)
		}
	}
}

func TestWebhookEndpointFromAPIReturnsSecretOnlyAtCreation(t *testing.T) {
	receiver := newHookReceiver(t, 0)
	env := newTestEnv(t)
	ep := env.ok(http.MethodPost, "/v1/webhook_endpoints", url.Values{
		"url":              {receiver.URL},
		"enabled_events[]": {"customer.created"},
	})
	secret, _ := ep["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret = %q, want whsec_...", secret)
	}
	if _, shown := env.ok(http.MethodGet, "/v1/webhook_endpoints/"+ep["id"].(string), nil)["secret"]; shown {
		t.Errorf("retrieve shows the secret; Stripe returns it only at creation")
	}

	env.ok(http.MethodPost, "/v1/customers", nil)
	env.ok(http.MethodPost, "/v1/products", url.Values{"name": {"Not subscribed"}})

	hooks := receiver.waitFor(t, 1)
	if e := verifiedEvent(t, hooks[0], secret); e["type"] != "customer.created" {
		t.Errorf("delivered %v, want the enabled customer.created", e["type"])
	}
	products := env.ok(http.MethodGet, "/v1/events", url.Values{"type": {"product.created"}})["data"].([]any)
	if got := dig(products[0], "pending_webhooks"); got != float64(0) {
		t.Errorf("product.created pending_webhooks = %v, want 0 because the endpoint did not enable it", got)
	}
}

func TestAdminRedeliversEvent(t *testing.T) {
	receiver := newHookReceiver(t, 0)
	env := newTestEnv(t, withWebhook(receiver.URL))
	env.ok(http.MethodPost, "/v1/customers", nil)
	first := verifiedEvent(t, receiver.waitFor(t, 1)[0], hookSecret)

	resp := env.postPage(env.web.URL+"/_sim/events/"+first["id"].(string)+"/redeliver", nil)

	if resp.status != http.StatusOK {
		t.Fatalf("redeliver answered %d, want 200", resp.status)
	}
	again := verifiedEvent(t, receiver.waitFor(t, 2)[1], hookSecret)
	if again["id"] != first["id"] {
		t.Errorf("redelivered event %v, want %v", again["id"], first["id"])
	}
}
