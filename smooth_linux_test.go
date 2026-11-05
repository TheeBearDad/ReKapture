//go:build linux

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSmoothness(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, vf string) string {
		p := filepath.Join(dir, name)
		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=60:d=6"}
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		args = append(args, "-c:v", "libopenh264", "-b:v", "10M", "-pix_fmt", "yuv420p", p)
		if out, err := exec.Command(testFFmpeg(), args...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return p
	}
	smooth := mk("smooth.mp4", "")
	// every 6th frame repeated (a 10/s hitch, like a starved capture)
	stutter := mk("stutter.mp4", "select='not(eq(mod(n\\,6)\\,0))',fps=60")
	still := filepath.Join(dir, "still.mp4")
	if out, err := exec.Command(testFFmpeg(), "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=gray:s=640x360:r=60:d=4",
		"-c:v", "libopenh264", "-pix_fmt", "yuv420p", still).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r, err := CheckSmoothness(testFFmpeg(), smooth, exec.Command, false)
	if err != nil || r.Percent > 0.5 || !r.Moving {
		t.Errorf("smooth clip: %+v %v", r, err)
	}
	r, err = CheckSmoothness(testFFmpeg(), stutter, exec.Command, false)
	if err != nil || r.Percent < 10 || r.Percent > 25 {
		t.Errorf("stuttering clip should show ~16%% repeats: %+v %v", r, err)
	}
	t.Logf("stutter: %s", r.Summary)
	r, _ = CheckSmoothness(testFFmpeg(), still, exec.Command, false)
	if r.Moving {
		t.Errorf("a still picture can't be judged: %+v", r)
	}
}

func TestHeavyStutterIsNotStill(t *testing.T) {
	// 30 new frames per second shown at 60: every other frame is a repeat,
	// so the median difference is 0 even though the picture is moving.
	d := make([]float64, 600)
	for i := range d {
		if i%2 == 0 {
			d[i] = 3.5
		}
	}
	r := AnalyzeDiffs(d, 60)
	if !r.Moving || r.UniqueFPS < 29 || r.UniqueFPS > 31 || r.Percent < 40 {
		t.Errorf("half-rate clip must be reported as stutter: %+v", r)
	}
	t.Log(r.Summary)
}

func TestCaptureTimes(t *testing.T) {
	var smooth, uneven []float64
	for i := 0; i < 600; i++ {
		smooth = append(smooth, float64(i)/120)
	}
	tt := 0.0
	for i := 0; i < 300; i++ {
		if i%3 == 0 {
			tt += 0.05 // a 50 ms gap every third frame
		} else {
			tt += 0.0167
		}
		uneven = append(uneven, tt)
	}
	if r := AnalyzeCaptureTimes(smooth, 60); r.FPS < 119 || r.SlowShare > 0 || !strings.Contains(r.Summary, "smoothly") {
		t.Errorf("smooth: %+v", r)
	}
	if r := AnalyzeCaptureTimes(uneven, 60); r.SlowShare < 30 || !strings.Contains(r.Summary, "bottleneck") {
		t.Errorf("uneven: %+v", r)
	}
	out := "[Parsed_showinfo_1 @ 0x1] n:   0 pts:      0 pts_time:0       duration:1\n[Parsed_showinfo_1 @ 0x1] n:   1 pts:  83333 pts_time:0.0083333 duration:1\nother"
	if ts := ParseShowinfoTimes(out); len(ts) != 2 || ts[1] != 0.0083333 {
		t.Errorf("parse: %v", ts)
	}
}

func TestCaptureTestArgsRunOnTestSource(t *testing.T) {
	out, _ := exec.Command(testFFmpeg(), CaptureTestArgs("testsrc2=size=320x240:rate=120,realtime", 2)...).CombinedOutput()
	r := AnalyzeCaptureTimes(ParseShowinfoTimes(string(out)), 60)
	if r.Frames < 200 || r.FPS < 110 {
		t.Errorf("capture test on a 120 fps source: %+v\n%s", r, string(out)[:min(400, len(out))])
	}
}
