package main

import (
	"testing"
	"time"
)

// fakeUI models the WebView: dispatched work waits in a queue and runs when
// a wake-up message arrives. lose=true throws the next wake-up away, as
// Windows does while a window is being dragged or a menu is open.
type fakeUI struct {
	queue []func()
	lose  bool
}

func (u *fakeUI) dispatch(f func()) {
	u.queue = append(u.queue, f)
	if u.lose {
		u.lose = false
		return
	}
	q := u.queue
	u.queue = nil
	for _, f := range q {
		f()
	}
}

func TestCoalescerRecoversFromLostMessage(t *testing.T) {
	clock := time.Unix(1000, 0)
	ui := &fakeUI{}
	delivered := 0
	c := &coalescer{dispatch: ui.dispatch, deliver: func() { delivered++ }, now: func() time.Time { return clock }, retryAfter: time.Second}

	c.Trigger()
	if delivered != 1 {
		t.Fatalf("normal delivery: %d", delivered)
	}
	ui.lose = true // the user drags the window: this wake-up is lost
	c.Trigger()
	if delivered != 1 {
		t.Fatalf("lost message should not deliver yet: %d", delivered)
	}
	// Updates keep coming every half second (the engine's pace).
	for i := 0; i < 6; i++ {
		clock = clock.Add(500 * time.Millisecond)
		c.Trigger()
	}
	if delivered < 3 {
		t.Fatalf("the bar must start moving again after a lost message, got %d deliveries", delivered)
	}
	// Once recovered, updates don't pile up: one delivery per trigger.
	before := delivered
	for i := 0; i < 4; i++ {
		clock = clock.Add(500 * time.Millisecond)
		c.Trigger()
	}
	if delivered-before != 4 {
		t.Errorf("after recovery: %d deliveries for 4 updates", delivered-before)
	}
}

// Without the retry (the old behaviour) a single lost message froze the
// status for good. This documents the bug the coalescer fixes.
func TestOldCoalescingFroze(t *testing.T) {
	ui := &fakeUI{}
	delivered := 0
	c := &coalescer{dispatch: ui.dispatch, deliver: func() { delivered++ }, now: func() time.Time { return time.Unix(0, 0) }, retryAfter: 24 * time.Hour}
	ui.lose = true
	for i := 0; i < 10; i++ {
		c.Trigger()
	}
	if delivered != 0 {
		t.Errorf("expected the old freeze, got %d", delivered)
	}
}
