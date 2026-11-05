//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlatform stands in for Windows: a test pattern instead of the screen,
// a beeping tone generator instead of WASAPI, and a fifo instead of a pipe.
type fakePlatform struct {
	dir      string
	mu       sync.Mutex
	notes    []string
	fg       ForegroundApp   // the window "in front"
	alive    map[uint32]bool // running processes
	resolved []Settings      // what each pipeline start asked to capture
	kinds    map[string]int  // audio sources opened, by kind
}

func (f *fakePlatform) ResolveCapture(s Settings) (CaptureSource, error) {
	f.mu.Lock()
	f.resolved = append(f.resolved, s)
	f.mu.Unlock()
	return CaptureSource{Kind: "test", Width: 640, Height: 360, Label: "test pattern"}, nil
}
func (f *fakePlatform) NewAudio(kind, id string) AudioSource {
	f.mu.Lock()
	if f.kinds == nil {
		f.kinds = map[string]int{}
	}
	f.kinds[kind]++
	f.mu.Unlock()
	return &beepSource{dir: f.dir, kind: kind}
}
func (f *fakePlatform) ForegroundApp() ForegroundApp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fg
}
func (f *fakePlatform) ProcessAlive(pid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}
func (f *fakePlatform) setForeground(app ForegroundApp, running bool) {
	f.mu.Lock()
	f.fg = app
	if f.alive == nil {
		f.alive = map[uint32]bool{}
	}
	f.alive[app.PID] = running
	f.mu.Unlock()
}
func (f *fakePlatform) Command(n string, a ...string) *exec.Cmd { return exec.Command(n, a...) }
func (f *fakePlatform) ForegroundName() string                  { return "Test: Game?" }
func (f *fakePlatform) HideDir(string)                          {}
func (f *fakePlatform) AfterStart(*exec.Cmd)                    {}
func (f *fakePlatform) Notify(kind, title, msg string) {
	f.mu.Lock()
	f.notes = append(f.notes, kind+": "+title+" — "+msg)
	f.mu.Unlock()
}

// beepSource emits a 1 kHz beep during the first 100 ms of every wall-clock
// second and silence otherwise, with random-ish gaps (like loopback does).
type beepSource struct {
	dir, kind string
	path      string
	ln        net.Listener
	stop      chan struct{}
	wg        sync.WaitGroup
}

func (b *beepSource) Open(t0 time.Time) (string, error) {
	// Same transport as the Windows app: a loopback TCP socket.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	b.ln = ln
	b.path = fmt.Sprintf("tcp://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	b.stop = make(chan struct{})
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		f, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer f.Close()
		al := NewAligner(48000, 2)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-tick.C:
			}
			el := time.Since(t0)
			if el < 0 {
				continue
			}
			expected := int64(el.Seconds() * 48000)
			// produce "captured" data for what is missing, except during gaps
			var cap []byte
			now := float64(time.Now().UnixMicro()) / 1e6
			fr := now - math.Floor(now)
			gap := int64(now)%2 == 0 && fr > 0.35 && fr < 0.75 // 400 ms of nothing every 2 s, like silent loopback
			if !gap {
				n := expected - al.Written()
				if n > 0 {
					cap = make([]byte, n*8)
					for k := int64(0); k < n; k++ {
						fr := al.Written() + k
						wall := float64(t0.UnixMicro())/1e6 + float64(fr)/48000
						var v float32
						if wall-math.Floor(wall) < 0.1 {
							v = float32(0.5 * math.Sin(2*math.Pi*1000*float64(fr)/48000))
						}
						binary.LittleEndian.PutUint32(cap[k*8:], math.Float32bits(v))
						binary.LittleEndian.PutUint32(cap[k*8+4:], math.Float32bits(v))
					}
				}
			}
			out := al.Process(cap, expected)
			if len(out) > 0 {
				if _, err := f.Write(out); err != nil {
					return
				}
			}
		}
	}()
	return b.path, nil
}

func (b *beepSource) Close() {
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
	if b.ln != nil {
		b.ln.Close() // unblocks a pending Accept
	}
	b.wg.Wait()
}

