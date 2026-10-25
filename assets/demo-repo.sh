#!/bin/sh
# Makes the demo repository that the README's examples and assets/demo.gif
# come from, and prints its path. It holds a small shop API whose SAVE10
# coupon takes 10% off again on every use, with unit tests and e2e tests of
# the API, which run against the shop in Docker Compose. Its branches from
# main:
#
#   test-only     the test that reproduces the bug (checked out)
#   coupon-once   test-only, then the fix
#   late-check    test-only, then a fix that checks only after the discount
#   api-break     a change to the API that only the e2e tests see
#
# Fixed names and dates give the same commit IDs on every machine.
set -eu
repo=$(mktemp -d "${TMPDIR:-/tmp}/elprovo-demo.XXXXXX")
cd "$repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Demo GIT_AUTHOR_EMAIL=demo@example.invalid
export GIT_COMMITTER_NAME=Demo GIT_COMMITTER_EMAIL=demo@example.invalid

# commit commits every change with the date $1 and the message $2.
commit() {
	git add -A
	GIT_AUTHOR_DATE=$1 GIT_COMMITTER_DATE=$1 git commit -q -m "$2"
}

# cart prints cart/cart.go. $1 is the body of ApplyCoupon after the code
# check, and $2 the extra field of Cart.
cart() {
	cat <<EOF
// Package cart holds the shop's cart and its coupons.
package cart

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownCoupon is the error for a code that is not a coupon.
var ErrUnknownCoupon = errors.New("unknown coupon")

// Cart is a shopping cart. Its zero value is an empty cart.
type Cart struct {
	mu    sync.Mutex
	cents int64$2
}

// Add puts an item that costs cents into the cart.
func (c *Cart) Add(cents int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cents += cents
}

// ApplyCoupon applies a coupon. SAVE10 takes 10% off the total.
func (c *Cart) ApplyCoupon(code string) error {
	if code != "SAVE10" {
		return ErrUnknownCoupon
	}
	c.mu.Lock()
	defer c.mu.Unlock()
$1	return nil
}

// Total returns the total, such as 90.00.
func (c *Cart) Total() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("%d.%02d", c.cents/100, c.cents%100)
}
EOF
}

applied='
	applied map[string]bool'

git init -q -b main
cat > go.mod <<'EOF'
module example.com/shop

go 1.24
EOF
printf '/bin/\n.elprovo/\n' > .gitignore
cat > elprovo.toml <<'EOF'
version = 3
pass_env = ["GOCACHE", "GOMODCACHE", "DOCKER_CONFIG"]

[tests]
files = ["**/*_test.go", "**/testdata/**"]

[suites.unit]
run = "go test -json -count=1 -timeout=5m {packages}"
report = "go-test-json"

[suites.e2e]
run = "go test -json -count=1 -timeout=5m -tags=e2e ./e2e"
report = "go-test-json"

[suites.e2e.app]
start = "docker compose up --detach --build --wait"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "docker compose down --volumes --rmi local"
EOF
cat > Dockerfile <<'EOF'
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /shop .

FROM alpine:3.21
COPY --from=build /shop /shop
ENTRYPOINT ["/shop"]
EOF
cat > compose.yaml <<'EOF'
services:
  shop:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
EOF
cat > main.go <<'EOF'
// Command shop is a small shop API with one cart.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"example.com/shop/cart"
)

func main() {
	var c cart.Cart
	http.HandleFunc("POST /cart/items", func(w http.ResponseWriter, r *http.Request) {
		cents, err := strconv.ParseInt(r.URL.Query().Get("cents"), 10, 64)
		if err != nil || cents <= 0 {
			http.Error(w, "cents must be a positive whole number", http.StatusBadRequest)
			return
		}
		c.Add(cents)
	})
	http.HandleFunc("POST /cart/coupon", func(w http.ResponseWriter, r *http.Request) {
		if err := c.ApplyCoupon(r.URL.Query().Get("code")); errors.Is(err, cart.ErrUnknownCoupon) {
			http.Error(w, err.Error(), http.StatusNotFound)
		}
	})
	http.HandleFunc("GET /cart/total", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, c.Total())
	})
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
EOF
mkdir cart
cart '	c.cents = c.cents * 9 / 10
' '' > cart/cart.go
cat > cart/cart_test.go <<'EOF'
package cart

