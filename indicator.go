package main

// In-game indicators: small icons shown in a screen corner instead of text
// cards. A rewind icon when a replay is saved, a red dot when recording
// starts, a red stop icon when it ends, and a camera for screenshots. Drawn here (not from image
// files) so they're sharp at any display scaling and take any color.

import (
	"image"
	"image/color"
	"math"
	"strings"
)

const (
	IconReplay   = "replay"
	IconRecStart = "rec-start"
	IconRecStop  = "rec-stop"
	IconShot     = "shot"
)

// IndicatorFor decides how a notice is shown when "small icons" is on: which
// icon (if any) replaces it, and whether the text card is skipped. Problems
// and everything else keep their text card, because they need words.
func IndicatorFor(kind, title string) (icon string, skipCard bool) {
	switch {
	case kind == "busy" && strings.HasPrefix(title, "Saving replay"):
		return IconReplay, true // shown the moment the hotkey is pressed
	case kind == "rec":
		return IconRecStart, true
	case kind == "busy" && strings.HasPrefix(title, "Saving recording"):
		return IconRecStop, true
	case kind == "shot":
		return IconShot, true // shown once the picture is saved
	case kind == "ok" && (title == "Replay saved" || title == "Recording saved"):
		return "", true // the icon already confirmed it
	}
	return "", false
}

// IndicatorMillis is how long an indicator or notice stays up.
func IndicatorMillis(s Settings, kind string) int {
	ms := int(s.OverlaySeconds * 1000)
	if kind == "warn" && ms < 4500 {
		ms = 4500 // problems stay long enough to read
	}
	return ms
}

// IndicatorBase is the icon size in pixels at 100% display scaling.
func IndicatorBase(icon, size string) int {
	base := map[string]int{"small": 30, "medium": 40, "large": 54}[size]
	if base == 0 {
		base = 40
	}
	if icon == IconRecStart || icon == IconRecStop {
		return base * 7 / 10 // the record dot is meant to be small
	}
	return base
}