type probe struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		StartTime string `json:"start_time"`
		Duration  string `json:"duration"`
		Tags      map[string]string
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(t *testing.T, p string) probe {
	out, err := exec.Command(strings.TrimSuffix(testFFmpeg(), "ffmpeg")+"ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", p).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", p, err)
	}
	var pr probe
	json.Unmarshal(out, &pr)
	return pr
}

func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func newTestEngine(t *testing.T, mutate func(*Settings)) (*Engine, *fakePlatform, Settings) {
	return newTestEngineSrc(t, mutate, "testsrc2=size=640x360:rate=30,realtime") // paced like live capture
}

func newTestEngineSrc(t *testing.T, mutate func(*Settings), src string) (*Engine, *fakePlatform, Settings) {
	dir := t.TempDir()
	s := DefaultSettings(dir)
	s.OutputDir = filepath.Join(dir, "out")
	s.Mic = true
	s.ReplaySeconds = 10
	s.FPS = 30
	s.Codec = "h264"
	if mutate != nil {
		mutate(&s)
	}
	s.Normalize(DefaultSettings(dir))
	fp := &fakePlatform{dir: dir, fg: ForegroundApp{Shell: true}}
	e := &Engine{plat: fp, ffmpeg: testFFmpeg(), settings: s, quit: make(chan struct{}), replayOn: s.ReplayEnabled,
		videoTest: src,
		encTest:   testEncoder()}
	go e.loop()
	t.Cleanup(func() { e.Shutdown() })
	return e, fp, s
}

// GL_FFMPEG points the tests at a specific ffmpeg build — use the same
// version the app ships, so option changes between versions are caught.
func testFFmpeg() string {
	if p := os.Getenv("GL_FFMPEG"); p != "" {
		return p
	}
	return "ffmpeg"
}

