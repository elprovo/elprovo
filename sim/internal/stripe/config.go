package stripe

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	defaultSecretKey  = "sk_test_stagely"
	defaultAPIVersion = "2026-09-30.endive"
)

type config struct {
	enabled         bool
	apiAddr         string
	webAddr         string
	webURL          string
	secretKeys      []string
	apiVersion      string
	plan            planConfig
	webhooks        []webhookConfig
	markUnpaid      bool
	deliveryRetries []time.Duration
}

type planConfig struct {
	name     string
	amount   int64
	currency string
	interval string
}

type webhookConfig struct {
	url    string
	secret string
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func registerFlags(fs *flag.FlagSet, cfg *config, keys, hooks *repeatedFlag) {
	fs.BoolVar(&cfg.enabled, "stripe", false, "run the Stripe simulation")
	fs.StringVar(&cfg.apiAddr, "stripe-api-addr", ":9200", "listen address of the simulated Stripe API (api.stripe.com)")
	fs.StringVar(&cfg.webAddr, "stripe-web-addr", ":9201", "listen address of the hosted Checkout, billing portal and invoice pages")
	fs.StringVar(&cfg.webURL, "stripe-web-url", "http://localhost:9201", "public base URL of the hosted pages, used in Checkout and portal session URLs")
	fs.Var(keys, "stripe-secret-key", "API secret key that the API accepts, sk_test_...; repeatable (default "+defaultSecretKey+")")
	fs.StringVar(&cfg.apiVersion, "stripe-api-version", defaultAPIVersion, "Stripe API version written into events")
	fs.StringVar(&cfg.plan.name, "stripe-plan-name", "Team Stagelet", "name of the seeded product "+seedProductID)
	fs.Int64Var(&cfg.plan.amount, "stripe-plan-amount", 2000, "amount of the seeded price "+seedPriceID+", in the smallest currency unit")
	fs.StringVar(&cfg.plan.currency, "stripe-plan-currency", "usd", "currency of the seeded price")
	fs.StringVar(&cfg.plan.interval, "stripe-plan-interval", "month", "billing interval of the seeded price: day, week, month or year")
	fs.Var(hooks, "stripe-webhook", "webhook endpoint that receives every event, as url=...,secret=whsec_...; repeatable")
	fs.BoolVar(&cfg.markUnpaid, "stripe-mark-unpaid", false, "mark a subscription unpaid instead of canceling it when every payment retry fails")
}

func (c *config) finish(keys, hooks repeatedFlag) error {
	c.secretKeys = keys
	if len(c.secretKeys) == 0 {
		c.secretKeys = []string{defaultSecretKey}
	}
	for _, k := range c.secretKeys {
		if !strings.HasPrefix(k, "sk_test_") {
			return fmt.Errorf("stripe-secret-key %q must start with sk_test_", k)
		}
	}
	for _, h := range hooks {
		hook, err := parseWebhookFlag(h)
		if err != nil {
			return err
		}
		c.webhooks = append(c.webhooks, hook)
	}
	c.webURL = strings.TrimSuffix(c.webURL, "/")
	return c.validate()
}

func (c *config) validate() error {
	if u, err := url.Parse(c.webURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("stripe-web-url %q must be an absolute http or https URL", c.webURL)
	}
	switch {
	case c.apiVersion == "":
		return errors.New("stripe-api-version must not be empty")
	case c.plan.name == "":
		return errors.New("stripe-plan-name must not be empty")
	case c.plan.amount < 0:
		return errors.New("stripe-plan-amount must not be negative")
	case len(c.plan.currency) != 3 || strings.ToLower(c.plan.currency) != c.plan.currency:
		return fmt.Errorf("stripe-plan-currency %q must be a lowercase three-letter ISO code", c.plan.currency)
	case !validInterval(c.plan.interval):
		return fmt.Errorf("stripe-plan-interval %q must be day, week, month or year", c.plan.interval)
	}
	return nil
}

func parseWebhookFlag(value string) (webhookConfig, error) {
	var hook webhookConfig
	for part := range strings.SplitSeq(value, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "url":
			hook.url = v
		case "secret":
			hook.secret = v
		default:
			return hook, fmt.Errorf("stripe-webhook %q: unknown field %q; the format is url=<url>,secret=whsec_<secret>", value, k)
		}
	}
	if err := validateEndpointURL(hook.url); err != nil {
		return hook, fmt.Errorf("stripe-webhook %q: url must be an absolute http or https URL", value)
	}
	if !strings.HasPrefix(hook.secret, "whsec_") {
		return hook, fmt.Errorf("stripe-webhook %q: secret must start with whsec_", value)
	}
	return hook, nil
}

func (c *config) acceptsKey(key string) bool {
	return slices.Contains(c.secretKeys, key)
}
