//go:build linux

package main

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScreenshotArgs(t *testing.T) {
	src := ScreenshotSource(CaptureSource{Kind: "monitor", Handle: 65537, Width: 3840, Height: 2160})
	for _, want := range []string{"hmonitor=65537", "capture_cursor=0", "hwdownload,format=bgra"} {
		if !strings.Contains(src, want) {
			t.Errorf("source %q lacks %q", src, want)
		}
	}
	// Full resolution: no scaling, cropping or dropped repeats.
	for _, bad := range []string{"width=", "height=", "crop_", "dup_frames=0", "scale"} {
		if strings.Contains(src, bad) {
			t.Errorf("source %q should not contain %q", src, bad)
		}
	}
	png := strings.Join(ScreenshotArgs(src, "a.png", "png"), " ")
	if !strings.Contains(png, "format=rgb24[v]") || !strings.Contains(png, "-frames:v 1") || !strings.Contains(png, "-c:v png") {
		t.Errorf("png args: %s", png)
	}
	jpg := strings.Join(ScreenshotArgs(src, "a.jpg", "jpg"), " ")
	if !strings.Contains(jpg, "yuvj444p") || !strings.Contains(jpg, "-c:v mjpeg") {
		t.Errorf("jpg args: %s", jpg)
	}
}

func decodeImage(t *testing.T, p string) (image.Image, string) {
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return img, format
}

func TestEngineScreenshot(t *testing.T) {
	for _, format := range []string{"png", "jpg"} {
		t.Run(format, func(t *testing.T) {
			e, fp, s := newTestEngineSrc(t, func(s *Settings) {
				s.ReplayEnabled = false // screenshots don't need Instant Replay
				s.ScreenshotFormat = format
				s.FolderPerGame = true
				s.Resolution = "1080" // a recording setting; screenshots stay native
			}, "testsrc2=size=1920x1080:rate=30")
			fp.setForeground(ForegroundApp{Exe: "notepad.exe", Title: "notes", MonitorID: `\\.\DISPLAY2`, PID: 9}, true)
			start := time.Now()
			e.Screenshot()
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("screenshot took %v", d)
			}
			if want := filepath.Join(filepath.Dir(filepath.Dir(s.OutputDir)), "Pictures", "Screenshots"); s.ScreenshotDir != want {
				t.Errorf("default folder %s, want %s", s.ScreenshotDir, want)
			}
			shots, _ := filepath.Glob(filepath.Join(s.ScreenshotDir, "*", "*Screenshot."+format))
			if len(shots) != 1 {
				all, _ := filepath.Glob(filepath.Join(s.ScreenshotDir, "*", "*"))
				t.Fatalf("want 1 screenshot, found %v; notes %v", all, fp.notes)
			}
			img, got := decodeImage(t, shots[0])
			if b := img.Bounds(); b.Dx() != 1920 || b.Dy() != 1080 {
				t.Errorf("size %v, want full 1920x1080", b)
			}
			if want := map[string]string{"png": "png", "jpg": "jpeg"}[format]; got != want {
				t.Errorf("format %s, want %s", got, want)
			}
			// Fully opaque everywhere (alpha from the capture must not leak).
			if _, _, _, a := img.At(10, 10).RGBA(); a != 0xffff {
				t.Errorf("pixel not opaque: alpha %x", a)
			}
			// Captured the monitor the window in front is on, as a monitor capture.
			fp.mu.Lock()
			last := fp.resolved[len(fp.resolved)-1]
			notes := strings.Join(fp.notes, "\n")
			fp.mu.Unlock()
			if last.CaptureMode != "monitor" || last.MonitorID != `\\.\DISPLAY2` {
				t.Errorf("captured %s %s", last.CaptureMode, last.MonitorID)
			}
			if !strings.Contains(notes, "shot: Screenshot saved") {
				t.Errorf("notes: %s", notes)
			}
			clips := e.Status().Shots
			if len(clips) == 0 || clips[0].Kind != "Screenshot" || clips[0].Path != shots[0] {
				t.Errorf("library: %+v", clips)
			}
			if clipsDir, _ := filepath.Glob(filepath.Join(s.OutputDir, "*")); len(clipsDir) != 0 {
				t.Errorf("screenshot went to the clips folder: %v", clipsDir)
			}
			if leftover, _ := filepath.Glob(filepath.Join(s.ScreenshotDir, "*", "*.part.*")); len(leftover) != 0 {
				t.Errorf("temp files left: %v", leftover)
			}
		})
	}
}

