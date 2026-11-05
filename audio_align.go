package main

// Aligner keeps a captured audio stream locked to the wall clock.
//
// WASAPI loopback delivers nothing while no sound is playing, and device
// clocks drift slightly from the system clock. The aligner inserts silence
// for gaps and drops samples when the stream runs ahead, so the audio track
// always stays within `tolerance` frames of real time — which keeps it in
// sync with the video timestamps.
type Aligner struct {
	Rate      int   // frames per second
	Channels  int   // interleaved channels
	Tolerance int64 // frames
	written   int64 // frames emitted so far
}

func NewAligner(rate, channels int) *Aligner {
	return &Aligner{Rate: rate, Channels: channels, Tolerance: int64(rate / 20)} // 50 ms
}

func (a *Aligner) Written() int64 { return a.written }

// Process takes captured float32 samples (interleaved, as raw bytes) and the
// number of frames that *should* exist by now, and returns what to emit.
func (a *Aligner) Process(captured []byte, expected int64) []byte {
	bpf := 4 * a.Channels
	n := int64(len(captured) / bpf)
	captured = captured[:n*int64(bpf)]
	if expected < 0 {
		return nil
	}
	after := a.written + n
	switch {
	case after < expected-a.Tolerance:
		// Behind (silence or a device hiccup): pad with zeros before the new data.
		pad := expected - after
		out := make([]byte, int(pad)*bpf+len(captured))
		copy(out[int(pad)*bpf:], captured)
		a.written = expected
		return out
	case after > expected+a.Tolerance:
		// Ahead (clock drift or a burst): drop the oldest frames.
		drop := after - expected
		if drop >= n {
			return nil
		}
		a.written = expected
		return captured[drop*int64(bpf):]
	}
	a.written = after
	return captured
}
