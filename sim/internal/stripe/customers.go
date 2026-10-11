package stripe

import (
	"crypto/rand"
	"fmt"
)

type customerFields struct {
	email                *string
	name                 *string
	description          *string
	phone                *string
	metadata             metadataUpdate
	defaultPaymentMethod *string
	testClock            string
}

func (t *tx) createCustomer(in customerFields) (*customer, error) {
	if in.testClock != "" && find(t.acct.TestClocks, in.testClock) == nil {
		return nil, noSuch("test_clock", in.testClock, "test_clock")
	}
	if pm := in.defaultPaymentMethod; pm != nil && *pm != "" {
		return nil, paymentMethodNotAttached(*pm)
	}
	c := &customer{
		ID:                  newID("cus"),
		Object:              "customer",
		Created:             t.clockNow(nullString(in.testClock)),
		InvoicePrefix:       invoicePrefix(),
		Metadata:            in.metadata.apply(nil),
		NextInvoiceSequence: 1,
		PreferredLocales:    []string{},
		TaxExempt:           "none",
		TestClock:           nullString(in.testClock),
	}
	in.applyContact(c)
	t.acct.Customers = append(t.acct.Customers, c)
	t.emit("customer.created", c)
	return c, nil
}

func (t *tx) updateCustomer(id string, in customerFields) (*customer, error) {
	c := find(t.acct.Customers, id)
	if c == nil {
		return nil, noSuch("customer", id, "id")
	}
	if pm := in.defaultPaymentMethod; pm != nil && *pm != "" {
		if attached := find(t.acct.PaymentMethods, *pm); attached == nil || string(attached.Customer) != c.ID {
			return nil, paymentMethodNotAttached(*pm)
		}
	}
	before := snapshot(c)
	in.applyContact(c)
	c.Metadata = in.metadata.apply(c.Metadata)
	if in.defaultPaymentMethod != nil {
		c.InvoiceSettings.DefaultPaymentMethod = nullString(*in.defaultPaymentMethod)
	}
	t.emitUpdate("customer.updated", c, before)
	return c, nil
}

func (in customerFields) applyContact(c *customer) {
	setIfSent(&c.Email, in.email)
	setIfSent(&c.Name, in.name)
	setIfSent(&c.Description, in.description)
	setIfSent(&c.Phone, in.phone)
}

func setIfSent(field *nullString, value *string) {
	if value != nil {
		*field = nullString(*value)
	}
}

func paymentMethodNotAttached(id string) *apiError {
	return invalidRequest("invoice_settings[default_payment_method]",
		"The customer does not have a payment method with the ID %s. The payment method must be attached to the customer.", id)
}

func invoicePrefix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%X", b)
}
