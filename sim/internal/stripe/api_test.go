package stripe

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRequestsWithoutAcceptedKeyAreRejected(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		name          string
		authorization string
		wantMessage   string
	}{
		{"no key", "", "You did not provide an API key."},
		{"unknown bearer key", "Bearer sk_test_wrong_key_1234", "Invalid API Key provided: sk_test_**********1234"},
		{"unknown basic auth key", basicAuth("sk_test_wrong_key_1234"), "Invalid API Key provided: sk_test_**********1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := rawGet(t, env.api.URL+"/v1/customers", tc.authorization)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if got := dig(body, "error", "type"); got != "invalid_request_error" {
				t.Errorf("error.type = %v, want invalid_request_error", got)
			}
			if got, _ := dig(body, "error", "message").(string); !strings.HasPrefix(got, tc.wantMessage) {
				t.Errorf("error.message = %q, want prefix %q", got, tc.wantMessage)
			}
		})
	}
}

func TestBasicAuthWithSecretKeyAsUsernameIsAccepted(t *testing.T) {
	env := newTestEnv(t)
	status, body := rawGet(t, env.api.URL+"/v1/customers", basicAuth(testKey))
	if status != http.StatusOK || body["object"] != "list" {
		t.Fatalf("status = %d, body = %v; want 200 with a list", status, body)
	}
}

func TestMissingObjectAnswersResourceMissing(t *testing.T) {
	env := newTestEnv(t)
	resp := env.request(http.MethodGet, "/v1/customers/cus_missing", nil, nil)
	want := map[string]any{
		"type":    "invalid_request_error",
		"code":    "resource_missing",
		"param":   "id",
		"message": "No such customer: 'cus_missing'",
	}
	if resp.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.status)
	}
	for k, v := range want {
		if got := dig(resp.body, "error", k); got != v {
			t.Errorf("error.%s = %v, want %v", k, got, v)
		}
	}
}

func TestNestedFormParametersAreDecoded(t *testing.T) {
	env := newTestEnv(t)
	c := env.ok(http.MethodPost, "/v1/customers", url.Values{
		"email":                {"ada@example.com"},
		"metadata[account_id]": {"acct_42"},
	})
	if got := dig(c, "metadata", "account_id"); got != "acct_42" {
		t.Fatalf("customer metadata.account_id = %v, want acct_42", got)
	}

	session := env.createSession(url.Values{
		"customer":                {c["id"].(string)},
		"line_items[0][quantity]": {"3"},
		"metadata[order]":         {"order-7"},
		"subscription_data[metadata][account_id]": {"acct_42"},
		"expand[]": {"customer"},
	})
	if got := session["amount_total"]; got != float64(6000) {
		t.Errorf("amount_total = %v, want 6000 for quantity 3 at 2000", got)
	}
	if got := dig(session, "metadata", "order"); got != "order-7" {
		t.Errorf("session metadata.order = %v, want order-7", got)
	}
	if got := dig(session, "customer", "email"); got != "ada@example.com" {
		t.Errorf("expand[]=customer gave customer.email = %v, want the customer object", got)
	}

	env.submitCheckout(session["url"].(string), "4242424242424242", nil)
	completed := env.ok(http.MethodGet, "/v1/checkout/sessions/"+session["id"].(string), nil)
	sub := env.ok(http.MethodGet, "/v1/subscriptions/"+completed["subscription"].(string), url.Values{
		"expand[0]": {"customer"},
		"expand[1]": {"latest_invoice.payment_intent"},
	})
	if got := dig(sub, "metadata", "account_id"); got != "acct_42" {
		t.Errorf("subscription metadata.account_id = %v, want acct_42 from subscription_data[metadata]", got)
	}
	if got := dig(sub, "customer", "id"); got != c["id"] {
		t.Errorf("expand[0]=customer gave customer.id = %v, want %v", got, c["id"])
	}
	if got := dig(sub, "latest_invoice", "payment_intent", "status"); got != "succeeded" {
		t.Errorf("latest_invoice.payment_intent.status = %v, want succeeded", got)
	}
}

func TestIdempotencyKeyReplaysFirstResponse(t *testing.T) {
	env := newTestEnv(t)
	header := http.Header{"Idempotency-Key": {"create-ada"}}
	form := url.Values{"email": {"ada@example.com"}}

	first := env.request(http.MethodPost, "/v1/customers", form, header)
	second := env.request(http.MethodPost, "/v1/customers", form, header)

	if first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 200 twice", first.status, second.status)
	}
	if first.body["id"] != second.body["id"] {
		t.Errorf("replayed id = %v, want the first id %v", second.body["id"], first.body["id"])
	}
	if got := second.header.Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("Idempotent-Replayed = %q, want true", got)
	}
	list := env.ok(http.MethodGet, "/v1/customers", nil)
	if n := len(list["data"].([]any)); n != 1 {
		t.Errorf("customers = %d, want 1", n)
	}
}

func TestIdempotencyKeyReusedWithDifferentParametersIsRejected(t *testing.T) {
	env := newTestEnv(t)
	header := http.Header{"Idempotency-Key": {"create-customer"}}
	env.request(http.MethodPost, "/v1/customers", url.Values{"email": {"ada@example.com"}}, header)

	resp := env.request(http.MethodPost, "/v1/customers", url.Values{"email": {"grace@example.com"}}, header)

	if resp.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.status)
	}
	if got := dig(resp.body, "error", "type"); got != "idempotency_error" {
		t.Errorf("error.type = %v, want idempotency_error", got)
	}
}

func TestListsPageNewestFirstWithCursors(t *testing.T) {
	env := newTestEnv(t)
	var ids []string
	for range 5 {
		ids = append(ids, env.ok(http.MethodPost, "/v1/customers", nil)["id"].(string))
	}
	cases := []struct {
		name        string
		query       url.Values
		wantIDs     []string
		wantHasMore bool
	}{
		{"first page", url.Values{"limit": {"2"}}, []string{ids[4], ids[3]}, true},
		{"next page", url.Values{"limit": {"2"}, "starting_after": {ids[3]}}, []string{ids[2], ids[1]}, true},
		{"last page", url.Values{"limit": {"2"}, "starting_after": {ids[1]}}, []string{ids[0]}, false},
		{"previous page", url.Values{"limit": {"2"}, "ending_before": {ids[2]}}, []string{ids[4], ids[3]}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := env.ok(http.MethodGet, "/v1/customers", tc.query)
			var got []string
			for _, item := range page["data"].([]any) {
				got = append(got, item.(map[string]any)["id"].(string))
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
			if page["has_more"] != tc.wantHasMore {
				t.Errorf("has_more = %v, want %v", page["has_more"], tc.wantHasMore)
			}
			if page["object"] != "list" || page["url"] != "/v1/customers" {
				t.Errorf("object, url = %v, %v; want list, /v1/customers", page["object"], page["url"])
			}
		})
	}
}

func basicAuth(user string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"))
}

func rawGet(t *testing.T, target, authorization string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, body
}
