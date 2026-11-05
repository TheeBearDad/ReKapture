//go:build linux

package main

import (
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeLayeredClip builds a clip like InstaReplay's "Mixed + separate" output:
// track 1 = game+mic mix, 2 = game (440 Hz), 3 = mic (660 Hz).
func makeLayeredClip(t *testing.T, path string, secs int) {
	t.Helper()
	d := strconv.Itoa(secs)
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:d=" + d,
		"-f", "lavfi", "-i", "sine=f=440:d=" + d, "-f", "lavfi", "-i", "sine=f=660:d=" + d,
		"-filter_complex", "[1:a][2:a]amix=inputs=2:normalize=0[m]",
		"-map", "0:v", "-map", "[m]", "-map", "1:a", "-map", "2:a",
		"-c:v", "libopenh264", "-g", "60", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ar", "48000",
		"-metadata:s:a:0", "title=Game + Mic", "-metadata:s:a:1", "title=Game audio", "-metadata:s:a:2", "title=Microphone",
		"-shortest", path}
	if out, err := exec.Command(testFFmpeg(), args...).CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, out)
	}
}

func maxVolume(t *testing.T, p string, track int) float64 {
	out, _ := exec.Command(testFFmpeg(), "-hide_banner", "-nostats", "-i", p, "-map", "0:a:"+strconv.Itoa(track),
		"-af", "volumedetect", "-f", "null", "-").CombinedOutput()
	m := regexp.MustCompile(`max_volume: (-?[\d.]+|-inf) dB`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no volumedetect output for track %d: %s", track, out)
	}
	if m[1] == "-inf" {
		return -200
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

func testEditor() *Editor {
	return &Editor{FFmpeg: testFFmpeg(), Command: exec.Command, NVENC: func() bool { return false }}
}

func TestParseProbe(t *testing.T) {
	dir := t.TempDir()
	for _, ext := range []string{".mkv", ".mp4"} {
		p := filepath.Join(dir, "clip"+ext)
		makeLayeredClip(t, p, 4)
		info, err := ProbeClip(testFFmpeg(), p, exec.Command)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(info.Duration-4) > 0.1 || info.Width != 640 || info.Height != 360 || info.VideoCodec != "h264" {
			t.Errorf("%s: bad info %+v", ext, info)
		}
		if len(info.Audio) != 3 || info.Audio[1].Title != "Game audio" || !info.Mixable {
			t.Errorf("%s: audio %+v mixable=%v", ext, info.Audio, info.Mixable)
		}
	}
	if _, err := ProbeClip(testFFmpeg(), filepath.Join(dir, "nope.mp4"), exec.Command); err == nil {
		t.Errorf("missing file should error")
	}
}

func TestEditTrimExactAndFast(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Game 2026-11-04 06-18-24 Replay.mp4")
	makeLayeredClip(t, src, 8)
	ed := testEditor()
	var last float64
	out, err := ed.Run(EditSpec{Path: src, Start: 1.5, End: 4.0, Exact: true}, func(p float64) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "Game 2026-11-04 06-18-24 Replay (edited).mp4" || last != 1 {
		t.Errorf("out=%s progress=%v", out, last)
	}
	info, _ := ProbeClip(testFFmpeg(), out, exec.Command)
	if math.Abs(info.Duration-2.5) > 0.1 {
		t.Errorf("exact trim duration %.3f, want 2.5", info.Duration)
	}
	if len(info.Audio) != 3 || info.Audio[2].Title != "Microphone" {
		t.Errorf("tracks/titles lost: %+v", info.Audio)
	}
	for _, st := range ffprobe(t, out).Streams { // audio and video must start and end together
		if atof(st.StartTime) > 0.05 || math.Abs(atof(st.Duration)-2.5) > 0.1 {
			t.Errorf("%s stream start %s duration %s, want 0 / 2.5", st.CodecType, st.StartTime, st.Duration)
		}
	}
	// Fast mode: stream copy snaps to the keyframe at or before the start.
	out2, err := ed.Run(EditSpec{Path: src, Start: 2.5, End: 6.0}, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out2, "(edited 2).mp4") {
		t.Errorf("second copy name: %s", out2)
	}
	i2, _ := ProbeClip(testFFmpeg(), out2, exec.Command)
	if i2.Duration < 3.4 || i2.Duration > 6.1 {
		t.Errorf("fast trim duration %.3f, want 3.5..6 (keyframe snap)", i2.Duration)
	}
}

func TestEditAudioRebuildsMix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mkv")
	makeLayeredClip(t, src, 4)
	before := maxVolume(t, src, 0)
	out, err := testEditor().Run(EditSpec{Path: src, Start: 0, End: 4, Exact: false,
		Tracks: []TrackEdit{{Index: 2, Mute: true}, {Index: 1, Volume: 0.5}}}, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if v := maxVolume(t, out, 2); v > -80 {
		t.Errorf("muted mic track should be silent, max %.1f dB", v)
	}
	game := maxVolume(t, out, 1)
	if orig := maxVolume(t, src, 1); math.Abs((orig-game)-6.02) > 1 {
		t.Errorf("game at 50%% should be ~6 dB quieter: %.1f -> %.1f", orig, game)
	}
	if mix := maxVolume(t, out, 0); math.Abs(mix-game) > 1 || math.Abs(mix-before) < 1 {
		t.Errorf("mixed track should now equal the quieter game track: mix %.1f game %.1f before %.1f", mix, game, before)
	}
}

func TestEditReplaceAndCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	makeLayeredClip(t, src, 6)
	out, err := testEditor().Run(EditSpec{Path: src, Start: 1, End: 3, Exact: true, Replace: true}, func(float64) {})
	if err != nil || out != src {
		t.Fatalf("replace: %v %s", err, out)
	}
	if info, _ := ProbeClip(testFFmpeg(), src, exec.Command); math.Abs(info.Duration-2) > 0.1 {
		t.Errorf("replaced file duration %.2f", info.Duration)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 1 {
		t.Errorf("leftover temp/backup files: %v", left)
	}

	big := filepath.Join(dir, "big.mp4")
	makeLayeredClip(t, big, 60)
	ed := testEditor()
	errc := make(chan error, 1)
	go func() {
		_, err := ed.Run(EditSpec{Path: big, Start: 0, End: 60, Exact: true}, func(float64) {})
		errc <- err
	}()
	time.Sleep(400 * time.Millisecond)
	if !ed.Busy() {
		t.Fatal("editor should be busy")
	}
	if _, err := ed.Run(EditSpec{Path: big, End: 5}, func(float64) {}); err == nil {
		t.Errorf("a second edit while busy should be refused")
	}
	ed.Cancel()
	if err := <-errc; err != ErrCanceled {
		t.Errorf("want canceled, got %v", err)
	}
	left, _ = filepath.Glob(filepath.Join(dir, "*.part"))
	if len(left) != 0 || fileExists(EditedPath(big)) && strings.Contains(EditedPath(big), "(edited 2)") {
		t.Errorf("cancel left files behind: %v", left)
	}
	if _, err := testEditor().Run(EditSpec{Path: big, Start: 5, End: 5.1}, func(float64) {}); err == nil {
		t.Errorf("tiny selection should be refused")
	}
}

func TestFilmstrip(t *testing.T) {
	root := t.TempDir()
	clip := filepath.Join(root, "a.mp4")
	makeLayeredClip(t, clip, 10)
	m, err := NewMediaServer(testFFmpeg(), t.TempDir(), exec.Command, func() []string { return []string{root} }, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	resp, err := http.Get(m.Base() + "/strip?p=" + url.QueryEscape(clip))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("strip: %v %v", err, resp)
	}
	resp.Body.Close()
	files, _ := filepath.Glob(filepath.Join(m.cacheDir, "*.strip.jpg"))
	if len(files) != 1 {
		t.Fatalf("no strip cached")
	}
	out, _ := exec.Command(testFFmpeg(), "-hide_banner", "-i", files[0]).CombinedOutput()
	if !regexp.MustCompile(`, (\d+)x72`).Match(out) {
		t.Errorf("strip should be 72 px tall: %s", out)
	}
	os.Remove(files[0])
}

func TestEditArgsNVENC(t *testing.T) {
	info := ClipInfo{Duration: 120, VideoCodec: "h264", Audio: []AudioTrack{{0, "Game audio", "aac"}}}
	a := strings.Join(EditArgs(info, EditSpec{Path: `C:\c.mp4`, Start: 10, End: 20, Exact: true}, `C:\o.mp4`, VideoEncoderFor("h264", true)), " ")
	for _, want := range []string{"-hwaccel auto -ss 10.000 -i C:\\c.mp4 -t 10.000", "-c:v h264_nvenc", "-c:a aac", "title=Game audio", "-movflags +faststart"} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %q in %s", want, a)
		}
	}
	if b := strings.Join(EditArgs(info, EditSpec{Path: "c.mp4", End: 5}, "o.mp4", nil), " "); !strings.Contains(b, "-c:v copy") || !strings.Contains(b, "-c:a copy") || strings.Contains(b, "hwaccel") {
		t.Errorf("fast mode should stream-copy: %s", b)
	}
}
