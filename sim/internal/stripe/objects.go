package stripe

import (
	"encoding/json"
	"strconv"
)

type nullString string

func (s nullString) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(s))
}

func (s *nullString) UnmarshalJSON(raw []byte) error {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*s = ""
	if v != nil {
		*s = nullString(*v)
	}
	return nil
}

type nullTime int64

func (t nullTime) MarshalJSON() ([]byte, error) {
	if t == 0 {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatInt(int64(t), 10)), nil
}

func (t *nullTime) UnmarshalJSON(raw []byte) error {
	var v *int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*t = 0
	if v != nil {
		*t = nullTime(*v)
	}
	return nil
}

type jsonNull struct{}

func (jsonNull) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func (*jsonNull) UnmarshalJSON([]byte) error { return nil }

type customer struct {
	ID                  string                  `json:"id"`
	Object              string                  `json:"object"`
	Address             jsonNull                `json:"address"`
	Balance             int64                   `json:"balance"`
	Created             int64                   `json:"created"`
	Currency            nullString              `json:"currency"`
	DefaultSource       jsonNull                `json:"default_source"`
	Delinquent          bool                    `json:"delinquent"`
	Description         nullString              `json:"description"`
	Email               nullString              `json:"email"`
	InvoicePrefix       string                  `json:"invoice_prefix"`
	InvoiceSettings     customerInvoiceSettings `json:"invoice_settings"`
	Livemode            bool                    `json:"livemode"`
	Metadata            map[string]string       `json:"metadata"`
	Name                nullString              `json:"name"`
	NextInvoiceSequence int64                   `json:"next_invoice_sequence"`
	Phone               nullString              `json:"phone"`
	PreferredLocales    []string                `json:"preferred_locales"`
	Shipping            jsonNull                `json:"shipping"`
	TaxExempt           string                  `json:"tax_exempt"`
	TestClock           nullString              `json:"test_clock"`
}

type customerInvoiceSettings struct {
	CustomFields         jsonNull   `json:"custom_fields"`
	DefaultPaymentMethod nullString `json:"default_payment_method"`
	Footer               jsonNull   `json:"footer"`
	RenderingOptions     jsonNull   `json:"rendering_options"`
}

type product struct {
	ID                  string            `json:"id"`
	Object              string            `json:"object"`
	Active              bool              `json:"active"`
	Created             int64             `json:"created"`
	DefaultPrice        nullString        `json:"default_price"`
	Description         nullString        `json:"description"`
	Images              []string          `json:"images"`
	Livemode            bool              `json:"livemode"`
	MarketingFeatures   []string          `json:"marketing_features"`
	Metadata            map[string]string `json:"metadata"`
	Name                string            `json:"name"`
	StatementDescriptor nullString        `json:"statement_descriptor"`
	TaxCode             nullString        `json:"tax_code"`
	Type                string            `json:"type"`
	UnitLabel           nullString        `json:"unit_label"`
	Updated             int64             `json:"updated"`
	URL                 nullString        `json:"url"`
}

type price struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Active            bool              `json:"active"`
	BillingScheme     string            `json:"billing_scheme"`
	Created           int64             `json:"created"`
	Currency          string            `json:"currency"`
	CustomUnitAmount  jsonNull          `json:"custom_unit_amount"`
	Livemode          bool              `json:"livemode"`
	LookupKey         nullString        `json:"lookup_key"`
	Metadata          map[string]string `json:"metadata"`
	Nickname          nullString        `json:"nickname"`
	Product           string            `json:"product"`
	Recurring         *recurring        `json:"recurring"`
	TaxBehavior       string            `json:"tax_behavior"`
	TiersMode         jsonNull          `json:"tiers_mode"`
	TransformQuantity jsonNull          `json:"transform_quantity"`
	Type              string            `json:"type"`
	UnitAmount        int64             `json:"unit_amount"`
	UnitAmountDecimal string            `json:"unit_amount_decimal"`
}

type recurring struct {
	Interval        string   `json:"interval"`
	IntervalCount   int64    `json:"interval_count"`
	Meter           jsonNull `json:"meter"`
	TrialPeriodDays jsonNull `json:"trial_period_days"`
	UsageType       string   `json:"usage_type"`
}

