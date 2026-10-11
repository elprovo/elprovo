package stripe

import (
	"encoding/json"
	"reflect"
	"slices"
	"time"
)

type tx struct {
	acct       *account
	cfg        *config
	now        time.Time
	req        requestInfo
	deliveries []delivery
}

type requestInfo struct {
	id             string
	idempotencyKey string
	expand         []string
}

func (t *tx) clockNow(clockID nullString) int64 {
	if clock := find(t.acct.TestClocks, string(clockID)); clock != nil {
		return clock.FrozenTime
	}
	return t.now.Unix()
}

func snapshot(obj any) map[string]any {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	return tree
}

func (t *tx) emit(eventType string, obj any) {
	t.record(eventType, obj, nil)
}

func (t *tx) emitUpdate(eventType string, obj any, before map[string]any) {
	changed := previousAttributes(before, snapshot(obj))
	if len(changed) == 0 {
		return
	}
	t.record(eventType, obj, changed)
}

func (t *tx) record(eventType string, obj any, previous map[string]any) {
	raw, _ := json.Marshal(obj)
	e := &event{
		ID:         newID("evt"),
		Object:     "event",
		APIVersion: t.cfg.apiVersion,
		Created:    t.now.Unix(),
		Data:       eventData{Object: raw, PreviousAttributes: previous},
		Request:    eventRequest{ID: nullString(t.req.id), IdempotencyKey: nullString(t.req.idempotencyKey)},
		Type:       eventType,
	}
	for _, ep := range t.acct.WebhookEndpoints {
		if ep.receives(eventType) {
			e.PendingWebhooks++
			t.deliveries = append(t.deliveries, delivery{eventID: e.ID, endpointID: ep.ID})
		}
	}
	t.acct.Events = append(t.acct.Events, e)
	t.acct.pruneEvents(t.now)
}

func (e *webhookEndpoint) receives(eventType string) bool {
	if e.Status != "enabled" {
		return false
	}
	return slices.Contains(e.EnabledEvents, "*") || slices.Contains(e.EnabledEvents, eventType)
}

func previousAttributes(before, after map[string]any) map[string]any {
	changed := map[string]any{}
	for key, old := range before {
		current := after[key]
		if reflect.DeepEqual(old, current) {
			continue
		}
		oldHash, oldOK := old.(map[string]any)
		newHash, newOK := current.(map[string]any)
		if oldOK && newOK && oldHash["object"] == nil {
			changed[key] = previousAttributes(oldHash, newHash)
			continue
		}
		changed[key] = old
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			changed[key] = nil
		}
	}
	return changed
}
