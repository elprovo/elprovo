#!/bin/sh
# Makes the demo repository that the README's examples and assets/demo.gif
# come from, and prints its path. It holds a small shop API whose SAVE10
# coupon takes 10% off again on every use, and three changes to it:
#
#   coupon-once   fixes the bug, with its proof in proofs/ (checked out)
#   late-check    tries to fix it, but checks only after the discount
#   outside-test  fixes the bug, but keeps the proof's test in tests/
#
# Fixed names and dates give the same commit IDs on every machine.
set -eu
repo=$(mktemp -d "${TMPDIR:-/tmp}/elprovo-demo.XXXXXX")
cd "$repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Demo GIT_AUTHOR_EMAIL=demo@example.invalid GIT_AUTHOR_DATE=2026-10-24T12:00:00Z
export GIT_COMMITTER_NAME=Demo GIT_COMMITTER_EMAIL=demo@example.invalid GIT_COMMITTER_DATE=2026-10-24T12:00:00Z

fix() {
	cat <<'EOF'
diff --git a/main.go b/main.go
index b5a4f00..ba583b3 100644
--- a/main.go
+++ b/main.go
@@ -11,12 +11,13 @@ import (
 )
 
 type cart struct {
-	mu    sync.Mutex
-	cents int64
+	mu      sync.Mutex
+	cents   int64
+	applied map[string]bool
 }
 
 func main() {
-	var c cart
+	c := cart{applied: map[string]bool{}}
 	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
 		fmt.Fprintln(w, "ok")
 	})
@@ -37,6 +38,10 @@ func main() {
 		}
 		c.mu.Lock()
 		defer c.mu.Unlock()
+		if c.applied["SAVE10"] {
+			return
+		}
+		c.applied["SAVE10"] = true
 		c.cents = c.cents * 9 / 10
 	})
 	http.HandleFunc("GET /cart/total", func(w http.ResponseWriter, r *http.Request) {
EOF
}

late_check() {
	cat <<'EOF'
diff --git a/main.go b/main.go
index b5a4f00..42470a9 100644
--- a/main.go
+++ b/main.go
@@ -11,12 +11,13 @@ import (
 )
 
 type cart struct {
-	mu    sync.Mutex
-	cents int64
+	mu      sync.Mutex
+	cents   int64
+	applied map[string]bool
 }
 
 func main() {
-	var c cart
+	c := cart{applied: map[string]bool{}}
 	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
 		fmt.Fprintln(w, "ok")
 	})
@@ -38,6 +39,10 @@ func main() {
 		c.mu.Lock()
 		defer c.mu.Unlock()
 		c.cents = c.cents * 9 / 10
+		if c.applied["SAVE10"] {
+			return
+		}
+		c.applied["SAVE10"] = true
 	})
 	http.HandleFunc("GET /cart/total", func(w http.ResponseWriter, r *http.Request) {
 		c.mu.Lock()
EOF
}

check() {
	cat <<'EOF'
set -eu
curl -fsS -X POST 'http://localhost:8080/cart/items?price=100.00'
curl -fsS -X POST 'http://localhost:8080/cart/coupon?code=SAVE10'
curl -fsS -X POST 'http://localhost:8080/cart/coupon?code=SAVE10'
total=$(curl -fsS http://localhost:8080/cart/total)
[ "$total" = 90.00 ] || { echo "FAIL: the total is $total after SAVE10 twice, not 90.00"; exit 1; }
EOF
}

claim='claim = "A coupon applies once per cart, even when the shop gets it twice."'

git init -q -b main
cat > go.mod <<'EOF'
module example.com/shop

go 1.24
EOF
printf '/bin/\nshop.pid\n.elprovo/\n' > .gitignore
cat > elprovo.toml <<'EOF'
version = 1
start = "go build -o bin/shop . && { ./bin/shop & echo $! > shop.pid; }"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "kill $(cat shop.pid)"
EOF
cat > main.go <<'EOF'
// Command shop is a small shop API with one cart.
package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
)

type cart struct {
	mu    sync.Mutex
	cents int64
}

func main() {
	var c cart
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("POST /cart/items", func(w http.ResponseWriter, r *http.Request) {
		price, err := strconv.ParseFloat(r.URL.Query().Get("price"), 64)
		if err != nil || price <= 0 {
			http.Error(w, "price must be a positive number", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cents += int64(math.Round(price * 100))
	})
	http.HandleFunc("POST /cart/coupon", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("code") != "SAVE10" {
			http.Error(w, "unknown coupon", http.StatusNotFound)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cents = c.cents * 9 / 10
	})
	http.HandleFunc("GET /cart/total", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		fmt.Fprintf(w, "%d.%02d\n", c.cents/100, c.cents%100)
	})
	log.Fatal(http.ListenAndServe("localhost:8080", nil))
}
EOF
git add -A && git commit -q -m "Add the shop API"

git switch -q -c coupon-once main
fix | git apply
mkdir proofs
{ echo "$claim"; echo "run = '''"; check; echo "'''"; } > proofs/coupon-once.toml
git add -A && git commit -q -m "Apply a coupon once per cart"

git switch -q -c late-check main
late_check | git apply
mkdir proofs
{ echo "$claim"; echo "run = '''"; check; echo "'''"; } > proofs/coupon-once.toml
git add -A && git commit -q -m "Apply a coupon once per cart, with the check after the discount"

git switch -q -c outside-test main
fix | git apply
mkdir proofs tests
check > tests/coupon-once.sh
{ echo "$claim"; echo 'run = "sh tests/coupon-once.sh"'; } > proofs/coupon-once.toml
git add -A && git commit -q -m "Apply a coupon once per cart, with the test in tests/"

git switch -q coupon-once
echo "$repo"
