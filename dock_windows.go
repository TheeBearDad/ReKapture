//go:build windows

package main

import (
	"fmt"
	"image"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The quick bar's window: one always-on-top, per-pixel-alpha popup that
// never takes focus from the game (WS_EX_NOACTIVATE) and is left out of
// every capture (WDA_EXCLUDEFROMCAPTURE). While the mouse is away it is
// click-through (WS_EX_TRANSPARENT), or hidden outright at 0% opacity, so
// it can never catch a click meant for the game. All window work happens
// on the UI thread, from the host window's 30 fps timer.

var (
	pSetCapture     = user32.NewProc("SetCapture")
	pReleaseCapture = user32.NewProc("ReleaseCapture")
	pGetCursorInfo  = user32.NewProc("GetCursorInfo")
	pLoadCursorW    = user32.NewProc("LoadCursorW")
	pSetCursor      = user32.NewProc("SetCursor")
)

const (
	dockTimerID     = 0xD0C
	dockTickMs      = 33
	wmSetCursor     = 0x0020
	wmMouseActivate = 0x0021
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmCaptureChange = 0x0215
)

type dock struct {
	mu        sync.Mutex
	opts      DockOptions
	replaySec int
	colors    [4]string // replay, shot, record, accent
	replayOn  bool
	recording bool
	bufferSec float64
	antiCheat string // name of a running anti-cheat, or ""
	peekUntil time.Time

	onAction    func(act string)     // "save" | "shot" | "record"
	onPin       func(pinned bool)    // right-click
	onMoved     func(fx, fy float64) // dropped after a drag
	onAntiCheat func(name string)    // anti-cheat started or stopped

	// UI thread only.
	hwnd        uintptr
	geom        DockGeom
	pos         image.Point
	hover       DockHover
	shown       float64 // current opacity, 0..100
	last        time.Time
	visible     bool
	interactive bool
	pressed     string
	dragging    bool
	dragMoved   bool
	dragOff     image.Point
	drawnKey    string
	topmostAt   time.Time
	dropMon     uintptr // monitor a custom bar was dropped on (this session)
	watching    bool
}

var dk = &dock{}

// Apply takes new settings. Safe from any goroutine.
func (d *dock) Apply(s Settings) {
	d.mu.Lock()
	d.opts = DockOptionsOf(s)
	if d.opts.Corner != "custom" {
		d.dropMon = 0 // back in a corner: follow the screen you're using again
	}
	d.replaySec = s.ReplaySeconds
	d.colors = [4]string{s.ReplayIconColor, s.ShotIconColor, s.RecDotColor, s.Accent}
	start := d.opts.Enabled && d.opts.HideAntiCheat && !d.watching
	if start {
		d.watching = true
	}
	d.mu.Unlock()
	if start {
		go d.watchAntiCheat()
	}
}

// SetStatus mirrors Instant Replay and recording state. Safe from any goroutine.
func (d *dock) SetStatus(st Status) {
	d.mu.Lock()
	d.replayOn, d.recording, d.bufferSec = st.ReplayOn, st.Recording, st.BufferSeconds
	d.mu.Unlock()
}

// Peek shows the bar for a few seconds, so you can see where it is.
func (d *dock) Peek(dur time.Duration) {
	d.mu.Lock()
	d.peekUntil = time.Now().Add(dur)
	d.mu.Unlock()
}

// AntiCheat is the anti-cheat currently hiding the bar, or "".
func (d *dock) AntiCheat() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.opts.Enabled || !d.opts.HideAntiCheat {
		return ""
	}
	return d.antiCheat
}

// watchAntiCheat looks for anti-cheat programs every 3 seconds while the
// bar is on and set to step aside for them.
func (d *dock) watchAntiCheat() {
	for {
		d.mu.Lock()
		on := d.opts.Enabled && d.opts.HideAntiCheat
		if !on {
			d.watching = false
			changed := d.antiCheat != ""
			d.antiCheat = ""
			cb := d.onAntiCheat
			d.mu.Unlock()
			if changed && cb != nil {
				cb("")
			}
			return
		}
		d.mu.Unlock()
		name := AntiCheatRunning(processNames())
		d.mu.Lock()
		changed := name != d.antiCheat
		d.antiCheat = name
		cb := d.onAntiCheat
		d.mu.Unlock()
		if changed {
			if name != "" {
				log.Printf("quick bar: hidden while %s is running", name)
			} else {
				log.Printf("quick bar: anti-cheat gone, showing again")
			}
			if cb != nil {
				cb(name)
			}
		}
		time.Sleep(3 * time.Second)
	}
}

