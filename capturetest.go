package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CaptureReport measures what Windows itself delivers, before any encoding.
type CaptureReport struct {
	Frames    int     `json:"frames"`
	Seconds   float64 `json:"seconds"`
	FPS       float64 `json:"fps"`       // frames delivered per second
	MedianMs  float64 `json:"medianMs"`  // typical gap between frames
	P95Ms     float64 `json:"p95Ms"`     // 95% of gaps are shorter than this
	MaxMs     float64 `json:"maxMs"`     // longest gap
	SlowShare float64 `json:"slowShare"` // % of gaps longer than 1.5× the target interval
	Summary   string  `json:"summary"`
}

// CaptureTestArgs captures for `secs` seconds without repeating frames and
// prints each frame's capture timestamp. Nothing is encoded.
func CaptureTestArgs(src0 string, secs int) []string {
	// showinfo logs at info level.
	return []string{"-hide_banner", "-nostats", "-loglevel", "info",
		"-filter_complex", src0 + ",showinfo=checksum=0[v]",
		"-map", "[v]", "-t", strconv.Itoa(secs), "-f", "null", "-"}
}

// AnalyzeCaptureTimes turns capture timestamps (seconds) into a report.
func AnalyzeCaptureTimes(ts []float64, targetFPS int) CaptureReport {
	r := CaptureReport{Frames: len(ts)}
	if len(ts) < 3 {
		r.Summary = "Windows delivered almost no frames. Is the game minimized, or is the screen not changing?"
		return r
	}
	sort.Float64s(ts)
	r.Seconds = ts[len(ts)-1] - ts[0]
	if r.Seconds <= 0 {
		r.Seconds = 1
	}
	r.FPS = float64(len(ts)-1) / r.Seconds
	gaps := make([]float64, 0, len(ts)-1)
	slow := 0
	limit := 1.5 * 1000 / float64(targetFPS)
	for i := 1; i < len(ts); i++ {
		g := (ts[i] - ts[i-1]) * 1000
		gaps = append(gaps, g)
		if g > limit {
			slow++
		}
	}
	sort.Float64s(gaps)
	r.MedianMs = gaps[len(gaps)/2]
	r.P95Ms = gaps[len(gaps)*95/100]
	r.MaxMs = gaps[len(gaps)-1]
	r.SlowShare = float64(slow) * 100 / float64(len(gaps))
	verdict := "Windows is delivering frames smoothly, so any stutter comes from later in the pipeline."
	if r.FPS < float64(targetFPS)*0.9 || r.SlowShare > 5 {
		verdict = "Windows itself is delivering frames unevenly or too slowly for your frame rate, so the capture method is the bottleneck."
	}
	r.Summary = fmt.Sprintf("Windows delivered %.0f frames per second (typical gap %.1f ms, 95%% under %.1f ms, longest %.0f ms). %s",
		r.FPS, r.MedianMs, r.P95Ms, r.MaxMs, verdict)
	return r
}

// ParseShowinfoTimes pulls pts_time values out of ffmpeg showinfo output.
func ParseShowinfoTimes(out string) []float64 {
	var ts []float64
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, "pts_time:")
		if i < 0 || !strings.Contains(line, "Parsed_showinfo") {
			continue
		}
		f := strings.Fields(line[i+len("pts_time:"):])
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			ts = append(ts, v)
		}
	}
	return ts
}
