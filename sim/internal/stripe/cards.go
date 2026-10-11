package stripe

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const declinedMessage = "Your card was declined."

type cardEntry struct {
	number string
	expiry string
	cvc    string
	name   string
	email  string
}

type validCard struct {
	brand           string
	last4           string
	fingerprint     string
	expMonth        int64
	expYear         int64
	declinesRenewal bool
}

type testCard struct {
	brand           string
	declinesAttach  bool
	declinesRenewal bool
}

func lookupTestCard(number string) (testCard, bool) {
	switch number {
	case "4242424242424242":
		return testCard{brand: "visa"}, true
	case "4000000000000002":
		return testCard{brand: "visa", declinesAttach: true}, true
	case "4000000000000341":
		return testCard{brand: "visa", declinesRenewal: true}, true
	}
	return testCard{}, false
}

func (e cardEntry) validate(now time.Time) (validCard, error) {
	number := strings.Join(strings.Fields(e.number), "")
	if number == "" {
		return validCard{}, cardProblem("incomplete_number", "Your card number is incomplete.")
	}
	month, year, ok := parseExpiry(e.expiry)
	if !ok {
		return validCard{}, cardProblem("incomplete_expiry", "Your card's expiration date is incomplete.")
	}
	if year < int64(now.Year()) || (year == int64(now.Year()) && month < int64(now.Month())) {
		return validCard{}, cardProblem("invalid_expiry_year", "Your card's expiration year is in the past.")
	}
	if !isDigits(e.cvc) || len(e.cvc) < 3 || len(e.cvc) > 4 {
		return validCard{}, cardProblem("incomplete_cvc", "Your card's security code is incomplete.")
	}
	tc, known := lookupTestCard(number)
	if !known {
		return validCard{}, cardDeclined(declinedMessage+" Your request was in test mode, but used a non test (live) card. "+
			"For a list of valid test cards, visit: https://stripe.com/docs/testing.", "test_mode_live_card")
	}
	if tc.declinesAttach {
		return validCard{}, cardDeclined(declinedMessage, "generic_decline")
	}
	sum := sha256.Sum256([]byte(number))
	return validCard{
		brand:           tc.brand,
		last4:           number[len(number)-4:],
		fingerprint:     hex.EncodeToString(sum[:8]),
		expMonth:        month,
		expYear:         year,
		declinesRenewal: tc.declinesRenewal,
	}, nil
}

func parseExpiry(s string) (month, year int64, ok bool) {
	m, y, found := strings.Cut(strings.ReplaceAll(s, " ", ""), "/")
	if !found || !isDigits(m) || !isDigits(y) || (len(y) != 2 && len(y) != 4) {
		return 0, 0, false
	}
	month, _ = strconv.ParseInt(m, 10, 64)
	year, _ = strconv.ParseInt(y, 10, 64)
	if len(y) == 2 {
		year += 2000
	}
	return month, year, month >= 1 && month <= 12
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func cardProblem(code, message string) *apiError {
	return newAPIError(http.StatusPaymentRequired, "card_error", code, "", message)
}