type plan struct {
	ID              string            `json:"id"`
	Object          string            `json:"object"`
	Active          bool              `json:"active"`
	Amount          int64             `json:"amount"`
	AmountDecimal   string            `json:"amount_decimal"`
	BillingScheme   string            `json:"billing_scheme"`
	Created         int64             `json:"created"`
	Currency        string            `json:"currency"`
	Interval        string            `json:"interval"`
	IntervalCount   int64             `json:"interval_count"`
	Livemode        bool              `json:"livemode"`
	Metadata        map[string]string `json:"metadata"`
	Nickname        nullString        `json:"nickname"`
	Product         string            `json:"product"`
	TiersMode       jsonNull          `json:"tiers_mode"`
	TrialPeriodDays jsonNull          `json:"trial_period_days"`
	UsageType       string            `json:"usage_type"`
}

type checkoutSession struct {
	ID                      string            `json:"id"`
	Object                  string            `json:"object"`
	AmountSubtotal          int64             `json:"amount_subtotal"`
	AmountTotal             int64             `json:"amount_total"`
	AutomaticTax            automaticTax      `json:"automatic_tax"`
	CancelURL               nullString        `json:"cancel_url"`
	ClientReferenceID       nullString        `json:"client_reference_id"`
	Created                 int64             `json:"created"`
	Currency                string            `json:"currency"`
	Customer                nullString        `json:"customer"`
	CustomerCreation        nullString        `json:"customer_creation"`
	CustomerDetails         *customerDetails  `json:"customer_details"`
	CustomerEmail           nullString        `json:"customer_email"`
	ExpiresAt               int64             `json:"expires_at"`
	Invoice                 nullString        `json:"invoice"`
	Livemode                bool              `json:"livemode"`
	Locale                  jsonNull          `json:"locale"`
	Metadata                map[string]string `json:"metadata"`
	Mode                    string            `json:"mode"`
	PaymentIntent           nullString        `json:"payment_intent"`
	PaymentLink             jsonNull          `json:"payment_link"`
	PaymentMethodCollection string            `json:"payment_method_collection"`
	PaymentMethodTypes      []string          `json:"payment_method_types"`
	PaymentStatus           string            `json:"payment_status"`
	SetupIntent             jsonNull          `json:"setup_intent"`
	Status                  string            `json:"status"`
	SubmitType              jsonNull          `json:"submit_type"`
	Subscription            nullString        `json:"subscription"`
	SuccessURL              nullString        `json:"success_url"`
	TotalDetails            totalDetails      `json:"total_details"`
	UIMode                  string            `json:"ui_mode"`
	URL                     nullString        `json:"url"`
}

type automaticTax struct {
	Enabled   bool     `json:"enabled"`
	Liability jsonNull `json:"liability"`
	Status    jsonNull `json:"status"`
}

type totalDetails struct {
	AmountDiscount int64 `json:"amount_discount"`
	AmountShipping int64 `json:"amount_shipping"`
	AmountTax      int64 `json:"amount_tax"`
}

type customerDetails struct {
	Address   jsonNull   `json:"address"`
	Email     nullString `json:"email"`
	Name      nullString `json:"name"`
	Phone     jsonNull   `json:"phone"`
	TaxExempt string     `json:"tax_exempt"`
	TaxIDs    []string   `json:"tax_ids"`
}

type subscription struct {
	ID                   string              `json:"id"`
	Object               string              `json:"object"`
	BillingCycleAnchor   int64               `json:"billing_cycle_anchor"`
	CancelAt             nullTime            `json:"cancel_at"`
	CancelAtPeriodEnd    bool                `json:"cancel_at_period_end"`
	CanceledAt           nullTime            `json:"canceled_at"`
	CancellationDetails  cancellationDetails `json:"cancellation_details"`
	CollectionMethod     string              `json:"collection_method"`
	Created              int64               `json:"created"`
	Currency             string              `json:"currency"`
	CurrentPeriodEnd     int64               `json:"current_period_end"`
	CurrentPeriodStart   int64               `json:"current_period_start"`
	Customer             string              `json:"customer"`
	DaysUntilDue         jsonNull            `json:"days_until_due"`
	DefaultPaymentMethod nullString          `json:"default_payment_method"`
	Description          nullString          `json:"description"`
	EndedAt              nullTime            `json:"ended_at"`
	Items                subscriptionItems   `json:"items"`
	LatestInvoice        nullString          `json:"latest_invoice"`
	Livemode             bool                `json:"livemode"`
	Metadata             map[string]string   `json:"metadata"`
	PendingSetupIntent   jsonNull            `json:"pending_setup_intent"`
	PendingUpdate        jsonNull            `json:"pending_update"`
	StartDate            int64               `json:"start_date"`
	Status               string              `json:"status"`
	TestClock            nullString          `json:"test_clock"`
	TrialEnd             jsonNull            `json:"trial_end"`
	TrialStart           jsonNull            `json:"trial_start"`
}

