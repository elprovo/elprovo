package stripe

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
)

//go:embed pages/*.html
var pageFiles embed.FS

const maxFormBody = 64 << 10

func parsePages() (*template.Template, error) {
	t, err := template.ParseFS(pageFiles, "pages/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse stripe pages: %w", err)
	}
	return t, nil
}

func (s *Simulation) webHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /c/pay/{id}", s.showCheckout)
	mux.HandleFunc("POST /c/pay/{id}", s.submitCheckout)
	mux.HandleFunc("GET /p/session/{secret}", s.showPortal)
	mux.HandleFunc("POST /p/session/{secret}/cancel", s.portalCancel)
	mux.HandleFunc("POST /p/session/{secret}/renew", s.portalRenew)
	mux.HandleFunc("POST /p/session/{secret}/payment_method", s.portalUpdateCard)
	mux.HandleFunc("GET /i/{id}", s.showInvoice)
	s.registerAdmin(mux)
	return mux
}

type cardForm struct {
	Email  string
	Number string
	Expiry string
	CVC    string
	Name   string
}

func cardFormFrom(r *http.Request) cardForm {
	return cardForm{
		Email:  r.PostFormValue("email"),
		Number: r.PostFormValue("cardNumber"),
		Expiry: r.PostFormValue("cardExpiry"),
		CVC:    r.PostFormValue("cardCvc"),
		Name:   r.PostFormValue("billingName"),
	}
}

func (f cardForm) entry() cardEntry {
	return cardEntry{number: f.Number, expiry: f.Expiry, cvc: f.CVC, name: f.Name, email: f.Email}
}

type lineView struct {
	Name     string
	Quantity int64
	Amount   string
}

type checkoutView struct {
	Title        string
	Action       string
	ProductName  string
	Interval     string
	Lines        []lineView
	Recurring    string
	Total        string
	TrialDays    int64
	CollectsCard bool
	EmailLocked  bool
	CancelURL    string
	Form         cardForm
	Error        string
}

type messageView struct {
	Title    string
	Message  string
	LinkURL  string
	LinkText string
}

func (s *Simulation) showCheckout(w http.ResponseWriter, r *http.Request) {
	s.renderCheckout(w, r.PathValue("id"), cardForm{}, "")
}

func (s *Simulation) renderCheckout(w http.ResponseWriter, id string, form cardForm, problem string) {
	var view checkoutView
	var gone *messageView
	s.store.Read(func(a *account) {
		cs := find(a.CheckoutSessions, id)
		switch {
		case cs == nil:
			gone = &messageView{Title: "Page not found", Message: "This Checkout page does not exist."}
		case cs.Status == "complete":
			gone = &messageView{Title: "Payment complete", Message: "This Checkout Session is already complete."}
		case cs.Status == "expired" || cs.ExpiresAt <= s.now().Unix():
			gone = &messageView{Title: "Session expired", Message: "This Checkout Session has expired.", LinkURL: string(cs.CancelURL), LinkText: "Return"}
		default:
			view = checkoutPage(a, cs, form, problem)
		}
	})
	if gone != nil {
		s.renderPage(w, http.StatusNotFound, "message", gone)
		return
	}
	s.renderPage(w, http.StatusOK, "checkout", view)
}

func checkoutPage(a *account, cs *checkoutSession, form cardForm, problem string) checkoutView {
	order := a.CheckoutOrders[cs.ID]
	view := checkoutView{
		Title:        "Checkout",
		Action:       "/c/pay/" + cs.ID,
		Total:        formatMoney(cs.AmountTotal, cs.Currency),
		TrialDays:    order.TrialPeriodDays,
		CollectsCard: cs.collectsPaymentMethod(),
		CancelURL:    string(cs.CancelURL),
		Form:         form,
		Error:        problem,
	}
	var recurring int64
	for _, li := range order.LineItems {
		pr := find(a.Prices, li.Price)
		name := pr.Product
		if p := find(a.Products, pr.Product); p != nil {
			name = p.Name
		}
		if view.ProductName == "" {
			view.ProductName, view.Interval = name, pr.Recurring.Interval
		}
		view.Lines = append(view.Lines, lineView{Name: name, Quantity: li.Quantity, Amount: formatMoney(pr.UnitAmount*li.Quantity, pr.Currency)})
		recurring += pr.UnitAmount * li.Quantity
	}
	view.Recurring = formatMoney(recurring, cs.Currency)
	if c := find(a.Customers, string(cs.Customer)); c != nil && c.Email != "" {
		view.Form.Email, view.EmailLocked = string(c.Email), true
	} else if cs.CustomerEmail != "" {
		view.Form.Email, view.EmailLocked = string(cs.CustomerEmail), true
	}
	return view
}

