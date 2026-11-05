//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestUIHarness serves the real UI and real clips for a browser check.
// Run with GL_UI_HARNESS=/path/to/write/base.txt.
func TestUIHarness(t *testing.T) {
	out := os.Getenv("GL_UI_HARNESS")
	if out == "" {
		t.Skip("set GL_UI_HARNESS to run")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Game"), 0o755)
	mkv := filepath.Join(root, "Game", "Game 2026-11-04 06-18-24 Replay.mkv")
	if os.Getenv("GL_UI_H264") != "" { // a codec this test browser can't decode
		mkv = filepath.Join(root, "Game", "Game 2026-11-04 06-18-24 Replay.mp4")
		makeClip(t, mkv)
	} else {
		makeClip(t, mkv, "-c:v", "libvpx-vp9", "-b:v", "1M")
	}
	m, err := NewMediaServer(testFFmpeg(), t.TempDir(), exec.Command, func() []string { return []string{root} }, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	ui, _ := os.ReadFile("ui/index.html")
	m.UI = ui
	os.WriteFile(out, []byte(m.Base()+"\n"+mkv), 0o644)
	time.Sleep(45 * time.Second)
}
