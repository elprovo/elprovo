package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/state"
)

const (
	deliveryTimeout = 10 * time.Second
	deliveryWorkers = 4
	deliveryQueue   = 256
	maxDeliveries   = 1000
	hookUserAgent   = "GitHub-Hookshot/stagely-sim"
)

type delivery struct {
	ID             int64           `json:"id"`
	GUID           string          `json:"guid"`
	AppID          int64           `json:"app_id"`
	InstallationID int64           `json:"installation_id,omitempty"`
	Event          string          `json:"event"`
	Action         string          `json:"action,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	Redelivery     bool            `json:"redelivery"`
	URL            string          `json:"url"`
	StatusCode     int             `json:"status_code,omitempty"`
	Error          string          `json:"error,omitempty"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
	DurationMS     int64           `json:"duration_ms,omitempty"`
}

func (t *tx) recordDelivery(a *app, installationID int64, event, action string, payload any) {
	if a.WebhookURL == "" {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	d := &delivery{
		ID:             t.w.newID(),
		GUID:           newGUID(),
		AppID:          a.ID,
		InstallationID: installationID,
		Event:          event,
		Action:         action,
		Payload:        raw,
		URL:            a.WebhookURL,
	}
	t.w.appendDelivery(d)
	t.deliveries = append(t.deliveries, d.ID)
}

func (w *world) appendDelivery(d *delivery) {
	w.Deliveries = append(w.Deliveries, d)
	if over := len(w.Deliveries) - maxDeliveries; over > 0 {
		w.Deliveries = w.Deliveries[over:]
	}
}

func (w *world) delivery(id int64) *delivery {
	for _, d := range w.Deliveries {
		if d.ID == id {
			return d
		}
	}
	return nil
}

type dispatcher struct {
	store  *state.Store[world]
	log    *slog.Logger
	client *http.Client
	queue  chan int64
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func newDispatcher(store *state.Store[world], log *slog.Logger) *dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	d := &dispatcher{
		store:  store,
		log:    log,
		client: &http.Client{Timeout: deliveryTimeout},
		queue:  make(chan int64, deliveryQueue),
		ctx:    ctx,
		cancel: cancel,
	}
	for range deliveryWorkers {
		d.wg.Go(d.work)
	}
	return d
}

func (d *dispatcher) enqueue(ids ...int64) {
	for _, id := range ids {
		select {
		case d.queue <- id:
		case <-d.ctx.Done():
			return
		default:
			d.finish(id, 0, "delivery queue is full", 0)
		}
	}
}

func (d *dispatcher) close() {
	d.once.Do(func() {
		d.cancel()
		d.wg.Wait()
	})
}

func (d *dispatcher) work() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case id := <-d.queue:
			d.deliver(id)
		}
	}
}

type outgoing struct {
	url, secret, event, guid string
	hookID, appID            int64
	payload                  []byte
}

func (d *dispatcher) deliver(id int64) {
	var out outgoing
	d.store.Read(func(w *world) {
		del := w.delivery(id)
		if del == nil {
			return
		}
		a := w.app(del.AppID)
		if a == nil {
			return
		}
		out = outgoing{
			url: a.WebhookURL, secret: a.WebhookSecret, event: del.Event, guid: del.GUID,
			hookID: a.HookID, appID: a.ID, payload: del.Payload,
		}
	})
	if out.url == "" {
		return
	}
	start := time.Now()
	code, err := d.post(out)
	msg := ""
	if err != nil {
		msg = err.Error()
		d.log.Warn("webhook delivery failed", "event", out.event, "delivery", out.guid, "error", err)
	}
	d.finish(id, code, msg, time.Since(start))
}

func (d *dispatcher) post(o outgoing) (int, error) {
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, o.url, bytes.NewReader(o.payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", hookUserAgent)
	req.Header.Set("X-GitHub-Event", o.event)
	req.Header.Set("X-GitHub-Delivery", o.guid)
	req.Header.Set("X-GitHub-Hook-ID", strconv.FormatInt(o.hookID, 10))
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(o.appID, 10))
	if o.secret != "" {
		req.Header.Set("X-Hub-Signature", "sha1="+sign(sha1.New, o.secret, o.payload))
		req.Header.Set("X-Hub-Signature-256", "sha256="+sign(sha256.New, o.secret, o.payload))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver answered %s", resp.Status)
	}
	return resp.StatusCode, nil
}

func (d *dispatcher) finish(id int64, code int, msg string, took time.Duration) {
	err := d.store.Update(func(w *world) error {
		del := w.delivery(id)
		if del == nil {
			return nil
		}
		at := now()
		del.StatusCode, del.Error, del.DeliveredAt, del.DurationMS = code, msg, &at, took.Milliseconds()
		return nil
	})
	if err != nil {
		d.log.Error("record webhook delivery", "error", err)
	}
}

func sign(h func() hash.Hash, secret string, payload []byte) string {
	mac := hmac.New(h, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