func processNames() []string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var names []string
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		names = append(names, windows.UTF16ToString(e.ExeFile[:]))
	}
	return names
}

// start begins the 30 fps tick on the host window (UI thread).
func (d *dock) start(host uintptr) {
	registerClass("ReKaptureDock", syscall.NewCallback(d.proc), 0)
	pSetTimer.Call(host, dockTimerID, dockTickMs, 0)
}

func (d *dock) window() uintptr {
	if d.hwnd == 0 {
		d.hwnd = createWindow("ReKaptureDock", exLayer|exTop|exTool|exTransp|exNoAct, wsPopup)
		pSetWindowDisplayAffinity.Call(d.hwnd, 0x11 /*WDA_EXCLUDEFROMCAPTURE*/)
		d.interactive = false
	}
	return d.hwnd
}

func (d *dock) hide() {
	if d.visible && d.hwnd != 0 {
		pShowWindow.Call(d.hwnd, 0 /*SW_HIDE*/)
	}
	d.visible = false
	d.drawnKey = ""
	d.setInteractive(false)
}

func (d *dock) setInteractive(on bool) {
	if d.hwnd == 0 || on == d.interactive {
		return
	}
	ex, _, _ := pGetWindowLongPtrW.Call(d.hwnd, ^uintptr(19) /*GWL_EXSTYLE*/)
	if on {
		ex &^= exTransp
	} else {
		ex |= exTransp
	}
	pSetWindowLongPtrW.Call(d.hwnd, ^uintptr(19), ex)
	d.interactive = on
}

func cursorPos() (image.Point, bool) {
	var ci struct {
		Size, Flags uint32
		Cursor      uintptr
		Pt          point
	}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		var pt point
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		return image.Pt(int(pt.X), int(pt.Y)), true
	}
	return image.Pt(int(ci.Pt.X), int(ci.Pt.Y)), ci.Flags&1 != 0 // CURSOR_SHOWING
}

// monitorAt returns the work area (screen minus taskbar) and scale of the
// monitor holding a window or point.
func monitorWork(mon uintptr) (image.Rectangle, float64) {
	var mi [40]byte
	*(*uint32)(unsafe.Pointer(&mi[0])) = 40
	pGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi[0])))
	w := *(*rect)(unsafe.Pointer(&mi[20]))
	scale := 1.0
	var dx, dy uint32
	if r, _, _ := pGetDpiForMonitor.Call(mon, 0 /*MDT_EFFECTIVE_DPI*/, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); r == 0 && dx > 0 {
		scale = float64(dx) / 96
	}
	return image.Rect(int(w.Left), int(w.Top), int(w.Right), int(w.Bottom)), scale
}

func monitorOfPoint(p image.Point) uintptr {
	m, _, _ := pMonitorFromPoint.Call(uintptr(uint32(int32(p.X)))|uintptr(uint32(int32(p.Y)))<<32, 2 /*NEAREST*/)
	return m
}

