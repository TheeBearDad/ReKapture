package main

import "testing"

func frames(n int) []byte { return make([]byte, n*8) }

func TestAlignerPadsSilence(t *testing.T) {
	a := NewAligner(48000, 2)
	out := a.Process(nil, 48000) // one second with no data
	if len(out) != 48000*8 || a.Written() != 48000 {
		t.Fatalf("want 1s of padding, got %d frames", len(out)/8)
	}
	// Small jitter within tolerance is passed through untouched.
	out = a.Process(frames(480), 48000+480+1000)
	if len(out) != 480*8 {
		t.Fatalf("jitter should pass through, got %d", len(out)/8)
	}
}

func TestAlignerDropsWhenAhead(t *testing.T) {
	a := NewAligner(48000, 2)
	out := a.Process(frames(10000), 4800)
	if a.Written() != 4800 || len(out) != 4800*8 {
		t.Fatalf("should trim to expected, wrote %d emitted %d", a.Written(), len(out)/8)
	}
	if out := a.Process(frames(100), 4800); out != nil && len(out) != 0 {
		// 4900 > 4800+2400? no -> passes through
	}
}

func TestAlignerLongRunStaysLocked(t *testing.T) {
	a := NewAligner(48000, 2)
	exp := int64(0)
	for i := 0; i < 100000; i++ { // ~16 min of 10 ms ticks, device 0.1% fast with silent stretches
		exp += 480
		n := 481
		if i%3000 < 500 {
			n = 0
		}
		a.Process(frames(n), exp)
		d := a.Written() - exp
		if d > a.Tolerance+481 || d < -a.Tolerance-481 {
			t.Fatalf("drifted %d frames at tick %d", d, i)
		}
	}
}
