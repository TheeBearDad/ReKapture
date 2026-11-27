package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestIndicatorFor(t *testing.T) {
	cases := []struct {
		kind, title, icon string
		skip              bool
	}{
		{"busy", "Saving replay…", IconReplay, true},
		{"rec", "Recording started", IconRecStart, true},
		{"busy", "Saving recording…", IconRecStop, true},
		{"ok", "Replay saved", "", true},
		{"ok", "Recording saved", "", true},
		{"shot", "Screenshot saved", IconShot, true},
		{"warn", "Screenshot failed", "", false},
		{"warn", "Replay failed", "", false}, // problems keep their words
		{"ok", "Clip copied", "", false},     // unrelated notices too
		{"info", "Instant Replay on", "", false},
		{"busy", "Finishing your recording…", "", false},
	}
	for _, c := range cases {
		icon, skip := IndicatorFor(c.kind, c.title)
		if icon != c.icon || skip != c.skip {
			t.Errorf("%s %q: got (%q,%v) want (%q,%v)", c.kind, c.title, icon, skip, c.icon, c.skip)
		}
	}
}

func TestIndicatorSettings(t *testing.T) {
	s := DefaultSettings("/v")
	if s.OverlayStyle != "icons" || s.OverlaySeconds != 3 {
		t.Errorf("defaults: %s %v", s.OverlayStyle, s.OverlaySeconds)
	}
	if IndicatorMillis(s, "ok") != 3000 || IndicatorMillis(s, "warn") != 4500 {
		t.Error("durations")
	}
	s.OverlaySeconds, s.ReplayIconColor, s.RecDotOpacity, s.OverlayStyle = 99, "red", 400, "x"
	s.Normalize(DefaultSettings("/v"))
	if s.OverlaySeconds != 30 || s.ReplayIconColor != "#76B900" || s.RecDotOpacity != 100 || s.OverlayStyle != "icons" {
		t.Errorf("normalize: %v %s %d %s", s.OverlaySeconds, s.ReplayIconColor, s.RecDotOpacity, s.OverlayStyle)
	}
	s.OverlaySeconds = 0.2
	s.Normalize(DefaultSettings("/v"))
	if s.OverlaySeconds != 0.5 {
		t.Errorf("minimum time %v", s.OverlaySeconds)
	}
}

func TestRenderIndicator(t *testing.T) {
	for _, c := range []struct {
		icon, hex string
		op        int
	}{{IconReplay, "#0A72F0", 100}, {IconReplay, "#76B900", 60}, {IconRecStart, "#E5484D", 100}, {IconRecStop, "#E5484D", 100}, {IconShot, "#0A72F0", 100}, {IconShot, "#F2F4F5", 100}, {IconShot, "#E5484D", 60}} {
		img := RenderIndicator(c.icon, 80, c.hex, c.op)
		if a := img.RGBAAt(0, 0).A; a != 0 {
			t.Errorf("%s: corner should be transparent, alpha %d", c.icon, a)
		}
		mid := img.RGBAAt(40, 20) // upper part of the disc
		if c.op == 100 && mid.A < 250 {
			t.Errorf("%s: disc should be solid, alpha %d", c.icon, mid.A)
		}
		if c.op == 60 && (mid.A < 140 || mid.A > 165) {
			t.Errorf("%s: 60%% opacity gave alpha %d", c.icon, mid.A)
		}
		for y := 0; y < 80; y++ { // premultiplied: no channel above alpha
			for x := 0; x < 80; x++ {
				p := img.RGBAAt(x, y)
				if p.R > p.A || p.G > p.A || p.B > p.A {
					t.Fatalf("%s: not premultiplied at %d,%d: %v", c.icon, x, y, p)
				}
			}
		}
		if dir := os.Getenv("GL_ICON_OUT"); dir != "" {
			f, _ := os.Create(filepath.Join(dir, c.icon+"-"+c.hex[1:]+".png"))
			png.Encode(f, img)
			f.Close()
		}
	}
}
