package main

// The quick bar: a small on-screen bar with Save replay, Screenshot and
// Record buttons. It can stay invisible until the mouse comes near it,
// right-click pins it on screen, and it sits in a corner or wherever you
// drag it. This file holds the parts that don't need Windows: layout,
// hit-testing, the hover and fade logic, drawing, and anti-cheat detection.
// dock_windows.go puts it on screen.

import (
	"image"
	"math"
	"strings"
	"time"
)

// Dock buttons, left to right.
var DockButtons = []string{"save", "shot", "record"}

// DockGeom is the bar's layout in pixels, for drawing and hit-testing.
type DockGeom struct {
	W, H    int
	Pad     int
	Btn     int                        // button diameter
	Buttons map[string]image.Rectangle // by DockButtons name
	Grip    image.Rectangle            // drag handle
	Track   image.Rectangle            // Instant Replay buffer bar
}

// DockButtonPx is the button size for a size setting at 100% scaling.
func DockButtonPx(size string) int {
	switch size {
	case "small":
		return 28
	case "large":
		return 44
	}
	return 36
}

// LayoutDock lays the bar out for a button size (already DPI-scaled).
func LayoutDock(btn int) DockGeom {
	if btn < 16 {
		btn = 16
	}
	pad := btn * 2 / 9
	gap := btn / 4
	grip := btn * 3 / 8
	trackH := max(3, btn/12)
	g := DockGeom{Btn: btn, Pad: pad, Buttons: map[string]image.Rectangle{}}
	x := pad
	g.Grip = image.Rect(x, pad, x+grip, pad+btn)
	x += grip + gap/2
	for i, name := range DockButtons {
		if i > 0 {
			x += gap
		}
		g.Buttons[name] = image.Rect(x, pad, x+btn, pad+btn)
		x += btn
	}
	g.W = x + pad
	ty := pad + btn + pad*2/3
	g.Track = image.Rect(g.Grip.Max.X+gap/2, ty, g.W-pad, ty+trackH)
	g.H = ty + trackH + pad*2/3
	return g
}

// HitTest says what's under a point in bar coordinates: a button name,
// "grip", "bar" (the background) or "" (outside).
func (g DockGeom) HitTest(x, y int) string {
	p := image.Pt(x, y)
	for _, name := range DockButtons {
		// A slightly larger target than the drawn circle.
		if p.In(g.Buttons[name].Inset(-g.Pad / 2)) {
			return name
		}
	}
	if p.In(g.Grip.Inset(-g.Pad / 2)) {
		return "grip"
	}
	if p.In(image.Rect(0, 0, g.W, g.H)) {
		return "bar"
	}
	return ""
}

// PlaceDock gives the bar's top-left corner inside a monitor's work area
// (the screen minus the taskbar, so the bar never covers the tray).
// corner is tl|tr|bl|br|custom; fx, fy (0..1) place a custom bar.
func PlaceDock(work image.Rectangle, w, h int, corner string, fx, fy float64, margin int) image.Point {
	free := image.Pt(max(0, work.Dx()-w), max(0, work.Dy()-h))
	m := min(margin, free.X/2, free.Y/2)
	switch corner {
	case "tl":
		return image.Pt(work.Min.X+m, work.Min.Y+m)
	case "tr":
		return image.Pt(work.Max.X-w-m, work.Min.Y+m)
	case "bl":
		return image.Pt(work.Min.X+m, work.Max.Y-h-m)
	case "custom":
		fx, fy = clampF(fx, 0, 1), clampF(fy, 0, 1)
		return image.Pt(work.Min.X+int(math.Round(fx*float64(free.X))), work.Min.Y+int(math.Round(fy*float64(free.Y))))
	}
	return image.Pt(work.Max.X-w-m, work.Max.Y-h-m) // br
}

// DockFractions turns a dragged position back into the 0..1 fractions
// PlaceDock uses, so a custom spot survives resolution and monitor changes.
func DockFractions(work image.Rectangle, w, h int, at image.Point) (fx, fy float64) {
	free := image.Pt(work.Dx()-w, work.Dy()-h)
	if free.X > 0 {
		fx = clampF(float64(at.X-work.Min.X)/float64(free.X), 0, 1)
	}
	if free.Y > 0 {
		fy = clampF(float64(at.Y-work.Min.Y)/float64(free.Y), 0, 1)
	}
	return fx, fy
}

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// DockHover tracks whether the mouse is "at" the bar. The zone is the bar
// plus a margin, and leaving has a short grace period, so the bar doesn't
// flicker when the mouse brushes its edge.
type DockHover struct {
	inside   bool
	lastSeen time.Time
}

const dockLeaveGrace = 350 * time.Millisecond

