// Clip stutter analysis, used by the tests to check that recordings have no
// repeated frames (a "Check for stutter" button used this in 2.2.2-2.3.0).

package main

import (
	"bufio"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// SmoothReport says how many frames of a clip are repeats of the frame
// before them while the picture is moving — each one is a visible hitch.
type SmoothReport struct {
	Frames    int     `json:"frames"`
	UniqueFPS float64 `json:"uniqueFps"` // new (non-repeated) frames per second
	FPS       float64 `json:"fps"`       // the file's frame rate
	Hitches   int     `json:"hitches"`
	Percent   float64 `json:"percent"`
	LongestMs int     `json:"longestMs"` // longest run of repeated frames
	Moving    bool    `json:"moving"`    // false: not enough motion to judge
	Summary   string  `json:"summary"`
}

// SmoothArgs measures the average pixel difference between each frame and
// the one before it (0 = identical) over the first `secs` seconds.
func SmoothArgs(path string, secs int, hwaccel bool) []string {
	a := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	if hwaccel {
		a = append(a, "-hwaccel", "auto")
	}
	return append(a, "-t", strconv.Itoa(secs), "-i", path, "-map", "0:v:0",
		"-vf", "scale=480:-2,format=gray,tblend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-",
		"-f", "null", "-")
}

// AnalyzeDiffs turns per-frame differences into a report.
func AnalyzeDiffs(d []float64, fps float64) SmoothReport {
	r := SmoothReport{Frames: len(d)}
	if len(d) < 10 {
		r.Summary = "The clip is too short to check."
		return r
	}
	s := append([]float64(nil), d...)
	sort.Float64s(s)
	// Judge motion by the busiest frames: when more than half of the frames
	// are repeats (heavy stutter), the median is ~0 even while panning.
	p90 := s[len(s)*9/10]
	r.Moving = p90 > 0.5
	if fps <= 0 {
		fps = 60
	}
	r.FPS = fps
	if !r.Moving {
		r.Summary = "Not enough motion to judge. Record a few seconds of turning the camera in a game, then check that clip."
		return r
	}
	const same, moving = 0.1, 0.3
	run, longest := 0, 0
	for i, v := range d {
		if v < same {
			run++
			prevMoving := i > 0 && d[i-1] > moving
			nextMoving := i+1 < len(d) && d[i+1] > moving
			if prevMoving || nextMoving || run > 1 {
				r.Hitches++
			}
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	unique := 0
	for _, v := range d {
		if v >= same {
			unique++
		}
	}
	r.UniqueFPS = float64(unique) * fps / float64(len(d))
	r.LongestMs = int(float64(longest) * 1000 / fps)
	r.Percent = float64(r.Hitches) * 100 / float64(len(d))
	switch {
	case r.Percent < 0.5:
		r.Summary = fmt.Sprintf("Smooth: %.0f new frames per second (of %.0f), %d repeats out of %d frames.", r.UniqueFPS, fps, r.Hitches, r.Frames)
	case r.Percent < 3:
		r.Summary = fmt.Sprintf("Mostly smooth: %.0f new frames per second (of %.0f), %.1f%% repeats.", r.UniqueFPS, fps, r.Percent)
	default:
		r.Summary = fmt.Sprintf("Stutters: only %.0f new frames per second (of %.0f). %.0f%% of frames are repeats, longest freeze %d ms.", r.UniqueFPS, fps, r.Percent, r.LongestMs)
	}
	return r
}

// CheckSmoothness analyzes the first 30 seconds of a clip.
func CheckSmoothness(ffmpeg, path string, command func(string, ...string) *exec.Cmd, hwaccel bool) (SmoothReport, error) {
	info, err := ProbeClip(ffmpeg, path, command)
	if err != nil {
		return SmoothReport{}, err
	}
	cmd := command(ffmpeg, SmoothArgs(path, 30, hwaccel)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return SmoothReport{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return SmoothReport{}, err
	}
	var diffs []float64
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "lavfi.signalstats.YAVG="); i >= 0 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(line[i+len("lavfi.signalstats.YAVG="):]), 64); err == nil {
				diffs = append(diffs, v)
			}
		}
	}
	if err := cmd.Wait(); err != nil && len(diffs) == 0 {
		return SmoothReport{}, fmt.Errorf("couldn't read the clip: %s", lastLine(stderr.String()))
	}
	return AnalyzeDiffs(diffs, info.FPS), nil
}
