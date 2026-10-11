package stripe

import "time"

const testClockLifetime = 7 * 24 * time.Hour

func (t *tx) createTestClock(frozenTime int64, name string) (*testClock, error) {
	if frozenTime == 0 {
		return nil, missingParam("frozen_time")
	}
	clock := &testClock{
		ID:           newID("clock"),
		Object:       "test_helpers.test_clock",
		Created:      t.now.Unix(),
		DeletesAfter: t.now.Add(testClockLifetime).Unix(),
		FrozenTime:   frozenTime,
		Name:         nullString(name),
		Status:       "ready",
	}
	t.acct.TestClocks = append(t.acct.TestClocks, clock)
	t.emit("test_helpers.test_clock.created", clock)
	return clock, nil
}

func (t *tx) advanceTestClock(id string, target int64) (*testClock, error) {
	clock := find(t.acct.TestClocks, id)
	if clock == nil {
		return nil, noSuch("test_clock", id, "id")
	}
	if target == 0 {
		return nil, missingParam("frozen_time")
	}
	if target <= clock.FrozenTime {
		return nil, invalidRequest("frozen_time", "The test clock can only advance to a time after its current frozen_time (%d).", clock.FrozenTime)
	}
	if limit := t.advanceLimit(clock); target > limit {
		return nil, invalidRequest("frozen_time",
			"Cannot advance the test clock past %d: at most two intervals of its shortest subscription, or two years when it has none.", limit)
	}
	clock.Status = "advancing"
	t.emit("test_helpers.test_clock.advancing", clock)
	for {
		action, at := t.nextClockAction(clock.ID, target)
		if action == nil {
			break
		}
		clock.FrozenTime = at
		action()
	}
	clock.FrozenTime = target
	reply := *clock
	clock.Status = "ready"
	t.emit("test_helpers.test_clock.ready", clock)
	return &reply, nil
}

func (t *tx) advanceLimit(clock *testClock) int64 {
	frozen := time.Unix(clock.FrozenTime, 0).UTC()
	limit := frozen.AddDate(2, 0, 0).Unix()
	for _, sub := range t.acct.Subscriptions {
		if string(sub.TestClock) != clock.ID || sub.Status == "canceled" {
			continue
		}
		interval, count := sub.interval()
		limit = min(limit, periodBoundary(frozen, interval, count, 2).Unix())
	}
	return limit
}

func (t *tx) nextClockAction(clockID string, target int64) (func(), int64) {
	var action func()
	at := target + 1
	for _, inv := range t.acct.Invoices {
		due := int64(inv.NextPaymentAttempt)
		if string(inv.TestClock) == clockID && inv.Status == "open" && due != 0 && due < at {
			action, at = func() { t.retryInvoice(inv) }, due
		}
	}
	for _, sub := range t.acct.Subscriptions {
		if string(sub.TestClock) == clockID && sub.renews() && sub.CurrentPeriodEnd < at {
			action, at = func() { t.endPeriod(sub) }, sub.CurrentPeriodEnd
		}
	}
	return action, at
}