// RenderIndicator draws an icon px×px with a transparent background.
// The result is premultiplied RGBA, ready for a per-pixel-alpha window.
func RenderIndicator(icon string, px int, hex string, opacity int) *image.RGBA {
	if px < 8 {
		px = 8
	}
	r, g, b := AccentRGB(hex)
	col := [3]float64{float64(r), float64(g), float64(b)}
	op := math.Max(0.1, math.Min(1, float64(opacity)/100))
	img := image.NewRGBA(image.Rect(0, 0, px, px))
	n := float64(px)
	c := n / 2
	shadow := n * 0.045 // drop shadow offset

	// Each layer: a coverage function (supersampled) and a color.
	type layer struct {
		in  func(x, y float64) bool
		rgb [3]float64
		a   float64
	}
	circle := func(cx, cy, rad float64) func(x, y float64) bool {
		return func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= rad*rad }
	}
	tri := func(ax, ay, bx, by, cx2, cy2 float64) func(x, y float64) bool {
		return func(x, y float64) bool {
			d1 := (x-bx)*(ay-by) - (ax-bx)*(y-by)
			d2 := (x-cx2)*(by-cy2) - (bx-cx2)*(y-cy2)
			d3 := (x-ax)*(cy2-ay) - (cx2-ax)*(y-ay)
			neg := d1 < 0 || d2 < 0 || d3 < 0
			pos := d1 > 0 || d2 > 0 || d3 > 0
			return !(neg && pos)
		}
	}
	mix := func(a [3]float64, t float64, to float64) [3]float64 {
		return [3]float64{a[0] + (to-a[0])*t, a[1] + (to-a[1])*t, a[2] + (to-a[2])*t}
	}
	rad := n/2 - shadow - 1
	var layers []layer
	// Soft shadow under the disc, so it reads on bright and dark scenes.
	layers = append(layers, layer{circle(c, c+shadow, rad), [3]float64{0, 0, 0}, 0.35})
	switch icon {
	case IconReplay:
		layers = append(layers, layer{circle(c, c, rad), col, 1})
		light := mix(col, 0.68, 255) // pale tint of the chosen color
		dark := mix(col, 0.35, 0)
		// Two left-pointing triangles (rewind), with a darker offset under each.
		h := rad * 0.52
		w := rad * 0.6
		x0 := c - rad*0.68
		mk := func(ox, oy float64) []func(x, y float64) bool {
			return []func(x, y float64) bool{
				tri(x0+ox, c+oy, x0+w+ox, c-h+oy, x0+w+ox, c+h+oy),
				tri(x0+w+ox, c+oy, x0+2*w+ox, c-h+oy, x0+2*w+ox, c+h+oy),
			}
		}
		for _, f := range mk(rad*0.05, rad*0.09) {
			layers = append(layers, layer{f, dark, 1})
		}
		for _, f := range mk(0, 0) {
			layers = append(layers, layer{f, light, 1})
		}
	case IconShot:
		layers = append(layers, layer{circle(c, c, rad), col, 1})
		// Camera in a pale tint of the color, details in a dark shade. On a
		// light disc (white, yellow) that flips so the camera stays visible.
		body, detail := mix(col, 0.78, 255), mix(col, 0.45, 0)
		if 0.299*col[0]+0.587*col[1]+0.114*col[2] > 170 {
			body, detail = mix(col, 0.72, 20), mix(col, 0.25, 255)
		}
		shadowC := mix(col, 0.4, 0)
		rrect := func(x0, y0, x1, y1, r float64) func(x, y float64) bool {
			return func(x, y float64) bool { // rounded rectangle
				dx := math.Max(math.Max(x0+r-x, x-(x1-r)), 0)
				dy := math.Max(math.Max(y0+r-y, y-(y1-r)), 0)
				return x >= x0 && x <= x1 && y >= y0 && y <= y1 && dx*dx+dy*dy <= r*r
			}
		}
		// Body, with a viewfinder hump on top (a trapezoid) and a shutter button.
		bw, bh := rad*1.34, rad*0.86
		bx0, by0 := c-bw/2, c-bh/2+rad*0.13
		bodyF := rrect(bx0, by0, bx0+bw, by0+bh, rad*0.16)
		hy0, hy1 := by0-rad*0.22, by0+rad*0.04
		humpF := func(x, y float64) bool {
			if y < hy0 || y > hy1 {
				return false
			}
			t := (y - hy0) / (hy1 - hy0) // wider at the bottom
			half := rad * (0.22 + 0.12*t)
			cx := c - rad*0.06
			return x >= cx-half && x <= cx+half
		}
		shutter := rrect(bx0+bw*0.70, by0-rad*0.12, bx0+bw*0.88, by0+rad*0.04, rad*0.04)
		cam := func(dx, dy float64) func(x, y float64) bool {
			return func(x, y float64) bool { x, y = x-dx, y-dy; return bodyF(x, y) || humpF(x, y) || shutter(x, y) }
		}
		lx, ly := c+rad*0.02, by0+bh*0.53
		layers = append(layers,
			layer{cam(0, rad*0.08), shadowC, 1}, // drop shadow under the camera
			layer{cam(0, 0), body, 1},
			layer{circle(lx, ly, bh*0.40), detail, 1}, // lens ring
			layer{circle(lx, ly, bh*0.29), body, 1},
			layer{circle(lx, ly, bh*0.19), detail, 1},                                             // glass
			layer{circle(lx-bh*0.07, ly-bh*0.07, bh*0.055), body, 1},                              // glint
			layer{rrect(bx0+bw*0.09, by0+bh*0.16, bx0+bw*0.25, by0+bh*0.30, rad*0.03), detail, 1}, // flash
		)
	case IconRecStart:
		layers = append(layers, layer{circle(c, c, rad), [3]float64{255, 255, 255}, 0.92}) // thin white rim
		layers = append(layers, layer{circle(c, c, rad*0.84), col, 1})
	case IconRecStop:
		layers = append(layers, layer{circle(c, c, rad), [3]float64{255, 255, 255}, 0.92})
		layers = append(layers, layer{circle(c, c, rad*0.84), col, 1})
		s := rad * 0.36
		layers = append(layers, layer{func(x, y float64) bool {
			return x >= c-s && x <= c+s && y >= c-s && y <= c+s
		}, [3]float64{255, 255, 255}, 1})
	}

	const ss = 4 // 4×4 supersampling for smooth edges
	for py := 0; py < px; py++ {
		for pxx := 0; pxx < px; pxx++ {
			// Straight (non-premultiplied) color, composited layer by layer.
			var R, G, B, A float64
			for _, l := range layers {
				hits := 0
				for sy := 0; sy < ss; sy++ {
					for sx := 0; sx < ss; sx++ {
						if l.in(float64(pxx)+(float64(sx)+0.5)/ss, float64(py)+(float64(sy)+0.5)/ss) {
							hits++
						}
					}
				}
				if hits == 0 {
					continue
				}
				a := l.a * float64(hits) / (ss * ss)
				outA := a + A*(1-a)
				if outA > 0 {
					R = (l.rgb[0]*a + R*A*(1-a)) / outA
					G = (l.rgb[1]*a + G*A*(1-a)) / outA
					B = (l.rgb[2]*a + B*A*(1-a)) / outA
				}
				A = outA
			}
			A *= op
			img.SetRGBA(pxx, py, color.RGBA{
				R: uint8(R*A + 0.5), G: uint8(G*A + 0.5), B: uint8(B*A + 0.5), A: uint8(A*255 + 0.5),
			})
		}
	}
	return img
}
