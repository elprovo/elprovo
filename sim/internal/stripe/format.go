package stripe

import (
	"fmt"
	"strings"
	"time"
)

func formatMoney(amount int64, currency string) string {
	value := fmt.Sprintf("%d.%02d", amount/100, amount%100)
	switch currency {
	case "usd":
		return "$" + value
	case "eur":
		return "€" + value
	case "gbp":
		return "£" + value
	}
	return value + " " + strings.ToUpper(currency)
}

func formatDate(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("January 2, 2006")
}
