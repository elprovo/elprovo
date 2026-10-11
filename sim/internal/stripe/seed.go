package stripe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	seedProductID = "prod_team_stagelet"
	seedPriceID   = "price_team_stagelet"
	seedLookupKey = "team_stagelet"
	seedMarker    = "stagely_sim_seed"
)

type seedPrice struct {
	productID   string
	productName string
	priceID     string
	lookupKey   string
	amount      int64
	currency    string
	interval    string
}

type catalogEntry struct {
	LookupKey string `json:"lookup_key"`
	Product   string `json:"product"`
	Amount    *int64 `json:"amount"`
	Currency  string `json:"currency"`
	Interval  string `json:"interval"`
}

func loadCatalog(path string) ([]seedPrice, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read stripe-catalog: %w", err)
	}
	var entries []catalogEntry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse stripe-catalog %s: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("stripe-catalog %s must list at least one price", path)
	}
	seed, err := catalogSeed(entries)
	if err != nil {
		return nil, fmt.Errorf("stripe-catalog %s: %w", path, err)
	}
	return seed, nil
}

func catalogSeed(entries []catalogEntry) ([]seedPrice, error) {
	seed := make([]seedPrice, 0, len(entries))
	lookupKeys := map[string]string{}
	productNames := map[string]string{}
	for i, e := range entries {
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		s := seedPrice{
			productID:   "prod_" + slug(e.Product),
			productName: e.Product,
			priceID:     "price_" + slug(e.LookupKey),
			lookupKey:   e.LookupKey,
			amount:      *e.Amount,
			currency:    e.Currency,
			interval:    e.Interval,
		}
		switch other, taken := lookupKeys[s.priceID]; {
		case taken && other == e.LookupKey:
			return nil, fmt.Errorf("entry %d: lookup_key %q is used twice", i, e.LookupKey)
		case taken:
			return nil, fmt.Errorf("entry %d: lookup keys %q and %q both give the price ID %s", i, other, e.LookupKey, s.priceID)
		}
		if other, taken := productNames[s.productID]; taken && other != e.Product {
			return nil, fmt.Errorf("entry %d: products %q and %q both give the product ID %s", i, other, e.Product, s.productID)
		}
		lookupKeys[s.priceID], productNames[s.productID] = e.LookupKey, e.Product
		seed = append(seed, s)
	}
	return seed, nil
}

func (e catalogEntry) validate() error {
	switch {
	case slug(e.LookupKey) == "":
		return fmt.Errorf("lookup_key %q must contain a letter or digit", e.LookupKey)
	case slug(e.Product) == "":
		return fmt.Errorf("product %q must contain a letter or digit", e.Product)
	case e.Amount == nil:
		return errors.New("amount is required")
	case *e.Amount < 0:
		return fmt.Errorf("amount %d must not be negative", *e.Amount)
	case !validCurrency(e.Currency):
		return fmt.Errorf("currency %q must be a lowercase three-letter ISO code", e.Currency)
	case !validInterval(e.Interval):
		return fmt.Errorf("interval %q must be day, week, month or year", e.Interval)
	}
	return nil
}

func validCurrency(currency string) bool {
	return len(currency) == 3 && strings.ToLower(currency) == currency
}

func slug(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(name) {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r)
		gap = false
	}
	return b.String()
}

func defaultSeed(plan planConfig) []seedPrice {
	return []seedPrice{{
		productID:   seedProductID,
		productName: plan.name,
		priceID:     seedPriceID,
		lookupKey:   seedLookupKey,
		amount:      plan.amount,
		currency:    plan.currency,
		interval:    plan.interval,
	}}
}

func seedCatalog(a *account, seed []seedPrice, now time.Time) {
	products := map[string]bool{}
	prices := map[string]bool{}
	for _, s := range seed {
		p := seedProduct(a, s, now)
		if !products[p.ID] {
			p.DefaultPrice = nullString(s.priceID)
			products[p.ID] = true
		}
		seedRecurringPrice(a, s, now)
		prices[s.priceID] = true
	}
	for _, pr := range a.Prices {
		if pr.Metadata[seedMarker] != "" && !prices[pr.ID] {
			pr.Active, pr.LookupKey = false, ""
		}
	}
	for _, p := range a.Products {
		if p.Metadata[seedMarker] != "" && !products[p.ID] {
			p.Active = false
		}
	}
}

func seedProduct(a *account, s seedPrice, now time.Time) *product {
	p := find(a.Products, s.productID)
	if p == nil {
		p = newProduct(s.productID, s.productName, now.Unix())
		a.Products = append(a.Products, p)
	}
	p.Name, p.Active = s.productName, true
	p.Metadata[seedMarker] = "true"
	return p
}

func seedRecurringPrice(a *account, s seedPrice, now time.Time) {
	pr := find(a.Prices, s.priceID)
	if pr == nil {
		pr = newPrice(s.priceID, s.productID, s.amount, s.currency, s.interval, 1, now.Unix())
		a.Prices = append(a.Prices, pr)
	}
	pr.Active = true
	pr.Product = s.productID
	pr.UnitAmount = s.amount
	pr.UnitAmountDecimal = strconv.FormatInt(s.amount, 10)
	pr.Currency = s.currency
	pr.Recurring.Interval = s.interval
	pr.LookupKey = nullString(s.lookupKey)
	pr.Metadata[seedMarker] = "true"
}
