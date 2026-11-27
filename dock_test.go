package main

import (
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDockLayoutAndHitTest(t *testing.T) {
	for _, size := range []string{"small", "medium", "large"} {
		g := LayoutDock(DockButtonPx(size))
		if g.W <= 0 || g.H <= 0 {
			t.Fatalf("%s: empty bar %dx%d", size, g.W, g.H)
		}
		// Buttons don't overlap, sit left to right, and stay inside the bar.
		prev := g.Grip
		for _, name := range DockButtons {
			b := g.Buttons[name]
			if b.Min.X < prev.Max.X || !b.In(image.Rect(0, 0, g.W, g.H)) {
				t.Errorf("%s: %s at %v overlaps %v or leaves the bar", size, name, b, prev)
			}
			c := b.Min.Add(b.Size().Div(2))
			if got := g.HitTest(c.X, c.Y); got != name {
				t.Errorf("%s: centre of %s hit %q", size, name, got)
			}
			prev = b
		}
		gc := g.Grip.Min.Add(g.Grip.Size().Div(2))
		if got := g.HitTest(gc.X, gc.Y); got != "grip" {
			t.Errorf("%s: grip hit %q", size, got)
		}
		if got := g.HitTest(g.W/2, g.H-1); got != "bar" {
			t.Errorf("%s: bottom edge hit %q", size, got)
		}
		if got := g.HitTest(g.W+5, 2); got != "" {
			t.Errorf("%s: outside hit %q", size, got)
		}
	}
	if a, b := LayoutDock(DockButtonPx("small")), LayoutDock(DockButtonPx("large")); a.W >= b.W {
		t.Errorf("small %d not smaller than large %d", a.W, b.W)
	}
}

func TestPlaceDock(t *testing.T) {
	work := image.Rect(0, 0, 3840, 2112) // a 4K screen above a 48 px taskbar
	w, h := 180, 60
	cases := map[string]image.Point{
		"tl": {20, 20}, "tr": {3840 - 180 - 20, 20}, "bl": {20, 2112 - 60 - 20}, "br": {3840 - 180 - 20, 2112 - 60 - 20},
	}
	for corner, want := range cases {
		if got := PlaceDock(work, w, h, corner, 0, 0, 20); got != want {
			t.Errorf("%s: %v want %v", corner, got, want)
		}
	}
	if got := PlaceDock(work, w, h, "nonsense", 0, 0, 20); got != cases["br"] {
		t.Errorf("unknown corner should be bottom-right, got %v", got)
	}
	// Custom spot round-trips through fractions, including on a second
	// monitor with a negative origin and a different size.
	at := image.Pt(1200, 700)
	fx, fy := DockFractions(work, w, h, at)
	if got := PlaceDock(work, w, h, "custom", fx, fy, 20); got != at {
		t.Errorf("custom round trip %v want %v", got, at)
	}
	left := image.Rect(-1920, 0, 0, 1040)
	p := PlaceDock(left, w, h, "custom", fx, fy, 20)
	if !image.Rect(p.X, p.Y, p.X+w, p.Y+h).In(left) {
		t.Errorf("custom bar %v off the %v monitor", p, left)
	}
	// Dragged off screen: kept fully on it.
	fx, fy = DockFractions(work, w, h, image.Pt(5000, -300))
	if fx != 1 || fy != 0 {
		t.Errorf("off-screen drag fractions %v %v", fx, fy)
	}
	// A screen smaller than the bar doesn't produce nonsense.
	tiny := image.Rect(0, 0, 100, 40)
	if p := PlaceDock(tiny, w, h, "br", 0, 0, 20); p.X < -w || p.Y < -h {
		t.Errorf("tiny screen %v", p)
	}
}

func TestDockHoverAndFade(t *testing.T) {
	var d DockHover
	t0 := time.Now()
	if d.Step(t0, false) {
		t.Error("hovered without the mouse")
	}
	if !d.Step(t0, true) {
		t.Error("not hovered with the mouse on it")
	}
	if !d.Step(t0.Add(200*time.Millisecond), false) {
		t.Error("hover should survive a brief exit")
	}
	if d.Step(t0.Add(500*time.Millisecond), false) {
		t.Error("hover should end after the grace period")
	}

	if got := DockTargetOpacity(false, false, 0, 95); got != 0 {
		t.Errorf("idle %d", got)
	}
	if got := DockTargetOpacity(false, true, 0, 95); got != 95 {
		t.Errorf("hover %d", got)
	}
	if got := DockTargetOpacity(true, false, 0, 95); got != 95 {
		t.Errorf("pinned %d", got)
	}
	if got := DockTargetOpacity(true, false, 60, 40); got != 60 {
		t.Errorf("hover below idle should not dim it: %d", got)
	}

	// Fade in takes ~0.12 s, fade out ~0.3 s, at 30 frames a second.
	frames := func(from, to float64) int {
		n := 0
		for v := from; v != to && n < 100; n++ {
			v = DockFade(v, to, 33*time.Millisecond)
		}
		return n
	}
	if in, out := frames(0, 95), frames(95, 0); in < 3 || in > 5 || out < 8 || out > 11 {
		t.Errorf("fade in %d frames, out %d", in, out)
	}
	if DockFade(50, 50, time.Second) != 50 {
		t.Error("steady opacity moved")
	}
}

func TestAntiCheatRunning(t *testing.T) {
	if got := AntiCheatRunning([]string{"explorer.exe", "Discord.exe", "GTA5.exe"}); got != "" {
		t.Errorf("false alarm: %s", got)
	}
	for proc, want := range map[string]string{
		"EasyAntiCheat_EOS.exe": "Easy Anti-Cheat", "BEService.exe": "BattlEye", "vgc.exe": "Riot Vanguard",
		"FACEIT.exe": "FACEIT", "EAAntiCheat.GameService.exe": "EA Javelin",
	} {
		if got := AntiCheatRunning([]string{"svchost.exe", proc}); got != want {
			t.Errorf("%s: got %q want %q", proc, got, want)
		}
	}
}

func TestDockSettings(t *testing.T) {
	d := DefaultSettings("/v")
	o := DockOptionsOf(d)
	if o.Enabled || o.Corner != "br" || o.IdleOpacity != 0 || o.HoverOpacity != 95 || !o.HideAntiCheat || o.Size != "medium" {
		t.Errorf("defaults: %+v", o)
	}
	// An older settings file (no quick bar keys) gets the defaults,
	// including the anti-cheat safety switch.
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"fps":60}`), 0o644)
	s := LoadSettings(p, DefaultSettings("/v"))
	if !s.DockHideAntiCheat || s.DockCorner != "br" || s.DockHoverOpacity != 95 {
		t.Errorf("upgrade: %+v", DockOptionsOf(s))
	}
	// Bad values are cleaned up.
	b, _ := json.Marshal(map[string]any{"dockCorner": "middle", "dockX": 7, "dockY": -2, "dockIdleOpacity": 300, "dockHoverOpacity": 5, "dockSize": "huge"})
	os.WriteFile(p, b, 0o644)
	s = LoadSettings(p, DefaultSettings("/v"))
	if s.DockCorner != "br" || s.DockX != 1 || s.DockY != 0 || s.DockIdleOpacity != 100 || s.DockHoverOpacity != 20 || s.DockSize != "medium" {
		t.Errorf("cleanup: %+v", DockOptionsOf(s))
	}
}

func TestRenderDock(t *testing.T) {
	out := os.Getenv("GL_ICON_OUT")
	views := map[string]DockView{
		"idle":      {Btn: 36, ReplayOn: true, Fill: 0.45},
		"hover":     {Btn: 36, ReplayOn: true, Fill: 1, Hover: "shot", Pinned: true},
		"recording": {Btn: 36, ReplayOn: true, Recording: true, Pressed: "record"},
		"replayoff": {Btn: 28, ReplayOn: false},
		"large":     {Btn: 44 * 3 / 2, ReplayOn: true, Fill: 0.7, Hover: "save"},
	}
	for name, v := range views {
		v.ReplayHex, v.ShotHex, v.RecHex, v.AccentHex = "#76B900", "#0A72F0", "#E5484D", "#76B900"
		img := RenderDock(v)
		g := LayoutDock(v.Btn)
		if img.Rect.Dx() != g.W || img.Rect.Dy() != g.H {
			t.Errorf("%s: size %v want %dx%d", name, img.Rect, g.W, g.H)
		}
		// Premultiplied: no channel above alpha; rounded corners transparent; middle opaque.
		for i := 0; i < len(img.Pix); i += 4 {
			if img.Pix[i] > img.Pix[i+3] || img.Pix[i+1] > img.Pix[i+3] || img.Pix[i+2] > img.Pix[i+3] {
				t.Fatalf("%s: not premultiplied at %d", name, i/4)
			}
		}
		if a := img.RGBAAt(0, 0).A; a != 0 {
			t.Errorf("%s: corner alpha %d", name, a)
		}
		if a := img.RGBAAt(g.W/2, g.H/2).A; a < 240 {
			t.Errorf("%s: middle alpha %d", name, a)
		}
		if out != "" {
			f, _ := os.Create(filepath.Join(out, "dock-"+name+".png"))
			png.Encode(f, img)
			f.Close()
		}
	}
}
