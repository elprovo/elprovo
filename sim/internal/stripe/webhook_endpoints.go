package stripe

import (
	"net/http"
	"net/url"
	"slices"
	"time"
)

type endpointFields struct {
	url           *string
	enabledEvents []string
	description   *string
	apiVersion    string
	metadata      metadataUpdate
	disabled      *bool
}

func (t *tx) createWebhookEndpoint(in endpointFields) (*webhookEndpoint, error) {
	if in.url == nil {
		return nil, missingParam("url")
	}
	if len(in.enabledEvents) == 0 {
		return nil, missingParam("enabled_events")
	}
	if err := validateEndpointURL(*in.url); err != nil {
		return nil, err
	}
	ep := newWebhookEndpoint(*in.url, "whsec_"+randomString(32), in.enabledEvents, t.now)
	ep.APIVersion = nullString(in.apiVersion)
	setIfSent(&ep.Description, in.description)
	ep.Metadata = in.metadata.apply(nil)
	t.acct.WebhookEndpoints = append(t.acct.WebhookEndpoints, ep)
	return ep, nil
}

func (t *tx) updateWebhookEndpoint(id string, in endpointFields) (*webhookEndpoint, error) {
	ep := find(t.acct.WebhookEndpoints, id)
	if ep == nil {
		return nil, noSuch("webhook endpoint", id, "id")
	}
	if in.url != nil {
		if err := validateEndpointURL(*in.url); err != nil {
			return nil, err
		}
		ep.URL = *in.url
	}
	if len(in.enabledEvents) > 0 {
		ep.EnabledEvents = in.enabledEvents
	}
	setIfSent(&ep.Description, in.description)
	ep.Metadata = in.metadata.apply(ep.Metadata)
	if in.disabled != nil {
		ep.Status = "enabled"
		if *in.disabled {
			ep.Status = "disabled"
		}
	}
	return ep, nil
}

func (t *tx) deleteWebhookEndpoint(id string) (*deletedObject, error) {
	for i, ep := range t.acct.WebhookEndpoints {
		if ep.ID == id {
			t.acct.WebhookEndpoints = append(t.acct.WebhookEndpoints[:i], t.acct.WebhookEndpoints[i+1:]...)
			return &deletedObject{ID: id, Object: "webhook_endpoint", Deleted: true}, nil
		}
	}
	return nil, noSuch("webhook endpoint", id, "id")
}

func validateEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return newAPIError(http.StatusBadRequest, "invalid_request_error", "url_invalid", "url", "Invalid URL: "+raw)
	}
	return nil
}

func newWebhookEndpoint(rawURL, secret string, events []string, now time.Time) *webhookEndpoint {
	return &webhookEndpoint{
		ID:            newID("we"),
		Object:        "webhook_endpoint",
		Created:       now.Unix(),
		EnabledEvents: events,
		Metadata:      map[string]string{},
		Secret:        secret,
		Status:        "enabled",
		URL:           rawURL,
	}
}

const flagEndpointMarker = "elprovo_sim_flag"

func syncFlagEndpoints(a *account, hooks []webhookConfig, now time.Time) {
	fromFlags := map[string]*webhookEndpoint{}
	kept := a.WebhookEndpoints[:0]
	for _, ep := range a.WebhookEndpoints {
		isFlagged := ep.Metadata[flagEndpointMarker] != ""
		if isFlagged && !slices.ContainsFunc(hooks, func(h webhookConfig) bool { return h.url == ep.URL }) {
			continue
		}
		if isFlagged {
			fromFlags[ep.URL] = ep
		}
		kept = append(kept, ep)
	}
	a.WebhookEndpoints = kept
	for _, hook := range hooks {
		ep := fromFlags[hook.url]
		if ep == nil {
			ep = newWebhookEndpoint(hook.url, hook.secret, []string{"*"}, now)
			ep.Metadata[flagEndpointMarker] = "true"
			a.WebhookEndpoints = append(a.WebhookEndpoints, ep)
			fromFlags[hook.url] = ep
		}
		ep.Secret = hook.secret
		ep.Status = "enabled"
	}
}

func (e webhookEndpoint) withoutSecret() *webhookEndpoint {
	e.Secret = ""
	return &e
}