func (s *Simulation) submitCheckout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	form, ok := s.parseForm(w, r)
	if !ok {
		return
	}
	var redirect string
	_, err := s.mutate(requestInfo{}, func(t *tx) (any, error) {
		cs, err := t.completeCheckout(id, form.entry())
		if err != nil {
			return nil, err
		}
		redirect = successRedirect(cs)
		return nil, nil
	})
	if err != nil {
		form.Number, form.CVC = "", ""
		s.renderCheckout(w, id, form, s.userMessage(err))
		return
	}
	if redirect == "" {
		s.renderPage(w, http.StatusOK, "message", messageView{Title: "Thanks for subscribing", Message: "Your payment is complete."})
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Simulation) parseForm(w http.ResponseWriter, r *http.Request) (cardForm, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		s.renderPage(w, http.StatusBadRequest, "message", messageView{Title: "Bad request", Message: "The form could not be read."})
		return cardForm{}, false
	}
	return cardFormFrom(r), true
}

func (s *Simulation) userMessage(err error) string {
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status < http.StatusInternalServerError {
		return apiErr.Message
	}
	s.log.Error("hosted page action failed", "error", err)
	return "Something went wrong. Please try again."
}

type subscriptionView struct {
	ID                string
	Name              string
	Price             string
	Interval          string
	Status            string
	CancelAtPeriodEnd bool
	PeriodEnd         string
}

type invoiceView struct {
	Number string
	Date   string
	Amount string
	Status string
	URL    string
}

type portalView struct {
	Title         string
	Base          string
	Email         string
	ReturnURL     string
	ReturnHost    string
	Subscriptions []subscriptionView
	Card          string
	Invoices      []invoiceView
	Form          cardForm
	Error         string
	CardError     string
}

func (s *Simulation) showPortal(w http.ResponseWriter, r *http.Request) {
	s.renderPortal(w, r.PathValue("secret"), "", "")
}

func (s *Simulation) renderPortal(w http.ResponseWriter, secret, problem, cardProblem string) {
	var view portalView
	found := false
	s.store.Read(func(a *account) {
		if ps := a.portalSession(secret); ps != nil {
			view, found = portalPage(a, ps, "/p/session/"+secret), true
		}
	})
	if !found {
		s.renderPage(w, http.StatusNotFound, "message", messageView{Title: "Page not found", Message: "This billing portal link is not valid."})
		return
	}
	view.Error, view.CardError = problem, cardProblem
	s.renderPage(w, http.StatusOK, "portal", view)
}

func portalPage(a *account, ps *portalSession, base string) portalView {
	c := find(a.Customers, ps.Customer)
	view := portalView{Title: "Billing", Base: base, Email: string(c.Email), ReturnURL: string(ps.ReturnURL)}
	if u, err := url.Parse(view.ReturnURL); err == nil {
		view.ReturnHost = u.Host
	}
	for _, sub := range a.Subscriptions {
		if sub.Customer != c.ID || sub.Status == "canceled" {
			continue
		}
		item := sub.Items.Data[0]
		name := item.Price.Product
		if p := find(a.Products, item.Price.Product); p != nil {
			name = p.Name
		}
		view.Subscriptions = append(view.Subscriptions, subscriptionView{
			ID:                sub.ID,
			Name:              name,
			Price:             formatMoney(item.Price.UnitAmount*item.Quantity, item.Price.Currency),
			Interval:          item.Price.Recurring.Interval,
			Status:            sub.Status,
			CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
			PeriodEnd:         formatDate(sub.CurrentPeriodEnd),
		})
		if view.Card == "" {
			view.Card = cardLabel(find(a.PaymentMethods, defaultPaymentMethod(sub, c)))
		}
	}
	if view.Card == "" {
		view.Card = cardLabel(find(a.PaymentMethods, string(c.InvoiceSettings.DefaultPaymentMethod)))
	}
	for i := len(a.Invoices) - 1; i >= 0; i-- {
		if inv := a.Invoices[i]; inv.Customer == c.ID && inv.Status != "draft" {
			view.Invoices = append(view.Invoices, invoiceView{
				Number: string(inv.Number),
				Date:   formatDate(inv.Created),
				Amount: formatMoney(inv.Total, inv.Currency),
				Status: inv.Status,
				URL:    string(inv.HostedInvoiceURL),
			})
		}
	}
	return view
}