func TestScreenshotFailureAndBusy(t *testing.T) {
	e, fp, s := newTestEngineSrc(t, func(s *Settings) { s.ReplayEnabled = false }, "nosuchsource=x")
	e.Screenshot()
	fp.mu.Lock()
	notes := strings.Join(fp.notes, "\n")
	fp.mu.Unlock()
	if !strings.Contains(notes, "warn: Screenshot failed") {
		t.Errorf("want a failure notice, got %s", notes)
	}
	if files, _ := filepath.Glob(filepath.Join(s.ScreenshotDir, "*")); len(files) != 0 {
		t.Errorf("failed screenshot left files: %v", files)
	}
	// A press while one is saving is ignored, not queued.
	e.mu.Lock()
	e.shooting = true
	e.mu.Unlock()
	count := func() int { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.notes) }
	before := count()
	e.Screenshot()
	if count() != before {
		t.Errorf("busy press should do nothing")
	}
}

func TestScreenshotSettings(t *testing.T) {
	d := DefaultSettings("/v")
	if d.ScreenshotFormat != "png" || HotkeyString(d.HotkeyShot) != "Alt+Shift+F1" {
		t.Errorf("defaults: %s %s", d.ScreenshotFormat, HotkeyString(d.HotkeyShot))
	}
	var old Settings // a settings file from before 2.8.0
	old.Normalize(d)
	d.Normalize(DefaultSettings("/v"))
	if old.ScreenshotFormat != "png" || old.HotkeyShot != d.HotkeyShot || old.ScreenshotDir != d.ScreenshotDir || old.ShotIconColor != "#0A72F0" || old.ShotIconOpacity != 90 {
		t.Errorf("upgrade: %+v %+v", old.ScreenshotFormat, old.HotkeyShot)
	}
	if got := d.PadXbox["shot"]; got == nil || len(got) != 0 {
		t.Errorf("controller screenshot should start unset, got %v", got)
	}
	d.PadPS["shot"] = []string{"Touchpad", "Options"}
	d.Normalize(DefaultSettings("/v"))
	if b := PadBindingsOf(d)[PadPS]["shot"]; b == 0 {
		t.Errorf("controller screenshot binding lost")
	}
}

func TestScanClipsScreenshots(t *testing.T) {
	root := t.TempDir()
	write := func(name string, age time.Duration) {
		p := filepath.Join(root, name)
		os.WriteFile(p, []byte("data"), 0o644)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	write("Game 2026-11-27 10-00-00 Screenshot.png", 0)
	write("Game 2026-11-27 09-00-00 Screenshot.jpg", time.Minute)
	write("Game 2026-11-27 08-00-00 Replay.mp4", time.Hour)
	write("Game 2026-11-27 10-01-00 Screenshot.part.png", 0) // still being written
	write("wallpaper.gif", 0)
	got := ScanClips(root, 12)
	if len(got) != 3 || got[0].Kind != "Screenshot" || got[1].Kind != "Screenshot" || got[2].Kind != "Replay" {
		t.Fatalf("got %+v", got)
	}
}

func TestScanLibrary(t *testing.T) {
	clips, shots := t.TempDir(), t.TempDir()
	write := func(p string, age time.Duration) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("data"), 0o644)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	write(filepath.Join(clips, "Game", "Game 2026-11-27 08-00-00 Replay.mp4"), 3*time.Minute)
	write(filepath.Join(clips, "Game", "Game 2026-11-27 07-00-00 Screenshot.png"), 4*time.Minute) // saved by 2.8.0
	write(filepath.Join(shots, "Game", "Game 2026-11-27 10-00-00 Screenshot.png"), time.Minute)
	write(filepath.Join(shots, "Desktop 2026-11-27 10-05-00 Screenshot (2).jpg"), 2*time.Minute)
	write(filepath.Join(shots, "Screenshot 2026-11-27 101500.png"), 0) // Windows' own Win+PrtScn
	write(filepath.Join(shots, "holiday.jpg"), 0)
	write(filepath.Join(shots, "Game", "Game 2026-11-27 10-06-00 Screenshot.part.png"), 0)
	got := ScanLibrary(clips, shots, 12)
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	want := "Game 2026-11-27 10-00-00 Screenshot.png|Desktop 2026-11-27 10-05-00 Screenshot (2).jpg|Game 2026-11-27 08-00-00 Replay.mp4|Game 2026-11-27 07-00-00 Screenshot.png"
	if strings.Join(names, "|") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(names, "|"), want)
	}
	// The same folder for both doesn't list anything twice.
	if n := len(ScanLibrary(clips, clips, 12)); n != 2 {
		t.Errorf("same folder twice: %d items", n)
	}
	if n := len(ScanLibrary(clips, shots, 2)); n != 2 {
		t.Errorf("limit: %d", n)
	}
}