// Step reports whether the bar counts as hovered at now.
func (d *DockHover) Step(now time.Time, cursorIn bool) bool {
	if cursorIn {
		d.inside, d.lastSeen = true, now
	} else if d.inside && now.Sub(d.lastSeen) > dockLeaveGrace {
		d.inside = false
	}
	return d.inside
}

// DockTargetOpacity is how visible the bar should be (0..100).
func DockTargetOpacity(pinned, hovered bool, idle, hover int) int {
	if pinned || hovered {
		return max(hover, idle)
	}
	return idle
}

// DockFade moves the shown opacity toward the target: quick to appear
// (about 0.12 s), a little slower to fade away (about 0.3 s).
func DockFade(cur, target float64, dt time.Duration) float64 {
	rate := 100 / 0.12
	if target < cur {
		rate = 100 / 0.3
	}
	step := rate * dt.Seconds()
	if math.Abs(target-cur) <= step {
		return target
	}
	if target > cur {
		return cur + step
	}
	return cur - step
}

// DockView is everything that changes how the bar is drawn.
type DockView struct {
	Btn       int
	Hover     string // button under the mouse
	Pressed   string // button held down
	ReplayOn  bool
	Recording bool
	Fill      float64 // Instant Replay buffer, 0..1
	Pinned    bool
	ReplayHex string
	ShotHex   string
	RecHex    string
	AccentHex string
}

// RenderDock draws the bar. The result is premultiplied RGBA, ready for a
// per-pixel-alpha window; overall opacity is applied when it's shown.
func RenderDock(v DockView) *image.RGBA {
	g := LayoutDock(v.Btn)
	img := image.NewRGBA(image.Rect(0, 0, g.W, g.H))
	W, H := float64(g.W), float64(g.H)
	r := float64(g.Btn)*0.34 + float64(g.Pad)

	rrect := func(x0, y0, x1, y1, rad float64) func(x, y float64) bool {
		return func(x, y float64) bool {
			dx := math.Max(math.Max(x0+rad-x, x-(x1-rad)), 0)
			dy := math.Max(math.Max(y0+rad-y, y-(y1-rad)), 0)
			return x >= x0 && x <= x1 && y >= y0 && y <= y1 && dx*dx+dy*dy <= rad*rad
		}
	}
	// fill paints a shape (4×4 supersampled) over what's there.
	fill := func(in func(x, y float64) bool, c [3]float64, a float64, area image.Rectangle) {
		area = area.Intersect(img.Rect)
		for y := area.Min.Y; y < area.Max.Y; y++ {
			for x := area.Min.X; x < area.Max.X; x++ {
				n := 0
				for sy := 0; sy < 4; sy++ {
					for sx := 0; sx < 4; sx++ {
						if in(float64(x)+(float64(sx)+0.5)/4, float64(y)+(float64(sy)+0.5)/4) {
							n++
						}
					}
				}
				if n == 0 {
					continue
				}
				blendOver(img, x, y, c, a*float64(n)/16)
			}
		}
	}
	all := img.Rect
	border := [3]float64{58, 63, 68}
	if v.Pinned {
		ar, ag, ab := AccentRGB(v.AccentHex)
		border = [3]float64{float64(ar), float64(ag), float64(ab)}
	}
	fill(rrect(0, 0, W, H, r), border, 0.95, all)
	fill(rrect(1.2, 1.2, W-1.2, H-1.2, r-1.2), [3]float64{18, 20, 22}, 0.97, all)

	// Grip: two columns of three dots.
	gr := g.Grip
	dot := math.Max(1.4, float64(g.Btn)*0.05)
	for i := 0; i < 3; i++ {
		for j := 0; j < 2; j++ {
			cx := float64(gr.Min.X) + float64(gr.Dx())*(0.32+0.36*float64(j))
			cy := float64(gr.Min.Y) + float64(gr.Dy())*(0.32+0.18*float64(i))
			fill(func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= dot*dot }, [3]float64{150, 156, 162}, 0.8, all)
		}
	}

	for _, name := range DockButtons {
		b := g.Buttons[name]
		cx, cy := float64(b.Min.X)+float64(b.Dx())/2, float64(b.Min.Y)+float64(b.Dy())/2
		if v.Hover == name || v.Pressed == name {
			hr := float64(g.Btn)/2 + float64(g.Pad)*0.55
			a := 0.16
			if v.Pressed == name {
				a = 0.28
			}
			fill(func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= hr*hr }, [3]float64{255, 255, 255}, a, b.Inset(-g.Pad))
		}
		icon, hex, op := IconReplay, v.ReplayHex, 100
		switch name {
		case "shot":
			icon, hex = IconShot, v.ShotHex
		case "record":
			icon, hex = IconRecStart, v.RecHex
			if v.Recording {
				icon = IconRecStop
			}
		case "save":
			if !v.ReplayOn {
				op = 35 // nothing to save while Instant Replay is off
			}
		}
		px := g.Btn
		if v.Pressed == name {
			px = px * 92 / 100
		}
		ic := RenderIndicator(icon, px, hex, op)
		off := image.Pt(b.Min.X+(g.Btn-px)/2, b.Min.Y+(g.Btn-px)/2)
		compositeOver(img, ic, off)
	}

	// Instant Replay buffer (red and full while recording).
	t := g.Track
	tr := float64(t.Dy()) / 2
	tx0, tx1, ty0, ty1 := float64(t.Min.X), float64(t.Max.X), float64(t.Min.Y), float64(t.Max.Y)
	fill(rrect(tx0, ty0, tx1, ty1, tr), [3]float64{48, 52, 56}, 1, all)
	frac, col := clampF(v.Fill, 0, 1), v.ReplayHex
	if v.Recording {
		frac, col = 1, v.RecHex
	} else if !v.ReplayOn {
		frac = 0
	}
	if frac > 0 {
		cr, cg, cb := AccentRGB(col)
		end := tx0 + math.Max(tr*2, (tx1-tx0)*frac)
		fill(rrect(tx0, ty0, end, ty1, tr), [3]float64{float64(cr), float64(cg), float64(cb)}, 1, all)
	}
	return img
}