func cardLabel(pm *paymentMethod) string {
	if pm == nil {
		return ""
	}
	return fmt.Sprintf("%s •••• %s, expires %02d/%d", displayBrand(pm.Card.Brand), pm.Card.Last4, pm.Card.ExpMonth, pm.Card.ExpYear)
}

func displayBrand(brand string) string {
	switch brand {
	case "visa":
		return "Visa"
	case "mastercard":
		return "Mastercard"
	}
	return brand
}

func (s *Simulation) portalCancel(w http.ResponseWriter, r *http.Request) {
	s.portalSetCancel(w, r, true)
}

func (s *Simulation) portalRenew(w http.ResponseWriter, r *http.Request) {
	s.portalSetCancel(w, r, false)
}

func (s *Simulation) portalSetCancel(w http.ResponseWriter, r *http.Request, cancel bool) {
	secret := r.PathValue("secret")
	if _, ok := s.parseForm(w, r); !ok {
		return
	}
	subscriptionID := r.PostFormValue("subscription")
	_, err := s.mutate(requestInfo{}, func(t *tx) (any, error) {
		return nil, t.portalSetCancelAtPeriodEnd(secret, subscriptionID, cancel)
	})
	if err != nil {
		s.renderPortal(w, secret, s.userMessage(err), "")
		return
	}
	http.Redirect(w, r, "/p/session/"+secret, http.StatusSeeOther)
}

func (s *Simulation) portalUpdateCard(w http.ResponseWriter, r *http.Request) {
	secret := r.PathValue("secret")
	form, ok := s.parseForm(w, r)
	if !ok {
		return
	}
	_, err := s.mutate(requestInfo{}, func(t *tx) (any, error) {
		return nil, t.portalReplaceCard(secret, form.entry())
	})
	if err != nil {
		s.renderPortal(w, secret, "", s.userMessage(err))
		return
	}
	http.Redirect(w, r, "/p/session/"+secret, http.StatusSeeOther)
}

type hostedInvoiceView struct {
	Title    string
	Number   string
	Status   string
	Date     string
	Total    string
	Customer string
	Lines    []lineView
}

func (s *Simulation) showInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var view *hostedInvoiceView
	s.store.Read(func(a *account) {
		inv := find(a.Invoices, id)
		if inv == nil || inv.Status == "draft" {
			return
		}
		view = &hostedInvoiceView{
			Title:    "Invoice " + string(inv.Number),
			Number:   string(inv.Number),
			Status:   inv.Status,
			Date:     formatDate(inv.Created),
			Total:    formatMoney(inv.Total, inv.Currency),
			Customer: string(inv.CustomerEmail),
		}
		for _, line := range inv.Lines.Data {
			view.Lines = append(view.Lines, lineView{Name: line.Description, Quantity: line.Quantity, Amount: formatMoney(line.Amount, line.Currency)})
		}
	})
	if view == nil {
		s.renderPage(w, http.StatusNotFound, "message", messageView{Title: "Page not found", Message: "This invoice does not exist."})
		return
	}
	s.renderPage(w, http.StatusOK, "invoice", view)
}

func (s *Simulation) renderPage(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render page", "page", name, "error", err)
		http.Error(w, "render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
