package stripe

import (
	"strconv"
	"time"
)

const (
	seedProductID = "prod_team_stagelet"
	seedPriceID   = "price_team_stagelet"
	seedLookupKey = "team_stagelet"
)

type productFields struct {
	name        string
	description string
	active      bool
	metadata    metadataUpdate
}

type priceFields struct {
	product       string
	productName   string
	unitAmount    int64
	currency      string
	interval      string
	intervalCount int64
	nickname      string
	lookupKey     string
	active        bool
	metadata      metadataUpdate
}

func validInterval(interval string) bool {
	switch interval {
	case "day", "week", "month", "year":
		return true
	}
	return false
}

func (t *tx) createProduct(in productFields) (*product, error) {
	if in.name == "" {
		return nil, missingParam("name")
	}
	p := newProduct(newID("prod"), in.name, t.now.Unix())
	p.Description = nullString(in.description)
	p.Active = in.active
	p.Metadata = in.metadata.apply(nil)
	t.acct.Products = append(t.acct.Products, p)
	t.emit("product.created", p)
	return p, nil
}

func (t *tx) createPrice(in priceFields) (*price, error) {
	if err := t.validatePrice(in); err != nil {
		return nil, err
	}
	productID := in.product
	if productID == "" {
		p, err := t.createProduct(productFields{name: in.productName, active: true})
		if err != nil {
			return nil, err
		}
		productID = p.ID
	}
	pr := newPrice(newID("price"), productID, in.unitAmount, in.currency, in.interval, in.intervalCount, t.now.Unix())
	pr.Active = in.active
	pr.Nickname = nullString(in.nickname)
	pr.LookupKey = nullString(in.lookupKey)
	pr.Metadata = in.metadata.apply(nil)
	t.acct.Prices = append(t.acct.Prices, pr)
	t.emit("price.created", pr)
	return pr, nil
}

func (t *tx) validatePrice(in priceFields) error {
	switch {
	case in.currency == "":
		return missingParam("currency")
	case in.unitAmount < 0:
		return invalidRequest("unit_amount", "This value must be greater than or equal to 0.")
	case in.product == "" && in.productName == "":
		return missingParam("product")
	case in.product != "" && find(t.acct.Products, in.product) == nil:
		return noSuch("product", in.product, "product")
	case in.interval != "" && !validInterval(in.interval):
		return invalidRequest("recurring[interval]", "Invalid recurring[interval]: must be one of day, month, week, or year")
	}
	if in.lookupKey == "" {
		return nil
	}
	for _, pr := range t.acct.Prices {
		if pr.Active && string(pr.LookupKey) == in.lookupKey {
			return invalidRequest("lookup_key", "A price (`%s`) already uses that lookup key.", pr.ID)
		}
	}
	return nil
}

func newProduct(id, name string, created int64) *product {
	return &product{
		ID:                id,
		Object:            "product",
		Active:            true,
		Created:           created,
		Images:            []string{},
		MarketingFeatures: []string{},
		Metadata:          map[string]string{},
		Name:              name,
		Type:              "service",
		Updated:           created,
	}
}

func newPrice(id, productID string, amount int64, currency, interval string, intervalCount, created int64) *price {
	pr := &price{
		ID:                id,
		Object:            "price",
		Active:            true,
		BillingScheme:     "per_unit",
		Created:           created,
		Currency:          currency,
		Metadata:          map[string]string{},
		Product:           productID,
		TaxBehavior:       "unspecified",
		Type:              "one_time",
		UnitAmount:        amount,
		UnitAmountDecimal: strconv.FormatInt(amount, 10),
	}
	if interval != "" {
		pr.Type = "recurring"
		pr.Recurring = &recurring{Interval: interval, IntervalCount: max(intervalCount, 1), UsageType: "licensed"}
	}
	return pr
}

func planOf(pr price) plan {
	return plan{
		ID:            pr.ID,
		Object:        "plan",
		Active:        pr.Active,
		Amount:        pr.UnitAmount,
		AmountDecimal: pr.UnitAmountDecimal,
		BillingScheme: pr.BillingScheme,
		Created:       pr.Created,
		Currency:      pr.Currency,
		Interval:      pr.Recurring.Interval,
		IntervalCount: pr.Recurring.IntervalCount,
		Metadata:      pr.Metadata,
		Nickname:      pr.Nickname,
		Product:       pr.Product,
		UsageType:     pr.Recurring.UsageType,
	}
}

func seedPlan(a *account, cfg planConfig, now time.Time) {
	p := find(a.Products, seedProductID)
	if p == nil {
		p = newProduct(seedProductID, cfg.name, now.Unix())
		a.Products = append(a.Products, p)
	}
	p.Name = cfg.name
	p.DefaultPrice = seedPriceID

	pr := find(a.Prices, seedPriceID)
	if pr == nil {
		pr = newPrice(seedPriceID, seedProductID, cfg.amount, cfg.currency, cfg.interval, 1, now.Unix())
		a.Prices = append(a.Prices, pr)
	}
	pr.UnitAmount = cfg.amount
	pr.UnitAmountDecimal = strconv.FormatInt(cfg.amount, 10)
	pr.Currency = cfg.currency
	pr.Recurring.Interval = cfg.interval
	pr.LookupKey = seedLookupKey
}