import (
	"errors"
	"testing"
)

func TestTotalAddsTheItems(t *testing.T) {
	var c Cart
	c.Add(1999)
	c.Add(501)
	if got := c.Total(); got != "25.00" {
		t.Errorf("total is %s; want 25.00", got)
	}
}

func TestAnUnknownCouponIsAnError(t *testing.T) {
	var c Cart
	c.Add(10000)
	if err := c.ApplyCoupon("FREE"); !errors.Is(err, ErrUnknownCoupon) {
		t.Errorf("ApplyCoupon(FREE) returned %v; want ErrUnknownCoupon", err)
	}
	if got := c.Total(); got != "100.00" {
		t.Errorf("total is %s after an unknown coupon; want 100.00", got)
	}
}
EOF
mkdir e2e
cat > e2e/shop_test.go <<'EOF'
//go:build e2e

// Package e2e tests the shop's HTTP API on a running shop.
package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// call sends a request to the shop and returns the body. It fails the test
// when the status is not want.
func call(t *testing.T, method, path string, want int) string {
	t.Helper()
	req, err := http.NewRequest(method, "http://localhost:8080"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %s; want %d", method, path, resp.Status, want)
	}
	return strings.TrimSpace(string(body))
}

func TestCheckoutWithACoupon(t *testing.T) {
	call(t, "POST", "/cart/items?cents=10000", http.StatusOK)
	call(t, "POST", "/cart/coupon?code=SAVE10", http.StatusOK)
	if got := call(t, "GET", "/cart/total", http.StatusOK); got != "90.00" {
		t.Errorf("total is %s; want 90.00", got)
	}
}

func TestAnUnknownCouponIsNotFound(t *testing.T) {
	call(t, "POST", "/cart/coupon?code=FREE", http.StatusNotFound)
}
EOF
commit 2026-10-24T12:00:00Z "Add the shop API"

git switch -q -c test-only main
cat > cart/coupon_test.go <<'EOF'
package cart

import "testing"

// SAVE10 took 10% off again on every use, so two uses took 19% off.
func TestCouponAppliesOnce(t *testing.T) {
	var c Cart
	c.Add(10000)
	c.ApplyCoupon("SAVE10")
	c.ApplyCoupon("SAVE10")
	if got := c.Total(); got != "90.00" {
		t.Errorf("total after SAVE10 twice is %s; want 90.00", got)
	}
}
EOF
commit 2026-10-24T12:10:00Z "Reproduce: SAVE10 takes 10% off again on every use"

git switch -q -c coupon-once test-only
cart '	if c.applied[code] {
		return nil
	}
	if c.applied == nil {
		c.applied = map[string]bool{}
	}
	c.applied[code] = true
	c.cents = c.cents * 9 / 10
' "$applied" > cart/cart.go
commit 2026-10-24T12:20:00Z "Apply a coupon once per cart"

git switch -q -c late-check test-only
cart '	c.cents = c.cents * 9 / 10
	if c.applied[code] {
		return nil
	}
	if c.applied == nil {
		c.applied = map[string]bool{}
	}
	c.applied[code] = true
' "$applied" > cart/cart.go
commit 2026-10-24T12:20:00Z "Apply a coupon once per cart, with the check after the discount"

git switch -q -c api-break main
sed 's/Query().Get("code")/Query().Get("coupon")/' main.go > main.go.new
mv main.go.new main.go
commit 2026-10-24T12:30:00Z "Take the coupon code from the parameter coupon"

git switch -q test-only
echo "$repo"