// blendOver paints color c with alpha a over one premultiplied pixel.
func blendOver(img *image.RGBA, x, y int, c [3]float64, a float64) {
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	k := 1 - a
	p[0] = uint8(math.Round(c[0]*a + float64(p[0])*k))
	p[1] = uint8(math.Round(c[1]*a + float64(p[1])*k))
	p[2] = uint8(math.Round(c[2]*a + float64(p[2])*k))
	p[3] = uint8(math.Round(255*a + float64(p[3])*k))
}

// compositeOver draws premultiplied src over dst at off.
func compositeOver(dst, src *image.RGBA, off image.Point) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dx, dy := x+off.X, y+off.Y
			if !image.Pt(dx, dy).In(dst.Rect) {
				continue
			}
			s := src.Pix[src.PixOffset(x, y):][:4]
			if s[3] == 0 {
				continue
			}
			d := dst.Pix[dst.PixOffset(dx, dy):][:4]
			k := 255 - int(s[3])
			for c := 0; c < 4; c++ {
				d[c] = uint8(int(s[c]) + int(d[c])*k/255)
			}
		}
	}
}

// Anti-cheat programs whose presence hides the quick bar. Some anti-cheats
// look for see-through windows drawn over a game (a technique cheats use),
// so the bar steps aside entirely while any of these runs. Keys are
// lower-case process names.
var antiCheatProcs = map[string]string{
	"vgc.exe":                     "Riot Vanguard",
	"easyanticheat.exe":           "Easy Anti-Cheat",
	"easyanticheat_eos.exe":       "Easy Anti-Cheat",
	"beservice.exe":               "BattlEye",
	"beservice_x64.exe":           "BattlEye",
	"faceit.exe":                  "FACEIT",
	"faceitservice.exe":           "FACEIT",
	"eaanticheat.gameservice.exe": "EA Javelin",
	"gamemon.des":                 "nProtect GameGuard",
	"gamemon64.des":               "nProtect GameGuard",
	"pnkbstra.exe":                "PunkBuster",
	"pnkbstrb.exe":                "PunkBuster",
	"ace-tray.exe":                "Anti-Cheat Expert",
	"sguard64.exe":                "Anti-Cheat Expert",
}

// AntiCheatRunning returns the name of an anti-cheat in a list of running
// process names, or "".
func AntiCheatRunning(procs []string) string {
	for _, p := range procs {
		if name, ok := antiCheatProcs[strings.ToLower(p)]; ok {
			return name
		}
	}
	return ""
}

// DockOptions is what the quick bar needs from the settings.
type DockOptions struct {
	Enabled       bool
	Corner        string
	X, Y          float64
	IdleOpacity   int
	HoverOpacity  int
	Size          string
	HideAntiCheat bool
	Pinned        bool
}

func DockOptionsOf(s Settings) DockOptions {
	return DockOptions{Enabled: s.DockEnabled, Corner: s.DockCorner, X: s.DockX, Y: s.DockY,
		IdleOpacity: s.DockIdleOpacity, HoverOpacity: s.DockHoverOpacity, Size: s.DockSize,
		HideAntiCheat: s.DockHideAntiCheat, Pinned: s.DockPinned}
}