// tick runs 30 times a second on the UI thread.
func (d *dock) tick() {
	d.mu.Lock()
	o, cols, rsec := d.opts, d.colors, d.replaySec
	view := DockView{ReplayOn: d.replayOn, Recording: d.recording, Pinned: o.Pinned,
		ReplayHex: cols[0], ShotHex: cols[1], RecHex: cols[2], AccentHex: cols[3]}
	if rsec > 0 {
		view.Fill = d.bufferSec / float64(rsec)
	}
	blocked := o.HideAntiCheat && d.antiCheat != ""
	peek := time.Now().Before(d.peekUntil)
	d.mu.Unlock()

	now := time.Now()
	dt := now.Sub(d.last)
	if dt <= 0 || dt > 200*time.Millisecond {
		dt = dockTickMs * time.Millisecond
	}
	d.last = now
	if !o.Enabled || blocked {
		d.hide()
		d.shown, d.dragging, d.pressed = 0, false, ""
		return
	}

	cur, cursorShown := cursorPos()
	var work image.Rectangle
	var scale float64
	if d.dragging {
		work, scale = monitorWork(monitorOfPoint(cur))
	} else {
		d.mu.Lock()
		mon := d.dropMon
		d.mu.Unlock()
		if mon == 0 || o.Corner != "custom" {
			fg := windows.GetForegroundWindow()
			mon, _, _ = pMonitorFromWindow.Call(uintptr(fg), 2)
		}
		work, scale = monitorWork(mon)
		if work.Empty() { // that monitor was unplugged
			d.mu.Lock()
			d.dropMon = 0
			d.mu.Unlock()
			fg := windows.GetForegroundWindow()
			mon, _, _ = pMonitorFromWindow.Call(uintptr(fg), 2)
			work, scale = monitorWork(mon)
		}
	}
	d.geom = LayoutDock(int(float64(DockButtonPx(o.Size)) * scale))
	view.Btn = d.geom.Btn
	size := image.Pt(d.geom.W, d.geom.H)
	if !d.dragging {
		d.pos = PlaceDock(work, size.X, size.Y, o.Corner, o.X, o.Y, int(16*scale))
	}

	bar := image.Rectangle{d.pos, d.pos.Add(size)}
	near := cursorShown && cur.In(bar.Inset(-int(14*scale)))
	hovered := d.hover.Step(now, near || d.dragging || d.pressed != "" || peek)
	d.shown = DockFade(d.shown, float64(DockTargetOpacity(o.Pinned, hovered, o.IdleOpacity, o.HoverOpacity)), dt)
	if d.shown < 0.5 {
		d.hide()
		return
	}
	if cursorShown && cur.In(bar) && !d.dragging {
		if hit := d.geom.HitTest(cur.X-d.pos.X, cur.Y-d.pos.Y); hit != "grip" && hit != "bar" {
			view.Hover = hit
		}
	}
	view.Pressed = d.pressed
	h := d.window()
	// Clickable only while it's actually showing under the mouse (or pinned).
	d.setInteractive((hovered || o.Pinned) && d.shown >= 15)

	alpha := byte(d.shown * 255 / 100)
	key := fmt.Sprintf("%+v|%v|%d", view, d.pos, alpha)
	if key != d.drawnKey || !d.visible {
		d.paint(h, view, alpha)
		d.drawnKey = key
	}
	if !d.visible || now.Sub(d.topmostAt) > time.Second {
		// Games that go fullscreen can end up above it; stay on top.
		pSetWindowPos.Call(h, ^uintptr(0) /*HWND_TOPMOST*/, 0, 0, 0, 0, 0x1|0x2|0x10|0x40 /*NOSIZE|NOMOVE|NOACTIVATE|SHOWWINDOW*/)
		d.topmostAt = now
		d.visible = true
	}
}

var dockBitmaps = map[string][]byte{}

func (d *dock) paint(h uintptr, v DockView, alpha byte) {
	key := fmt.Sprintf("%+v", v)
	bgr := dockBitmaps[key]
	if bgr == nil {
		img := RenderDock(v)
		bgr = make([]byte, len(img.Pix))
		for i := 0; i < len(img.Pix); i += 4 {
			bgr[i], bgr[i+1], bgr[i+2], bgr[i+3] = img.Pix[i+2], img.Pix[i+1], img.Pix[i], img.Pix[i+3]
		}
		if len(dockBitmaps) > 64 {
			dockBitmaps = map[string][]byte{}
		}
		dockBitmaps[key] = bgr
	}
	w, hh := d.geom.W, d.geom.H
	screen, _, _ := pGetDC.Call(0)
	mem, _, _ := pCreateCompatibleDC.Call(screen)
	bi := bitmapInfoHeader{Width: int32(w), Height: -int32(hh), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits uintptr
	dib, _, _ := pCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib != 0 && bits != 0 && len(bgr) == w*hh*4 {
		copy(unsafe.Slice((*byte)(unsafe.Pointer(bits)), len(bgr)), bgr)
		old, _, _ := pSelectObject.Call(mem, dib)
		pt := [2]int32{int32(d.pos.X), int32(d.pos.Y)}
		sz := [2]int32{int32(w), int32(hh)}
		src := [2]int32{0, 0}
		blend := blendFunction{Op: 0, Alpha: alpha, Format: 1 /*AC_SRC_ALPHA*/}
		pUpdateLayeredWindow.Call(h, screen, uintptr(unsafe.Pointer(&pt)), uintptr(unsafe.Pointer(&sz)), mem,
			uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), 2 /*ULW_ALPHA*/)
		pSelectObject.Call(mem, old)
	}
	if dib != 0 {
		pDeleteObject.Call(dib)
	}
	pDeleteDC.Call(mem)
	pReleaseDC.Call(0, screen)
}

