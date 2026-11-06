package main

import (
	"sync/atomic"
	"time"
)

// coalescer delivers the latest status to the UI thread at most once per
// pass, without piling up work when updates come faster than the UI can
// draw them.
//
// The WebView runs dispatched work when a Windows thread message arrives,
// and Windows throws thread messages away while a window is being moved or
// resized or a menu is open. A delivery can therefore get stuck until the
// next message arrives. If one has been outstanding for longer than
// retryAfter, Trigger dispatches again: that sends a fresh message, which
// also runs the stuck delivery.
type coalescer struct {
	dispatch   func(func()) // run f on the UI thread
	deliver    func()       // runs on the UI thread
	now        func() time.Time
	retryAfter time.Duration

	pending int32
	since   int64 // when the outstanding delivery was dispatched (unix ns)
}

func (c *coalescer) Trigger() {
	now := c.now().UnixNano()
	if atomic.CompareAndSwapInt32(&c.pending, 0, 1) {
		atomic.StoreInt64(&c.since, now)
		c.dispatch(c.run)
		return
	}
	if now-atomic.LoadInt64(&c.since) > int64(c.retryAfter) {
		atomic.StoreInt64(&c.since, now)
		c.dispatch(c.run) // the earlier one is stuck: nudge the UI thread
	}
}

func (c *coalescer) run() {
	atomic.StoreInt32(&c.pending, 0)
	c.deliver()
}
