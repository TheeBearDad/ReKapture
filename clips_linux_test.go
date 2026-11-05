//go:build linux

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"Cyberpunk 2077 (C) 2020 by CD Projekt RED": "Cyberpunk 2077",
		"Counter-Strike 2":                          "Counter-Strike 2",
		"Some Game ® 2024":                          "Some Game",
		"Game™":                                     "Game",
		"Minecraft 1.21.4":                          "Minecraft 1.21.4",
		"Indie Thing v1.2.3":                        "Indie Thing",
		"(C) Studio":                                "(C) Studio", // never return empty
		"Desktop":                                   "Desktop",
	}
	for in, want := range cases {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func makeClip(t *testing.T, path string, codec ...string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:d=4",
		"-f", "lavfi", "-i", "sine=f=440:d=4"}
	if len(codec) == 0 {
		codec = []string{"-c:v", "libopenh264"}
	}
	args = append(args, codec...)
	args = append(args, "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path)
	if out, err := exec.Command(testFFmpeg(), args...).CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, out)
	}
}

func TestScanClips(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Cyberpunk 2077"), 0o755)
	os.MkdirAll(filepath.Join(root, BufferDirName, "x"), 0o755)
	write := func(p string, age time.Duration) {
		os.WriteFile(p, []byte("data"), 0o644)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	write(filepath.Join(root, "Cyberpunk 2077", "Cyberpunk 2077 2026-11-04 06-18-24 Replay.mp4"), time.Minute)
	write(filepath.Join(root, "Desktop 2026-11-04 05-00-00 Recording.mkv"), time.Hour)
	write(filepath.Join(root, "notes.txt"), 0)
	write(filepath.Join(root, BufferDirName, "x", "seg_000001.mkv"), 0)
	write(filepath.Join(root, "Cyberpunk 2077", "old.mp4.part"), 0)
	got := ScanClips(root, 12)
	if len(got) != 2 {
		t.Fatalf("want 2 clips, got %+v", got)
	}
	if got[0].Kind != "Replay" || got[1].Kind != "Recording" {
		t.Errorf("order/kind wrong: %+v", got)
	}
	if ScanClips(filepath.Join(root, "missing"), 12) != nil {
		t.Errorf("missing folder should give no clips")
	}
}

func TestMediaServer(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	clip := filepath.Join(root, "Game", "Game 2026-11-04 06-18-24 Replay.mp4")
	os.MkdirAll(filepath.Dir(clip), 0o755)
	makeClip(t, clip)
	mkv := filepath.Join(root, "Game", "Game 2026-11-04 06-19-00 Recording.mkv")
	makeClip(t, mkv)
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	makeClip(t, outside)

	m, err := NewMediaServer(testFFmpeg(), cache, exec.Command, func() []string { return []string{root} }, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	get := func(kind, p string, hdr map[string]string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", m.Base()+"/"+kind+"?p="+url.QueryEscape(p), nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// the UI page is served at Base()/
	m.UI = []byte("<!DOCTYPE html><title>ui</title>")
	if resp, err := http.Get(m.Base() + "/"); err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("ui page not served: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
	if resp, err := http.Get(m.Base() + "/other"); err == nil {
		if resp.StatusCode != 404 {
			t.Errorf("unknown path should 404, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	// whole file + range request (seeking)
	r, b := get("file", clip, nil)
	fi, _ := os.Stat(clip)
	if r.StatusCode != 200 || int64(len(b)) != fi.Size() || r.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("file: %d len %d ctype %s", r.StatusCode, len(b), r.Header.Get("Content-Type"))
	}
	r, b = get("file", clip, map[string]string{"Range": "bytes=100-199"})
	if r.StatusCode != 206 || len(b) != 100 {
		t.Errorf("range: %d len %d", r.StatusCode, len(b))
	}

	// security: outside the clips folder, wrong type, bad token
	if r, _ := get("file", outside, nil); r.StatusCode != 403 {
		t.Errorf("outside root should be 403, got %d", r.StatusCode)
	}
	if r, _ := get("file", filepath.Join(root, "Game", "..", "..", filepath.Base(outside)), nil); r.StatusCode != 403 {
		t.Errorf("path traversal should be 403, got %d", r.StatusCode)
	}
	os.WriteFile(filepath.Join(root, "settings.json"), []byte("{}"), 0o644)
	if r, _ := get("file", filepath.Join(root, "settings.json"), nil); r.StatusCode != 403 {
		t.Errorf("non-video should be 403, got %d", r.StatusCode)
	}
	bad := strings.Replace(m.Base(), m.token, "nottoken", 1)
	if resp, err := http.Get(bad + "/file?p=" + url.QueryEscape(clip)); err == nil {
		if resp.StatusCode != 404 {
			t.Errorf("wrong token should be 404, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	// thumbnail: a JPEG, cached on second request
	r, b = get("thumb", clip, nil)
	if r.StatusCode != 200 || len(b) < 500 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatalf("thumb: %d len %d", r.StatusCode, len(b))
	}
	start := time.Now()
	get("thumb", clip, nil)
	if time.Since(start) > 300*time.Millisecond {
		t.Errorf("thumbnail not cached")
	}

	// preview copy of an MKV (falls back to libopenh264 without NVENC); concurrent requests share one job
	done := make(chan int, 3)
	for i := 0; i < 3; i++ {
		go func() { r, b := get("preview", mkv, nil); done <- r.StatusCode*0 + len(b) }()
	}
	sizes := []int{<-done, <-done, <-done}
	if sizes[0] < 1000 || sizes[0] != sizes[1] || sizes[1] != sizes[2] {
		t.Fatalf("preview sizes %v", sizes)
	}
	files, _ := filepath.Glob(filepath.Join(cache, "*.mp4"))
	if len(files) != 1 {
		t.Errorf("want exactly 1 cached preview, got %v", files)
	}
	pr := ffprobe(t, files[0])
	if len(pr.Streams) != 2 {
		t.Errorf("preview should have video+audio, got %d streams", len(pr.Streams))
	}
}
