package main

// Screenshots: one full-resolution frame of the monitor you're looking at,
// exactly as Windows shows it. Monitor capture (not window capture) is used
// on purpose, so anything drawn on top of the game is in the picture too:
// in-game upscaling and frame generation (DLSS), overlays from other apps
// (Steam, Discord, NVIDIA, filters), and the cursor-free desktop around it.
// ReKapture's own indicators hide themselves from capture, so they never
// end up in a screenshot.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// screenshotTimeout bounds one capture. Starting a capture session and
// encoding a 4K PNG takes well under a second; a stuck capture (monitor
// asleep, protected content) must not leave the hotkey busy forever.
const screenshotTimeout = 8 * time.Second

// ScreenshotSource is the capture for a screenshot: the whole monitor at its
// native size, no scaling or cropping, without the mouse pointer. Repeated
// frames stay on so a still screen still delivers a frame right away.
func ScreenshotSource(src CaptureSource) string {
	return fmt.Sprintf("gfxcapture=hmonitor=%d:max_framerate=60:capture_cursor=0,hwdownload,format=bgra", src.Handle)
}

// ScreenshotArgs encodes the first frame of src0 to out. PNG is lossless and
// stored as plain RGB (the capture's alpha channel is meaningless for a
// monitor and would make parts of the PNG transparent). JPG uses the
// highest practical quality with full color resolution (no 4:2:0 smearing
// of red text and HUD edges).
func ScreenshotArgs(src0, out, format string) []string {
	chain, codec := src0+",format=rgb24[v]", []string{"-c:v", "png"}
	if format == "jpg" {
		// RGB first (drops alpha), then full-range 4:4:4 for mjpeg.
		chain = src0 + ",format=rgb24,format=yuvj444p[v]"
		codec = []string{"-c:v", "mjpeg", "-q:v", "1", "-qmin", "1"}
	}
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-filter_complex", chain, "-map", "[v]", "-frames:v", "1", "-update", "1", "-f", "image2"}
	args = append(args, codec...)
	return append(args, out)
}

// screenshotMonitor picks the monitor to capture: the one the followed game
// is on, else the one with the window in front, else the chosen monitor.
func (e *Engine) screenshotMonitor(s Settings) string {
	if g := e.CurrentGame(); g != nil && g.MonitorID != "" {
		return g.MonitorID
	}
	if fg := e.plat.ForegroundApp(); fg.MonitorID != "" {
		return fg.MonitorID
	}
	return s.MonitorID
}

// Screenshot saves a full-resolution picture of what's on screen now. It
// runs on its own (Instant Replay and recording don't need to be on) and
// takes one screenshot at a time; a press while one is saving is ignored.
func (e *Engine) Screenshot() {
	e.mu.Lock()
	if e.shooting {
		e.mu.Unlock()
		return
	}
	e.shooting = true
	s := e.settings
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.shooting = false
		e.mu.Unlock()
	}()

	name := e.clipName()
	path, err := e.takeScreenshot(s, name)
	if err != nil {
		log.Printf("screenshot: %v", err)
		e.plat.Notify("warn", "Screenshot failed", err.Error())
		return
	}
	log.Printf("screenshot saved: %s", path)
	e.RefreshClips()
	e.plat.Notify("shot", "Screenshot saved", name)
}

func (e *Engine) takeScreenshot(s Settings, name string) (string, error) {
	t := time.Now()
	c := s
	c.CaptureMode = "monitor"
	c.MonitorID = e.screenshotMonitor(s)
	c.Resolution = "native"
	src, err := e.plat.ResolveCapture(c)
	if err != nil {
		return "", err
	}
	c.Container = s.ScreenshotFormat
	c.OutputDir = s.ScreenshotDir
	out, err := e.clipPath(c, name, "Screenshot", t)
	if err != nil {
		return "", err
	}
	src0 := e.videoTest
	if src0 == "" {
		src0 = ScreenshotSource(src)
	}
	// Written under a temporary name and renamed when complete, so the
	// library never lists a half-written picture.
	tmp := strings.TrimSuffix(out, "."+c.Container) + ".part." + c.Container
	ctx, cancel := context.WithTimeout(context.Background(), screenshotTimeout)
	defer cancel()
	cmd := e.plat.Command(e.ffmpeg, ScreenshotArgs(src0, tmp, c.Container)...)
	outb, err := runWithContext(ctx, cmd)
	if err != nil {
		os.Remove(tmp)
		os.Remove(filepath.Dir(out)) // the game folder, if this made it and it's empty
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("Windows didn't deliver a picture of the screen. Is the monitor asleep?")
		}
		msg := lastLine(strings.TrimSpace(string(outb)))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		os.Remove(tmp)
		return "", errors.New("the screen capture came back empty")
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, nil
}

// runWithContext runs cmd (already built by the platform, so it keeps its
// hidden-window settings) and kills it when ctx ends.
func runWithContext(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return []byte(buf.String()), err
	case <-ctx.Done():
		cmd.Process.Kill()
		<-done
		return []byte(buf.String()), ctx.Err()
	}
}