type cancellationDetails struct {
	Comment  jsonNull   `json:"comment"`
	Feedback jsonNull   `json:"feedback"`
	Reason   nullString `json:"reason"`
}

type subscriptionItems struct {
	Object     string             `json:"object"`
	Data       []subscriptionItem `json:"data"`
	HasMore    bool               `json:"has_more"`
	TotalCount int                `json:"total_count"`
	URL        string             `json:"url"`
}

type subscriptionItem struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"`
	Created            int64             `json:"created"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	Metadata           map[string]string `json:"metadata"`
	Plan               plan              `json:"plan"`
	Price              price             `json:"price"`
	Quantity           int64             `json:"quantity"`
	Subscription       string            `json:"subscription"`
	TaxRates           []string          `json:"tax_rates"`
}

type invoice struct {
	ID                   string            `json:"id"`
	Object               string            `json:"object"`
	AccountCountry       string            `json:"account_country"`
	AccountName          string            `json:"account_name"`
	AmountDue            int64             `json:"amount_due"`
	AmountOverpaid       int64             `json:"amount_overpaid"`
	AmountPaid           int64             `json:"amount_paid"`
	AmountRemaining      int64             `json:"amount_remaining"`
	AttemptCount         int64             `json:"attempt_count"`
	Attempted            bool              `json:"attempted"`
	AutoAdvance          bool              `json:"auto_advance"`
	AutomaticTax         automaticTax      `json:"automatic_tax"`
	BillingReason        string            `json:"billing_reason"`
	CollectionMethod     string            `json:"collection_method"`
	Created              int64             `json:"created"`
	Currency             string            `json:"currency"`
	Customer             string            `json:"customer"`
	CustomerEmail        nullString        `json:"customer_email"`
	CustomerName         nullString        `json:"customer_name"`
	DefaultPaymentMethod nullString        `json:"default_payment_method"`
	Description          nullString        `json:"description"`
	DueDate              jsonNull          `json:"due_date"`
	EffectiveAt          nullTime          `json:"effective_at"`
	HostedInvoiceURL     nullString        `json:"hosted_invoice_url"`
	InvoicePDF           jsonNull          `json:"invoice_pdf"`
	Issuer               invoiceIssuer     `json:"issuer"`
	Lines                invoiceLines      `json:"lines"`
	Livemode             bool              `json:"livemode"`
	Metadata             map[string]string `json:"metadata"`
	NextPaymentAttempt   nullTime          `json:"next_payment_attempt"`
	Number               nullString        `json:"number"`
	Paid                 bool              `json:"paid"`
	Parent               invoiceParent     `json:"parent"`
	PaymentIntent        nullString        `json:"payment_intent"`
	Payments             invoicePayments   `json:"payments"`
	PeriodEnd            int64             `json:"period_end"`
	PeriodStart          int64             `json:"period_start"`
	StartingBalance      int64             `json:"starting_balance"`
	Status               string            `json:"status"`
	StatusTransitions    statusTransitions `json:"status_transitions"`
	Subscription         nullString        `json:"subscription"`
	Subtotal             int64             `json:"subtotal"`
	SubtotalExcludingTax int64             `json:"subtotal_excluding_tax"`
	TestClock            nullString        `json:"test_clock"`
	Total                int64             `json:"total"`
	TotalExcludingTax    int64             `json:"total_excluding_tax"`
	WebhooksDeliveredAt  nullTime          `json:"webhooks_delivered_at"`
}

type invoiceIssuer struct {
	Type string `json:"type"`
}

type invoiceParent struct {
	QuoteDetails        jsonNull                   `json:"quote_details"`
	SubscriptionDetails invoiceSubscriptionDetails `json:"subscription_details"`
	Type                string                     `json:"type"`
}

type invoiceSubscriptionDetails struct {
	Metadata     map[string]string `json:"metadata"`
	Subscription string            `json:"subscription"`
}

type statusTransitions struct {
	FinalizedAt           nullTime `json:"finalized_at"`
	MarkedUncollectibleAt nullTime `json:"marked_uncollectible_at"`
	PaidAt                nullTime `json:"paid_at"`
	VoidedAt              nullTime `json:"voided_at"`
}

type invoiceLines struct {
	Object     string        `json:"object"`
	Data       []invoiceLine `json:"data"`
	HasMore    bool          `json:"has_more"`
	TotalCount int           `json:"total_count"`
	URL        string        `json:"url"`
}

type invoiceLine struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	Description      string            `json:"description"`
	Discountable     bool              `json:"discountable"`
	Invoice          string            `json:"invoice"`
	Livemode         bool              `json:"livemode"`
	Metadata         map[string]string `json:"metadata"`
	Parent           lineParent        `json:"parent"`
	Period           linePeriod        `json:"period"`
	Pricing          linePricing       `json:"pricing"`
	Proration        bool              `json:"proration"`
	Quantity         int64             `json:"quantity"`
	Subscription     string            `json:"subscription"`
	SubscriptionItem string            `json:"subscription_item"`
	Subtotal         int64             `json:"subtotal"`
	Type             string            `json:"type"`
}

type lineParent struct {
	InvoiceItemDetails      jsonNull                `json:"invoice_item_details"`
	SubscriptionItemDetails subscriptionItemDetails `json:"subscription_item_details"`
	Type                    string                  `json:"type"`
}

type subscriptionItemDetails struct {
	InvoiceItem      jsonNull         `json:"invoice_item"`
	Proration        bool             `json:"proration"`
	ProrationDetails prorationDetails `json:"proration_details"`
	Subscription     string           `json:"subscription"`
	SubscriptionItem string           `json:"subscription_item"`
}

type prorationDetails struct {
	CreditedItems jsonNull `json:"credited_items"`
}

type linePeriod struct {
	End   int64 `json:"end"`
	Start int64 `json:"start"`
}

type linePricing struct {
	PriceDetails      priceDetails `json:"price_details"`
	Type              string       `json:"type"`
	UnitAmountDecimal string       `json:"unit_amount_decimal"`
}

type priceDetails struct {
	Price   string `json:"price"`
	Product string `json:"product"`
}

type invoicePayments struct {
	Object     string           `json:"object"`
	Data       []invoicePayment `json:"data"`
	HasMore    bool             `json:"has_more"`
	TotalCount int              `json:"total_count"`
	URL        string           `json:"url"`
}

type invoicePayment struct {
	ID                string                   `json:"id"`
	Object            string                   `json:"object"`
	AmountPaid        int64                    `json:"amount_paid"`
	AmountRequested   int64                    `json:"amount_requested"`
	Created           int64                    `json:"created"`
	Currency          string                   `json:"currency"`
	Invoice           string                   `json:"invoice"`
	IsDefault         bool                     `json:"is_default"`
	Livemode          bool                     `json:"livemode"`
	Payment           invoicePaymentRef        `json:"payment"`
	Status            string                   `json:"status"`
	StatusTransitions paymentStatusTransitions `json:"status_transitions"`
}

type invoicePaymentRef struct {
	PaymentIntent string `json:"payment_intent"`
	Type          string `json:"type"`
}

type paymentStatusTransitions struct {
	CanceledAt nullTime `json:"canceled_at"`
	PaidAt     nullTime `json:"paid_at"`
}

type paymentIntent struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"`
	Amount             int64             `json:"amount"`
	AmountCapturable   int64             `json:"amount_capturable"`
	AmountReceived     int64             `json:"amount_received"`
	CanceledAt         jsonNull          `json:"canceled_at"`
	CaptureMethod      string            `json:"capture_method"`
	ClientSecret       string            `json:"client_secret"`
	ConfirmationMethod string            `json:"confirmation_method"`
	Created            int64             `json:"created"`
	Currency           string            `json:"currency"`
	Customer           nullString        `json:"customer"`
	Description        nullString        `json:"description"`
	Invoice            nullString        `json:"invoice"`
	LastPaymentError   *cardError        `json:"last_payment_error"`
	LatestCharge       jsonNull          `json:"latest_charge"`
	Livemode           bool              `json:"livemode"`
	Metadata           map[string]string `json:"metadata"`
	NextAction         jsonNull          `json:"next_action"`
	PaymentMethod      nullString        `json:"payment_method"`
	PaymentMethodTypes []string          `json:"payment_method_types"`
	SetupFutureUsage   jsonNull          `json:"setup_future_usage"`
	Status             string            `json:"status"`
}

