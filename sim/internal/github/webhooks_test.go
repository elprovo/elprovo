package github

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var guidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPingDeliveryCarriesGitHubHeadersAndSignatures(t *testing.T) {
	ts := newTestSim(t)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)

	d := ts.hooks.wait(t, "ping", "")
	h := d.header
	if !guidPattern.MatchString(h.Get("X-GitHub-Delivery")) {
		t.Fatalf("X-GitHub-Delivery = %q, want a GUID", h.Get("X-GitHub-Delivery"))
	}
	if h.Get("X-GitHub-Hook-Installation-Target-Type") != "integration" || h.Get("X-GitHub-Hook-Installation-Target-ID") != strconv.FormatInt(a.ID, 10) {
		t.Fatalf("installation target headers = %q %q, want integration %d", h.Get("X-GitHub-Hook-Installation-Target-Type"), h.Get("X-GitHub-Hook-Installation-Target-ID"), a.ID)
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "GitHub-Hookshot/") || h.Get("Content-Type") != "application/json" || h.Get("X-GitHub-Hook-ID") == "" {
		t.Fatalf("headers = %v, want a GitHub-Hookshot user agent, JSON and a hook id", h)
	}
	if got, want := h.Get("X-Hub-Signature-256"), "sha256="+hmacHex("hook-secret", d.body); got != want {
		t.Fatalf("X-Hub-Signature-256 = %q, want %q", got, want)
	}
	mac := hmac.New(sha1.New, []byte("hook-secret"))
	mac.Write(d.body)
	if got, want := h.Get("X-Hub-Signature"), "sha1="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("X-Hub-Signature = %q, want %q", got, want)
	}
	var ping pingEvent
	d.decode(t, &ping)
	if ping.Hook.AppID != a.ID || ping.Hook.Type != "App" || ping.Zen == "" {
		t.Fatalf("ping payload = %+v, want hook of app %d", ping, a.ID)
	}
}

func TestFailedDeliveryIsLoggedAndRedeliveredWithTheSameGUID(t *testing.T) {
	ts := newTestSim(t)
	ts.hooks.answer(http.StatusInternalServerError)
	ts.createUser("octo")
	a := ts.createApp("octo", nil)
	first := ts.hooks.wait(t, "ping", "")
	failed := ts.waitForDelivery(t, a.Slug, func(d delivery) bool { return d.StatusCode == http.StatusInternalServerError })
	if failed.Error == "" || failed.Redelivery {
		t.Fatalf("failed delivery = %+v, want an error and no redelivery flag", failed)
	}

	ts.hooks.answer(http.StatusOK)
	var redo struct {
		ID   int64  `json:"id"`
		GUID string `json:"guid"`
	}
	ts.admin(http.MethodPost, fmt.Sprintf("/_sim/deliveries/%d/redeliver", failed.ID), nil, http.StatusAccepted).decode(t, &redo)

	second := ts.hooks.wait(t, "ping", "")
	if second.header.Get("X-GitHub-Delivery") != first.header.Get("X-GitHub-Delivery") || redo.GUID != failed.GUID {
		t.Fatalf("redelivery GUID = %q, want %q", second.header.Get("X-GitHub-Delivery"), first.header.Get("X-GitHub-Delivery"))
	}
	ok := ts.waitForDelivery(t, a.Slug, func(d delivery) bool { return d.ID == redo.ID && d.StatusCode == http.StatusOK })
	if !ok.Redelivery {
		t.Fatalf("redelivery %+v is not marked as a redelivery", ok)
	}
}

func (ts *testSim) waitForDelivery(t *testing.T, slug string, match func(delivery) bool) delivery {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var list []delivery
		ts.admin(http.MethodGet, "/_sim/deliveries?app="+slug, nil, http.StatusOK).decode(t, &list)
		for _, d := range list {
			if match(d) {
				return d
			}
		}
		select {
		case <-ts.hooks.notify:
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("no matching delivery was logged")
	return delivery{}
}