func TestMediaServerScreenshots(t *testing.T) {
	root := t.TempDir()
	shot := filepath.Join(root, "Game 2026-11-27 10-00-00 Screenshot.png")
	if out, err := exec.Command(testFFmpeg(), "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720", "-frames:v", "1", shot).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	clipsRoot := t.TempDir() // screenshots live in their own folder, next to the clips folder
	m, err := NewMediaServer(testFFmpeg(), t.TempDir(), exec.Command, func() []string { return []string{clipsRoot, "", root} }, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	get := func(kind string) *http.Response {
		r, err := http.Get(m.Base() + "/" + kind + "?p=" + url.QueryEscape(shot))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	if r := get("file"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" {
		t.Errorf("file: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get("thumb"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("thumb: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get("preview"); r.StatusCode != 400 {
		t.Errorf("preview of a screenshot should be refused, got %d", r.StatusCode)
	}
}

func TestShareScreenshot(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "Game 2026-11-27 10-00-00 Screenshot.png")
	// Noise compresses badly, like a busy 4K game scene.
	if out, err := exec.Command(testFFmpeg(), "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "nullsrc=s=2560x1440,geq=random(1)*255:128:128",
		"-frames:v", "1", "-pix_fmt", "rgb24", shot).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	fi, _ := os.Stat(shot)
	limitMB := int(fi.Size()>>20) / 2
	if limitMB < 1 {
		t.Fatalf("test picture too small: %d bytes", fi.Size())
	}

	fd := newFakeDiscord(t)
	w, _, err := CheckWebhook(t.Context(), fd.hook("tok"))
	if err != nil {
		t.Fatal(err)
	}
	// Fits: posted as the original PNG.
	sh := testSharer(dir, 100)
	if p, _ := sh.Plan(shot, 0); p.Encode {
		t.Errorf("should post as is: %+v", p)
	}
	if _, err := sh.Share(ShareRequest{Path: shot, Webhook: w}, func(string, float64) {}); err != nil {
		t.Fatal(err)
	}
	if fd.gotType != "image/png" || fd.gotSize != fi.Size() || !strings.HasSuffix(fd.gotName, ".png") {
		t.Errorf("as is: %s %d %s", fd.gotType, fd.gotSize, fd.gotName)
	}
	// Too big: sent as a JPG under the limit.
	sh = testSharer(dir, limitMB)
	if p, _ := sh.Plan(shot, 0); !p.Encode {
		t.Errorf("should convert: %+v", p)
	}
	if _, err := sh.Share(ShareRequest{Path: shot, Webhook: w}, func(string, float64) {}); err != nil {
		t.Fatal(err)
	}
	if fd.gotType != "image/jpeg" || fd.gotSize > int64(limitMB)<<20 || !strings.HasSuffix(fd.gotName, ".jpg") {
		t.Errorf("shrunk: %s %d (limit %d MB) %s", fd.gotType, fd.gotSize, limitMB, fd.gotName)
	}
	if _, err := os.Stat(shot); err != nil {
		t.Errorf("original must be kept: %v", err)
	}
}

func TestSplitLibrary(t *testing.T) {
	var all []Clip
	for i := 0; i < 30; i++ {
		ext := ".mp4"
		if i%3 == 0 {
			ext = ".png"
		}
		all = append(all, Clip{Path: fmt.Sprintf("/c/%02d%s", i, ext)})
	}
	v, s := SplitLibrary(all, 12, 5)
	if len(v) != 12 || len(s) != 5 {
		t.Fatalf("got %d videos, %d shots", len(v), len(s))
	}
	if v[0].Path != "/c/01.mp4" || s[0].Path != "/c/00.png" || s[4].Path != "/c/12.png" {
		t.Errorf("order: %v %v", v[0], s)
	}
	for _, c := range v {
		if IsImage(c.Path) {
			t.Errorf("screenshot in the clips list: %s", c.Path)
		}
	}
	if v, s := SplitLibrary(nil, 12, 24); v != nil || s != nil {
		t.Errorf("empty library: %v %v", v, s)
	}
}