type cardError struct {
	Code          string         `json:"code"`
	DeclineCode   string         `json:"decline_code"`
	DocURL        string         `json:"doc_url"`
	Message       string         `json:"message"`
	PaymentMethod *paymentMethod `json:"payment_method"`
	Type          string         `json:"type"`
}

type paymentMethod struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	AllowRedisplay string            `json:"allow_redisplay"`
	BillingDetails billingDetails    `json:"billing_details"`
	Card           card              `json:"card"`
	Created        int64             `json:"created"`
	Customer       nullString        `json:"customer"`
	Livemode       bool              `json:"livemode"`
	Metadata       map[string]string `json:"metadata"`
	Type           string            `json:"type"`
}

type billingDetails struct {
	Address jsonNull   `json:"address"`
	Email   nullString `json:"email"`
	Name    nullString `json:"name"`
	Phone   jsonNull   `json:"phone"`
}

type card struct {
	Brand        string       `json:"brand"`
	Checks       cardChecks   `json:"checks"`
	Country      string       `json:"country"`
	DisplayBrand string       `json:"display_brand"`
	ExpMonth     int64        `json:"exp_month"`
	ExpYear      int64        `json:"exp_year"`
	Fingerprint  string       `json:"fingerprint"`
	Funding      string       `json:"funding"`
	Last4        string       `json:"last4"`
	Networks     cardNetworks `json:"networks"`
	Wallet       jsonNull     `json:"wallet"`
}

