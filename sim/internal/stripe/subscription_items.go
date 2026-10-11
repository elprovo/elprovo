package stripe

import (
	"fmt"
	"slices"
)

type itemUpdate struct {
	id       string
	price    string
	quantity *int64
	deleted  bool
}

func validateProrationBehavior(behavior string) error {
	switch behavior {
	case "", "always_invoice", "create_prorations", "none":
		return nil
	}
	return invalidRequest("proration_behavior", "Invalid proration_behavior: must be one of always_invoice, create_prorations, or none")
}

func (t *tx) updatedItems(sub *subscription, updates []itemUpdate) ([]subscriptionItem, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	items := slices.Clone(sub.Items.Data)
	for i, u := range updates {
		var err error
		if items, err = t.applyItemUpdate(sub, items, u, fmt.Sprintf("items[%d]", i)); err != nil {
			return nil, err
		}
	}
	if len(items) == 0 {
		return nil, invalidRequest("items", "A subscription must keep at least one item. To end the subscription, cancel it instead.")
	}
	for _, item := range items {
		if item.Price.Currency != sub.Currency || !sameBillingTerms(item.Price, items[0].Price) {
			return nil, mixedPriceTerms("items")
		}
	}
	return items, nil
}

func (t *tx) applyItemUpdate(sub *subscription, items []subscriptionItem, u itemUpdate, param string) ([]subscriptionItem, error) {
	if u.quantity != nil && *u.quantity < 0 {
		return nil, invalidRequest(param+"[quantity]", "This value must be greater than or equal to 0.")
	}
	if u.id == "" {
		return t.addItem(sub, items, u, param)
	}
	i := slices.IndexFunc(items, func(item subscriptionItem) bool { return item.ID == u.id })
	if i < 0 {
		return nil, noSuch("subscription_item", u.id, param+"[id]")
	}
	if u.deleted {
		return slices.Delete(items, i, i+1), nil
	}
	if u.price != "" {
		pr, err := t.subscribablePrice(u.price, param+"[price]")
		if err != nil {
			return nil, err
		}
		items[i].Price, items[i].Plan, items[i].Quantity = *pr, planOf(*pr), 1
	}
	if u.quantity != nil {
		items[i].Quantity = *u.quantity
	}
	return items, nil
}

func (t *tx) addItem(sub *subscription, items []subscriptionItem, u itemUpdate, param string) ([]subscriptionItem, error) {
	if u.deleted {
		return nil, missingParam(param + "[id]")
	}
	if u.price == "" {
		return nil, missingParam(param + "[price]")
	}
	pr, err := t.subscribablePrice(u.price, param+"[price]")
	if err != nil {
		return nil, err
	}
	quantity := int64(1)
	if u.quantity != nil {
		quantity = *u.quantity
	}
	item := newSubscriptionItem(sub.ID, *pr, quantity, t.clockNow(sub.TestClock))
	item.CurrentPeriodStart, item.CurrentPeriodEnd = sub.CurrentPeriodStart, sub.CurrentPeriodEnd
	return append(items, item), nil
}

func (t *tx) subscribablePrice(id, param string) (*price, error) {
	pr := find(t.acct.Prices, id)
	switch {
	case pr == nil:
		return nil, noSuch("price", id, param)
	case !pr.Active:
		return nil, invalidRequest(param, "The price specified is inactive. This field only accepts active prices.")
	case pr.Recurring == nil:
		return nil, invalidRequest(param, "The price specified is set to `type=one_time` but this field only accepts prices with `type=recurring`.")
	}
	return pr, nil
}
