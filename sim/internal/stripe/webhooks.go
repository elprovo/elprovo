package stripe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	deliveryTimeout  = 10 * time.Second
	deliveryWorkers  = 4
	idleDispatchWait = time.Hour
	maxResponseDrain = 64 << 10
)

type delivery struct {
	eventID    string
	endpointID string
	attempt    int
	due        time.Time
}

type deliveryTarget struct {
	url    string
	secret string
	body   []byte
}

type dispatcher struct {
	client    *http.Client
	retries   []time.Duration
	log       *slog.Logger
	target    func(delivery) (deliveryTarget, bool)
	delivered func(delivery)

	mu    sync.Mutex
	queue []delivery
	wake  chan struct{}
}

func newDispatcher(retries []time.Duration, log *slog.Logger, target func(delivery) (deliveryTarget, bool), delivered func(delivery)) *dispatcher {
	return &dispatcher{
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		retries:   retries,
		log:       log,
		target:    target,
		delivered: delivered,
		wake:      make(chan struct{}, 1),
	}
}

func (d *dispatcher) enqueue(deliveries ...delivery) {
	if len(deliveries) == 0 {
		return
	}
	d.mu.Lock()
	d.queue = append(d.queue, deliveries...)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *dispatcher) run(ctx context.Context) {
	work := make(chan delivery)
	var wg sync.WaitGroup
	for range deliveryWorkers {
		wg.Go(func() {
			for dl := range work {
				d.attempt(ctx, dl)
			}
		})
	}
	defer func() {
		close(work)
		wg.Wait()
	}()
	for {
		dl, wait, ok := d.next(time.Now())
		if ok {
			select {
			case work <- dl:
				continue
			case <-ctx.Done():
				return
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-d.wake:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

func (d *dispatcher) next(now time.Time) (delivery, time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	wait := idleDispatchWait
	for i, dl := range d.queue {
		if !dl.due.After(now) {
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			return dl, 0, true
		}
		wait = min(wait, dl.due.Sub(now))
	}
	return delivery{}, wait, false
}

func (d *dispatcher) attempt(ctx context.Context, dl delivery) {
	target, ok := d.target(dl)
	if !ok {
		return
	}
	log := d.log.With("event", dl.eventID, "endpoint", dl.endpointID, "url", target.url, "attempt", dl.attempt+1)
	err := d.post(ctx, target)
	if err == nil {
		log.Info("webhook delivered")
		d.delivered(dl)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if dl.attempt >= len(d.retries) {
		log.Warn("webhook delivery gave up", "error", err)
		return
	}
	dl.due = time.Now().Add(d.retries[dl.attempt])
	dl.attempt++
	log.Warn("webhook delivery failed; will retry", "error", err, "retry_at", dl.due)
	d.enqueue(dl)
}

func (d *dispatcher) post(ctx context.Context, target deliveryTarget) error {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(target.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "Stripe/1.0 (+https://stripe.com/docs/webhooks)")
	req.Header.Set("Stripe-Signature", signatureHeader(target.secret, time.Now().Unix(), target.body))
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseDrain))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}

func signatureHeader(secret string, timestamp int64, body []byte) string {
	ts := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