type cardChecks struct {
	AddressLine1Check      jsonNull `json:"address_line1_check"`
	AddressPostalCodeCheck jsonNull `json:"address_postal_code_check"`
	CVCCheck               string   `json:"cvc_check"`
}

type cardNetworks struct {
	Available []string `json:"available"`
	Preferred jsonNull `json:"preferred"`
}

type event struct {
	ID              string       `json:"id"`
	Object          string       `json:"object"`
	APIVersion      string       `json:"api_version"`
	Created         int64        `json:"created"`
	Data            eventData    `json:"data"`
	Livemode        bool         `json:"livemode"`
	PendingWebhooks int          `json:"pending_webhooks"`
	Request         eventRequest `json:"request"`
	Type            string       `json:"type"`
}

type eventData struct {
	Object             json.RawMessage `json:"object"`
	PreviousAttributes map[string]any  `json:"previous_attributes,omitempty"`
}

type eventRequest struct {
	ID             nullString `json:"id"`
	IdempotencyKey nullString `json:"idempotency_key"`
}

type webhookEndpoint struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	APIVersion    nullString        `json:"api_version"`
	Application   jsonNull          `json:"application"`
	Created       int64             `json:"created"`
	Description   nullString        `json:"description"`
	EnabledEvents []string          `json:"enabled_events"`
	Livemode      bool              `json:"livemode"`
	Metadata      map[string]string `json:"metadata"`
	Secret        string            `json:"secret,omitempty"`
	Status        string            `json:"status"`
	URL           string            `json:"url"`
}

type deletedObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

type portalSession struct {
	ID            string     `json:"id"`
	Object        string     `json:"object"`
	Configuration string     `json:"configuration"`
	Created       int64      `json:"created"`
	Customer      string     `json:"customer"`
	Flow          jsonNull   `json:"flow"`
	Livemode      bool       `json:"livemode"`
	Locale        jsonNull   `json:"locale"`
	OnBehalfOf    jsonNull   `json:"on_behalf_of"`
	ReturnURL     nullString `json:"return_url"`
	URL           string     `json:"url"`
}

type testClock struct {
	ID           string     `json:"id"`
	Object       string     `json:"object"`
	Created      int64      `json:"created"`
	DeletesAfter int64      `json:"deletes_after"`
	FrozenTime   int64      `json:"frozen_time"`
	Livemode     bool       `json:"livemode"`
	Name         nullString `json:"name"`
	Status       string     `json:"status"`
}
