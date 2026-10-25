#!/bin/sh
# Makes a repository whose test needs its app running, and prints its path.
# The CI workflow replays it through the action. Its elprovo.toml starts the
# app in Docker Compose in each run, and runs the e2e tests against it:
#
#   main   a service that greets every visitor with "hello"
#   fix    a test that it greets by name, and the fix (checked out)
set -eu
repo=$(mktemp -d "${TMPDIR:-/tmp}/elprovo-app.XXXXXX")
cd "$repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Demo GIT_AUTHOR_EMAIL=demo@example.invalid
export GIT_COMMITTER_NAME=Demo GIT_COMMITTER_EMAIL=demo@example.invalid

# commit commits every change with the date $1 and the message $2.
commit() {
	git add -A
	GIT_AUTHOR_DATE=$1 GIT_COMMITTER_DATE=$1 git commit -q -m "$2"
}

# service prints main.go. $1 is the body of greeting.
service() {
	cat <<EOF
// Command greet greets visitors over HTTP.
package main

import (
	"fmt"
	"log"
	"net/http"
)

// greeting returns the greeting for the visitor name.
func greeting(name string) string {
$1}

func main() {
	http.HandleFunc("GET /greet", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, greeting(r.URL.Query().Get("name")))
	})
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
EOF
}

git init -q -b main
cat > go.mod <<'EOF'
module example.com/greet

go 1.24
EOF
cat > elprovo.toml <<'EOF'
version = 2
pass_env = ["GOCACHE", "GOMODCACHE", "DOCKER_CONFIG"]

[tests]
files = ["**/*_test.go"]
run = "go test -json -count=1 -timeout=5m -tags=e2e {packages}"
report = "go-test-json"

[app]
start = "docker compose up --detach --build --wait"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "docker compose down --volumes --rmi local"
EOF
cat > Dockerfile <<'EOF'
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /greet .

FROM alpine:3.21
COPY --from=build /greet /greet
ENTRYPOINT ["/greet"]
EOF
cat > compose.yaml <<'EOF'
services:
  greet:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
EOF
service '	return "hello"
' > main.go
commit 2026-10-25T12:00:00Z "Add the greeting service"

git switch -q -c fix
mkdir e2e
cat > e2e/greet_test.go <<'EOF'
//go:build e2e

// Package e2e tests the greeting service while it runs.
package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGreetsByName(t *testing.T) {
	resp, err := http.Get("http://localhost:8080/greet?name=Ada")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(body)); got != "hello, Ada" {
		t.Errorf("GET /greet?name=Ada = %q; want %q", got, "hello, Ada")
	}
}
EOF
service '	return "hello, " + name
' > main.go
commit 2026-10-25T12:10:00Z "Greet visitors by name"

echo "$repo"