func lpPoint(lp uintptr) image.Point {
	return image.Pt(int(int16(lp&0xFFFF)), int(int16((lp>>16)&0xFFFF)))
}

func (d *dock) proc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmMouseActivate:
		return 3 // MA_NOACTIVATE: clicking the bar leaves the game focused
	case wmSetCursor:
		cur, _ := cursorPos()
		id := uintptr(32512) // arrow
		switch d.geom.HitTest(cur.X-d.pos.X, cur.Y-d.pos.Y) {
		case "save", "shot", "record":
			id = 32649 // hand
		case "grip", "bar":
			id = 32646 // move
		}
		c, _, _ := pLoadCursorW.Call(0, id)
		pSetCursor.Call(c)
		return 1
	case wmLButtonDown:
		p := lpPoint(lp)
		switch hit := d.geom.HitTest(p.X, p.Y); hit {
		case "save", "shot", "record":
			d.pressed = hit
		case "grip", "bar":
			cur, _ := cursorPos()
			d.dragging, d.dragMoved, d.dragOff = true, false, cur.Sub(d.pos)
		}
		pSetCapture.Call(hwnd)
		return 0
	case wmMouseMove:
		if d.dragging {
			cur, _ := cursorPos()
			work, _ := monitorWork(monitorOfPoint(cur))
			np := cur.Sub(d.dragOff)
			// Keep the whole bar on the monitor the mouse is on.
			np.X = min(max(np.X, work.Min.X), work.Max.X-d.geom.W)
			np.Y = min(max(np.Y, work.Min.Y), work.Max.Y-d.geom.H)
			if np != d.pos {
				d.dragMoved = true
				d.pos = np
				d.tick()
			}
		}
		return 0
	case wmLButtonUp:
		// Read the state before releasing the capture: ReleaseCapture sends
		// WM_CAPTURECHANGED right away, which clears it.
		dragging, moved, act := d.dragging, d.dragMoved, d.pressed
		d.dragging, d.pressed = false, ""
		pReleaseCapture.Call()
		if dragging {
			if moved {
				cur, _ := cursorPos()
				work, _ := monitorWork(monitorOfPoint(cur))
				fx, fy := DockFractions(work, d.geom.W, d.geom.H, d.pos)
				d.mu.Lock()
				d.opts.Corner, d.opts.X, d.opts.Y = "custom", fx, fy
				d.dropMon = monitorOfPoint(cur)
				cb := d.onMoved
				d.mu.Unlock()
				if cb != nil {
					cb(fx, fy)
				}
			}
			return 0
		}
		p := lpPoint(lp)
		if act != "" && d.geom.HitTest(p.X, p.Y) == act && d.onAction != nil {
			d.onAction(act)
		}
		return 0
	case wmRButtonUp:
		d.mu.Lock()
		d.opts.Pinned = !d.opts.Pinned
		pinned, cb := d.opts.Pinned, d.onPin
		d.mu.Unlock()
		if cb != nil {
			cb(pinned)
		}
		return 0
	case wmCaptureChange:
		d.dragging, d.pressed = false, ""
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// dockActionName is how a quick bar click is written to the log.
func dockActionName(act string) string {
	return map[string]string{"save": "save replay", "shot": "screenshot", "record": "record"}[strings.ToLower(act)]
}