func testEncoder() []string {
	out, _ := exec.Command(testFFmpeg(), "-hide_banner", "-encoders").Output()
	if strings.Contains(string(out), " libx264 ") {
		return []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
	}
	return []string{"-c:v", "mpeg4", "-q:v", "5", "-pix_fmt", "yuv420p"}
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	dl := time.Now().Add(d)
	for time.Now().Before(dl) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func findClips(t *testing.T, root, kind string) []string {
	var out []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(p, kind) && strings.HasSuffix(p, ".mp4") && !strings.Contains(p, BufferDirName) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestReplayBufferEndToEnd(t *testing.T) {
	e, fp, s := newTestEngine(t, nil)
	waitFor(t, "pipeline running", 10*time.Second, func() bool { return e.Status().State == "buffering" })
	// Fill more than the replay window so pruning kicks in.
	waitFor(t, "buffer full", 30*time.Second, func() bool { return e.Status().BufferSeconds >= 10 })
	time.Sleep(6 * time.Second)

	segs, _ := filepath.Glob(filepath.Join(e.buf.Dir(), "seg_*.mkv"))
	if len(segs) > 12 {
		t.Errorf("buffer not pruned: %d segments on disk", len(segs))
	}

	e.mu.Lock()
	t0 := e.t0
	e.mu.Unlock()
	e.SaveReplay()
	waitFor(t, "replay saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 })
	clips := findClips(t, s.OutputDir, "Replay")
	if len(clips) != 1 {
		t.Fatalf("expected 1 replay, found %v (notes %v)", clips, fp.notes)
	}
	if !strings.Contains(clips[0], filepath.Join("out", "Test Game")) {
		t.Errorf("per-game folder / sanitizing wrong: %s", clips[0])
	}
	pr := ffprobe(t, clips[0])
	dur := atof(pr.Format.Duration)
	if dur < 9.5 || dur > 14.5 {
		t.Errorf("replay duration %.2fs, want ~10-12s", dur)
	}
	var v, a int
	for _, st := range pr.Streams {
		if st.CodecType == "video" {
			v++
		} else if st.CodecType == "audio" {
			a++
		}
	}
	if v != 1 || a != 4 {
		t.Errorf("want 1 video + 4 audio tracks (mix, game, Discord, mic), got %d + %d", v, a)
	}
	checkSync(t, clips[0], dur, float64(t0.UnixMicro())/1e6)
	checkSmooth(t, clips[0])
	t.Logf("replay ok: %s %.2fs; notes: %v", filepath.Base(clips[0]), dur, fp.notes)
}

// checkSync verifies beeps (emitted at wall-clock whole seconds) land at
// whole-second boundaries of the video timeline, i.e. A/V share one clock.
func checkSync(t *testing.T, p string, dur, t0 float64) {
	// Video frame pts are wall-clock based too, so a frame's (pts + offset) ...
	// We measure the beep phase relative to the file start and compare it with
	// the video's first-frame wall-clock phase, recovered from frame count.
	out, _ := exec.Command(testFFmpeg(), "-hide_banner", "-nostats", "-i", p, "-map", "0:a:1",
		"-af", "silencedetect=n=-30dB:d=0.3", "-f", "null", "-").CombinedOutput()
	re := regexp.MustCompile(`silence_end: ([0-9.]+)`)
	var ends []float64
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		if v := atof(m[1]); v < dur-0.1 {
			ends = append(ends, v)
		}
	}
	if len(ends) < 5 {
		t.Fatalf("too few beeps detected: %v", ends)
	}
	// Beeps must be exactly 1 s apart (no audio stretching / gaps).
	for i := 1; i < len(ends); i++ {
		d := ends[i] - ends[i-1]
		if math.Abs(d-math.Round(d)) > 0.03 {
			t.Errorf("beep spacing %.3fs is not a whole second — audio timing broken (%v)", d, ends)
			break
		}
	}
	// A/V alignment: video keyframes (and so clip starts) sit on whole seconds of
	// the stream clock, which is wall-clock minus t0. A beep made at wall second W
	// must therefore appear at file time ≡ -t0 (mod 1) — within one video frame.
	want := math.Mod(-t0, 1)
	if want < 0 {
		want++
	}
	got := math.Mod(ends[0], 1)
	diff := math.Abs(got - want)
	if diff > 0.5 {
		diff = 1 - diff
	}
	if diff > 0.05 {
		t.Errorf("audio is %.0f ms off the video clock (beep phase %.3f, want %.3f)", diff*1000, got, want)
	} else {
		t.Logf("A/V offset %.1f ms", diff*1000)
	}
}

func TestRecordingEndToEnd(t *testing.T) {
	e, fp, s := newTestEngine(t, func(s *Settings) { s.ReplayEnabled = false; s.Container = "mp4"; s.AudioTracks = "separate" })
	if e.Status().State != "off" {
		t.Fatalf("should be off, got %s", e.Status().State)
	}
	e.StartRecording()
	waitFor(t, "recording", 10*time.Second, func() bool { return e.Status().State == "recording" && e.running })
	time.Sleep(7 * time.Second)
	e.StopRecording()
	waitFor(t, "recording saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 && e.Status().Finalizing == 0 })
	clips := findClips(t, s.OutputDir, "Recording")
	if len(clips) != 1 {
		t.Fatalf("expected one recording, got %v notes %v", clips, fp.notes)
	}
	pr := ffprobe(t, clips[0])
	dur := atof(pr.Format.Duration)
	if dur < 5 || dur > 9.5 {
		t.Errorf("recording duration %.2f, want ~6-8s", dur)
	}
	audio := 0
	for _, st := range pr.Streams {
		if st.CodecType == "audio" {
			audio++
		}
	}
	if audio != 3 {
		t.Errorf("separate tracks: want 3 audio (game, Discord, mic), got %d", audio)
	}
	// Pipeline should stop since replay is off.
	waitFor(t, "pipeline stopped", 10*time.Second, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.running })
	time.Sleep(1500 * time.Millisecond)
	left, _ := filepath.Glob(filepath.Join(s.OutputDir, BufferDirName, "*"))
	if len(left) != 0 {
		t.Errorf("temp buffer not cleaned up: %v", left)
	}
	t.Logf("recording ok: %.2fs", dur)
}

func TestRecordWhileReplayAndRecovery(t *testing.T) {
	e, _, s := newTestEngine(t, func(s *Settings) { s.ReplaySeconds = 4 })
	waitFor(t, "running", 10*time.Second, func() bool { return e.Status().State == "buffering" })
	time.Sleep(3 * time.Second)
	e.StartRecording()
	time.Sleep(9 * time.Second) // longer than the replay window: segments must be protected
	e.StopRecording()
	waitFor(t, "saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 && e.Status().Finalizing == 0 })
	if e.Status().State != "buffering" {
		t.Errorf("replay should keep running after recording, state=%s", e.Status().State)
	}
	clips := findClips(t, s.OutputDir, "Recording")
	if len(clips) != 1 {
		t.Fatalf("no recording")
	}
	if d := atof(ffprobe(t, clips[0]).Format.Duration); d < 8 || d > 12.5 {
		t.Errorf("recording during replay %.2fs, want ~9-11s (segments were pruned?)", d)
	}

	// Simulate a crash mid-recording: start, then kill ffmpeg hard and the engine.
	e.StartRecording()
	time.Sleep(5 * time.Second)
	e.mu.Lock()
	dir := e.buf.Dir()
	e.stopping = true // pretend the app died: no finalize
	e.recording = false
	cmd := e.cmd
	e.mu.Unlock()
	cmd.Process.Kill()
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(dir, "recording.json")); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	// A fresh launch doesn't own the old folder.
	e.mu.Lock()
	e.ownDirs = nil
	e.mu.Unlock()
	e.RecoverStale()
	waitFor(t, "recovered", 10*time.Second, func() bool {
		for _, c := range findClips(t, s.OutputDir, "Recording") {
			if strings.Contains(c, "interrupted") {
				return true
			}
		}
		return false
	})
}

func TestArgsAreValidForWindows(t *testing.T) {
	s := DefaultSettings(`C:\Users\T\Videos`)
	if s.Codec != "h264" {
		t.Errorf("default codec should be H.264, got %s", s.Codec)
	}
	s.Codec = "hevc"
	s.Mic = true
	args := CaptureArgs(s, CaptureSource{Kind: "monitor", Handle: 65537, Width: 2560, Height: 1440},
		[]AudioInput{{`\\.\pipe\a`, "Game audio"}, {`\\.\pipe\b`, "Microphone"}}, `C:\x\buf`, 1700000000000000, "")
	j := strings.Join(args, " ")
	if strings.Count(j, "-filter_complex") != 2 {
		t.Errorf("video and audio must be separate filter graphs: %s", j)
	}
	if strings.Contains(j, "thread_queue_size") {
		t.Errorf("-thread_queue_size is output-only in FFmpeg 8 and breaks inputs: %s", j)
	}
	if !strings.Contains(j, "setpts='PTS+if(eq(N\\,0)\\,st(0\\,(RTCTIME-1700000000000000)/(TB*1000000)-PTS)\\,ld(0))'") {
		t.Errorf("video must keep capture timestamps (one offset), got %s", j)
	}
	for _, want := range []string{"gfxcapture=hmonitor=65537:max_framerate=120:capture_cursor=1:width=2560:height=1440:resize_mode=", ":dup_frames=0,setpts",
		"-c:v hevc_nvenc", "-rc vbr -b:v 50M", "-f f32le -ar 48000 -ch_layout stereo -i", "-g 120", `C:\x\buf\seg_%06d.mkv`, "amix"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}

// checkSmooth fails if the video has duplicated frames. testsrc2 draws a
// different picture every frame, so any repeat is a stutter.
func checkSmooth(t *testing.T, p string) {
	t.Helper()
	out, err := exec.Command(testFFmpeg(), "-hide_banner", "-loglevel", "error", "-i", p, "-map", "0:v:0", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("framemd5: %v", err)
	}
	var prev string
	total, dups := 0, 0
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ", ")
		h := f[len(f)-1]
		if h == prev {
			dups++
		}
		prev = h
		total++
	}
	if total == 0 || float64(dups) > float64(total)*0.01 {
		t.Errorf("video stutters: %d of %d frames are duplicates", dups, total)
	} else {
		t.Logf("smoothness: %d duplicated of %d frames", dups, total)
	}
}

// A game running faster than the recording (here 81 fps into 60) must give
// 60 different frames every second: no repeats, only skipped extras.
func TestFasterSourceIntoSlowerRecording(t *testing.T) {
	e, fp, s := newTestEngineSrc(t, func(s *Settings) { s.FPS = 60; s.ReplaySeconds = 6 }, "testsrc2=size=640x360:rate=81,realtime")
	waitFor(t, "buffer full", 30*time.Second, func() bool { return e.Status().BufferSeconds >= 6 })
	e.SaveReplay()
	waitFor(t, "replay saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 })
	clips := findClips(t, s.OutputDir, "Replay")
	if len(clips) != 1 {
		t.Fatalf("no replay: %v", fp.notes)
	}
	checkSmooth(t, clips[0])
	r, err := CheckSmoothness(testFFmpeg(), clips[0], exec.Command, false)
	if err != nil || r.UniqueFPS < 58 {
		t.Errorf("want ~60 new frames per second: %+v %v", r, err)
	}
}

// When the screen stops changing, Windows delivers no frames at all. Audio
// must keep going, the gap must be filled with the last picture, and A/V
// must stay in sync afterwards.
func TestStillScreenKeepsAudioAndSync(t *testing.T) {
	e, fp, s := newTestEngineSrc(t, func(s *Settings) { s.ReplaySeconds = 12 },
		"testsrc2=size=640x360:rate=30,select='not(between(t,3,6))',realtime")
	waitFor(t, "pipeline running", 10*time.Second, func() bool { return e.Status().State == "buffering" })
	e.mu.Lock()
	t0 := e.t0
	e.mu.Unlock()
	time.Sleep(11 * time.Second)
	e.SaveReplay()
	waitFor(t, "replay saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 })
	clips := findClips(t, s.OutputDir, "Replay")
	if len(clips) != 1 {
		t.Fatalf("no replay: %v", fp.notes)
	}
	pr := ffprobe(t, clips[0])
	dur := atof(pr.Format.Duration)
	var vdur, adur float64
	for _, st := range pr.Streams {
		d := atof(st.Duration)
		if d == 0 {
			d = dur
		}
		if st.CodecType == "video" {
			vdur = d
		} else if adur == 0 {
			adur = d
		}
	}
	if dur < 9 || math.Abs(vdur-adur) > 0.5 {
		t.Errorf("clip %.2fs, video %.2fs vs audio %.2fs", dur, vdur, adur)
	}
	checkSync(t, clips[0], dur, float64(t0.UnixMicro())/1e6)
	t.Logf("still-screen clip %.2fs (video %.2f, audio %.2f)", dur, vdur, adur)
}

// Automatic capture + "only while playing": nothing runs on the desktop; a
// game starts the replay on its own window, clips go in the game's folder,
// and closing the game pauses replay again.
func TestAutoFollowsGame(t *testing.T) {
	e, fp, s := newTestEngine(t, func(s *Settings) { s.CaptureMode = "auto"; s.ReplayWhen = "games"; s.ReplaySeconds = 10 })
	time.Sleep(2 * time.Second)
	if st := e.Status().State; st != "standby" {
		t.Fatalf("no game open: state %q, want standby", st)
	}
	fp.setForeground(ForegroundApp{PID: 42, Exe: "CoolGame.exe", Title: "Cool Game™ v1.2",
		Path: `D:\SteamLibrary\steamapps\common\Cool Game\CoolGame.exe`}, true)
	waitFor(t, "replay to start for the game", 15*time.Second, func() bool { return e.Status().State == "buffering" })
	st := e.Status()
	if st.Game != "Cool Game" {
		t.Errorf("status game %q", st.Game)
	}
	fp.mu.Lock()
	last := fp.resolved[len(fp.resolved)-1]
	fp.mu.Unlock()
	if last.CaptureMode != "window" || last.WindowExe != "CoolGame.exe" {
		t.Errorf("should capture the game's window, asked for %s %q", last.CaptureMode, last.WindowExe)
	}
	if g := e.Settings().Games; len(g) != 1 || g[0].Exe != "CoolGame.exe" || !g[0].IsGame {
		t.Errorf("detected game not added to the list: %+v", g)
	}
	// Alt-tab to the desktop: still following.
	fp.setForeground(ForegroundApp{Shell: true}, true)
	fp.mu.Lock()
	fp.alive[42] = true
	fp.mu.Unlock()
	waitFor(t, "a few seconds of buffer", 20*time.Second, func() bool { return e.Status().BufferSeconds >= 4 })
	if e.Status().State != "buffering" {
		t.Fatal("alt-tab stopped the replay")
	}
	e.SaveReplay()
	waitFor(t, "replay saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 })
	if c := findClips(t, s.OutputDir, "Replay"); len(c) != 1 || !strings.Contains(c[0], filepath.Join("out", "Cool Game")) {
		t.Errorf("clip should be in the game's folder: %v", c)
	}
	// The game closes.
	fp.mu.Lock()
	fp.alive[42] = false
	fp.mu.Unlock()
	waitFor(t, "replay to pause", 15*time.Second, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.running && e.game == nil
	})
	if st := e.Status().State; st != "standby" {
		t.Errorf("after the game closed: state %q", st)
	}
	fp.mu.Lock()
	notes := strings.Join(fp.notes, "\n")
	fp.mu.Unlock()
	if !strings.Contains(notes, "Cool Game") || !strings.Contains(notes, "Instant Replay paused") {
		t.Errorf("notices: %s", notes)
	}
}

// Automatic capture with replay always on: the desktop is captured until a
// game starts, then capture switches to the game.
func TestAutoSwitchesFromDesktopToGame(t *testing.T) {
	e, fp, _ := newTestEngine(t, func(s *Settings) { s.CaptureMode = "auto"; s.MonitorID = `\\.\DISPLAY2` })
	waitFor(t, "desktop capture", 10*time.Second, func() bool { return e.Status().State == "buffering" })
	fp.mu.Lock()
	first := fp.resolved[0]
	fp.mu.Unlock()
	if first.CaptureMode != "monitor" || first.MonitorID != `\\.\DISPLAY2` {
		t.Errorf("no game: should capture the chosen monitor, got %+v", first.CaptureMode)
	}
	fp.setForeground(ForegroundApp{PID: 7, Exe: "Shooter.exe", Title: "Shooter", Fullscreen: true, Path: `C:\Games\Shooter.exe`}, true)
	waitFor(t, "switch to the game", 20*time.Second, func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		l := fp.resolved[len(fp.resolved)-1]
		return l.CaptureMode == "window" && l.WindowExe == "Shooter.exe"
	})
	waitFor(t, "buffering again", 10*time.Second, func() bool { return e.Status().State == "buffering" })
}

// Discord on its own track: the game source leaves Discord out, a Discord
// source is added, and the file gets mix + game + Discord + mic tracks.
func TestDiscordSeparateTrack(t *testing.T) {
	e, fp, s := newTestEngine(t, func(s *Settings) { s.ReplaySeconds = 10; s.CaptureMode = "monitor" })
	waitFor(t, "buffer", 30*time.Second, func() bool { return e.Status().BufferSeconds >= 5 })
	e.SaveReplay()
	waitFor(t, "replay saved", 20*time.Second, func() bool { return len(e.Status().Clips) == 1 })
	fp.mu.Lock()
	kinds := fp.kinds
	fp.mu.Unlock()
	if kinds["loopback-nodiscord"] != 1 || kinds["discord"] != 1 || kinds["mic"] != 1 || kinds["loopback"] != 0 {
		t.Errorf("audio sources: %v", kinds)
	}
	clip := findClips(t, s.OutputDir, "Replay")[0]
	info, err := ProbeClip(testFFmpeg(), clip, exec.Command)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, a := range info.Audio {
		titles = append(titles, a.Title)
	}
	if strings.Join(titles, "|") != "Game + Discord + Mic|Game audio|Discord|Microphone" || !info.Mixable || len(info.Parts) != 3 {
		t.Errorf("tracks %v mixable=%v parts=%v", titles, info.Mixable, info.Parts)
	}
	// The editor rebuilds the 3-way mix after a volume change.
	out := filepath.Join(filepath.Dir(clip), "edited.mp4")
	spec := EditSpec{Path: clip, Start: 1, End: 4, Tracks: []TrackEdit{{Index: 2, Volume: 0}}}
	args := EditArgs(info, spec, out, nil)
	if j := strings.Join(args, " "); !strings.Contains(j, "amix=inputs=3") {
		t.Errorf("edit should rebuild the 3-way mix: %s", j)
	}
	if b, err := exec.Command(testFFmpeg(), args...).CombinedOutput(); err != nil {
		t.Fatalf("edit failed: %v\n%s", err, b)
	}
	if info2, err := ProbeClip(testFFmpeg(), out, exec.Command); err != nil || len(info2.Audio) != 4 {
		t.Errorf("edited clip audio: %+v %v", info2.Audio, err)
	}
}
