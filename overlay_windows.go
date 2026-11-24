//go:build windows

package main

import (
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native, click-through, always-on-top overlay windows: a toast for
// "Replay saved" style notices and a small REC timer. Both are excluded from
// screen capture, so they never show up in your clips.

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

const (
	wmPaint  = 0x000F
	wmTimer  = 0x0113
	wsPopup  = 0x80000000
	exLayer  = 0x00080000
	exTop    = 0x00000008
	exTool   = 0x00000080
	exTransp = 0x00000020
	exNoAct  = 0x08000000
)

func rgb(r, g, b uint8) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

type overlay struct {
	mu      sync.Mutex
	toast   uintptr
	badge   uintptr
	icon    uintptr // per-pixel-alpha icon indicator
	kind    string
	title   string
	msg     string
	badgeTx string
	corner  string
	badgeOn bool
	accent  uintptr            // COLORREF for "saved" notices
	fonts   map[int][3]uintptr // dpi -> title, body, badge
}

func (o *overlay) setAccent(hexStr string) {
	r, g, b := AccentRGB(hexStr)
	o.accent = rgb(r, g, b)
}

var ov = &overlay{fonts: map[int][3]uintptr{}, corner: "tr", accent: rgb(0x76, 0xB9, 0x00)}

func registerClass(name string, proc uintptr, icon uintptr) {
	var hinst windows.Handle
	windows.GetModuleHandleEx(0, nil, &hinst)
	wc := wndClassEx{LpfnWndProc: proc, HInstance: hinst, LpszClassName: utf16(name), HIcon: icon, HIconSm: icon}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
}

func createWindow(class string, ex, style uintptr) uintptr {
	var hinst windows.Handle
	windows.GetModuleHandleEx(0, nil, &hinst)
	h, _, _ := pCreateWindowExW.Call(ex, uintptr(unsafe.Pointer(utf16(class))), uintptr(unsafe.Pointer(utf16(""))),
		style, 0, 0, 10, 10, 0, 0, uintptr(hinst), 0)
	return h
}

func (o *overlay) init() {
	registerClass("ReKaptureOverlay", syscall.NewCallback(overlayProc), 0)
	mk := func() uintptr {
		h := createWindow("ReKaptureOverlay", exLayer|exTop|exTool|exTransp|exNoAct, wsPopup)
		pSetLayeredWindowAttrs.Call(h, 0, 242, 2 /*LWA_ALPHA*/)
		pSetWindowDisplayAffinity.Call(h, 0x11 /*WDA_EXCLUDEFROMCAPTURE*/)
		return h
	}
	o.toast = mk()
	o.badge = mk()
}

func (o *overlay) scale(h uintptr) float64 {
	d, _, _ := pGetDpiForWindow.Call(h)
	if d == 0 {
		d = 96
	}
	return float64(d) / 96
}

func (o *overlay) font(dpi int) [3]uintptr {
	if f, ok := o.fonts[dpi]; ok {
		return f
	}
	mk := func(px int, weight int) uintptr {
		h, _, _ := pCreateFontW.Call(uintptr(uint32(-int32(px))), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5 /*CLEARTYPE*/, 0,
			uintptr(unsafe.Pointer(utf16("Bahnschrift"))))
		return h
	}
	s := float64(dpi) / 96
	f := [3]uintptr{mk(int(17*s), 600), mk(int(13.5*s), 400), mk(int(15*s), 600)}
	o.fonts[dpi] = f
	return f
}

// place computes the window position in the chosen corner of the monitor
// the user is looking at (where the foreground window is).
func (o *overlay) place(h uintptr, w, hh int, stackBelowBadge bool) (int, int) {
	fg := windows.GetForegroundWindow()
	mon, _, _ := pMonitorFromWindow.Call(uintptr(fg), 2 /*MONITOR_DEFAULTTONEAREST*/)
	var mi [40]byte
	*(*uint32)(unsafe.Pointer(&mi[0])) = 40
	pGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi[0])))
	work := *(*rect)(unsafe.Pointer(&mi[20]))
	s := o.scale(h)
	m := int(22 * s)
	gap := 0
	if stackBelowBadge {
		gap = int(44 * s)
	}
	var x, y int
	switch o.corner {
	case "tl":
		x, y = int(work.Left)+m, int(work.Top)+m+gap
	case "bl":
		x, y = int(work.Left)+m, int(work.Bottom)-m-hh-gap
	case "br":
		x, y = int(work.Right)-m-w, int(work.Bottom)-m-hh-gap
	default:
		x, y = int(work.Right)-m-w, int(work.Top)+m+gap
	}
	return x, y
}

func (o *overlay) show(h uintptr, w, hh int, stack bool) {
	x, y := o.place(h, w, hh, stack)
	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(hh+1), uintptr(10*o.scale(h)), uintptr(10*o.scale(h)))
	pSetWindowRgn.Call(h, rgn, 1)
	pSetWindowPos.Call(h, ^uintptr(0) /*HWND_TOPMOST*/, uintptr(x), uintptr(y), uintptr(w), uintptr(hh), 0x10|0x40 /*NOACTIVATE|SHOWWINDOW*/)
	pInvalidateRect.Call(h, 0, 1)
}

// Toast shows a notice. kind: ok | warn | rec | busy | info.
func (o *overlay) Toast(kind, title, msg string) { o.ToastFor(kind, title, msg, 0) }

// ToastFor shows a notice for ms milliseconds (0 = the default for its kind).
func (o *overlay) ToastFor(kind, title, msg string, ms int) {
	if o.toast == 0 {
		return
	}
	o.kind, o.title, o.msg = kind, title, msg
	s := o.scale(o.toast)
	w, h := int(360*s), int(66*s)
	if msg == "" {
		h = int(46 * s)
	}
	o.show(o.toast, w, h, o.badgeOn)
	pKillTimer.Call(o.toast, 1)
	if kind == "busy" {
		ms = 20000 // replaced by the result notice
	} else if ms <= 0 {
		ms = 2800
	}
	if kind == "warn" && ms < 4500 {
		ms = 4500
	}
	pSetTimer.Call(o.toast, 1, uintptr(ms), 0)
}

// Badge shows or updates the REC timer; empty text hides it.
func (o *overlay) Badge(text string) {
	if o.badge == 0 {
		return
	}
	if text == "" {
		if o.badgeOn {
			pShowWindow.Call(o.badge, 0)
		}
		o.badgeOn = false
		return
	}
	o.badgeTx = text
	s := o.scale(o.badge)
	w, h := int(132*s), int(36*s)
	o.badgeOn = true
	o.show(o.badge, w, h, false) // also re-asserts topmost over games
}

func overlayProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmTimer:
		pKillTimer.Call(hwnd, wp)
		pShowWindow.Call(hwnd, 0)
		return 0
	case wmPaint:
		if hwnd == ov.icon {
			break // drawn with UpdateLayeredWindow, not WM_PAINT
		}
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		var rc rect
		pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		s := ov.scale(hwnd)
		fonts := ov.font(int(s * 96))
		bg, _, _ := pCreateSolidBrush.Call(rgb(0x16, 0x18, 0x1A))
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
		pDeleteObject.Call(bg)
		pSetBkMode.Call(hdc, 1 /*TRANSPARENT*/)
		if hwnd == ov.badge {
			red, _, _ := pCreateSolidBrush.Call(rgb(0xE5, 0x48, 0x4D))
			old, _, _ := pSelectObject.Call(hdc, red)
			np, _, _ := pGetStockObject.Call(8) // NULL_PEN
			oldPen, _, _ := pSelectObject.Call(hdc, np)
			d := int(11 * s)
			cy := int(rc.Bottom) / 2
			pEllipse.Call(hdc, uintptr(int(14*s)), uintptr(cy-d/2), uintptr(int(14*s)+d), uintptr(cy+d/2))
			pSelectObject.Call(hdc, old)
			pSelectObject.Call(hdc, oldPen)
			pDeleteObject.Call(red)
			pSelectObject.Call(hdc, fonts[2])
			pSetTextColor.Call(hdc, rgb(0xF2, 0xF4, 0xF5))
			tr := rect{Left: int32(34 * s), Top: 0, Right: rc.Right - int32(10*s), Bottom: rc.Bottom}
			pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(utf16(ov.badgeTx))), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0x20|0x4|0x800)
		} else {
			accent := ov.accent
			switch ov.kind {
			case "warn":
				accent = rgb(0xF2, 0xA9, 0x3B)
			case "rec":
				accent = rgb(0xE5, 0x48, 0x4D)
			case "busy", "info":
				accent = rgb(0x8C, 0x94, 0x9A)
			}
			ab, _, _ := pCreateSolidBrush.Call(accent)
			bar := rect{Left: 0, Top: 0, Right: int32(5 * s), Bottom: rc.Bottom}
			pFillRect.Call(hdc, uintptr(unsafe.Pointer(&bar)), ab)
			pDeleteObject.Call(ab)
			left := int32(20 * s)
			pSelectObject.Call(hdc, fonts[0])
			pSetTextColor.Call(hdc, rgb(0xF2, 0xF4, 0xF5))
			if ov.msg == "" {
				tr := rect{Left: left, Top: 0, Right: rc.Right - int32(14*s), Bottom: rc.Bottom}
				pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(utf16(ov.title))), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0x20|0x4|0x800|0x8000)
			} else {
				tr := rect{Left: left, Top: int32(11 * s), Right: rc.Right - int32(14*s), Bottom: int32(34 * s)}
				pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(utf16(ov.title))), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0x20|0x800|0x8000)
				pSelectObject.Call(hdc, fonts[1])
				pSetTextColor.Call(hdc, rgb(0xA4, 0xAB, 0xB0))
				tr = rect{Left: left, Top: int32(35 * s), Right: rc.Right - int32(14*s), Bottom: rc.Bottom - int32(6*s)}
				pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(utf16(ov.msg))), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0x20|0x800|0x8000)
			}
		}
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// ---------- icon indicators (rewind / record dot) ----------

var (
	pUpdateLayeredWindow   = user32.NewProc("UpdateLayeredWindow")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pCreateCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	pDeleteDC              = gdi32.NewProc("DeleteDC")
	pCreateDIBSection      = gdi32.NewProc("CreateDIBSection")
	iconCache              = map[string]*iconBitmap{}
	iconWindowClassCreated bool
)

type iconBitmap struct {
	px  int
	bgr []byte // premultiplied BGRA, top-down
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPels, YPels  int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type blendFunction struct{ Op, Flags, Alpha, Format byte }

// iconWindow is a separate per-pixel-alpha window (UpdateLayeredWindow), so
// the round icon has soft edges and a truly transparent background.
func (o *overlay) iconWindow() uintptr {
	if o.icon != 0 {
		return o.icon
	}
	h := createWindow("ReKaptureOverlay", exLayer|exTop|exTool|exTransp|exNoAct, wsPopup)
	pSetWindowDisplayAffinity.Call(h, 0x11 /*WDA_EXCLUDEFROMCAPTURE*/)
	o.icon = h
	return h
}

// ShowIcon shows an indicator in the overlay corner for ms milliseconds.
// Must run on the UI thread (it owns the overlay windows and their timers).
func (o *overlay) ShowIcon(icon, size, hex string, opacity, ms int) {
	if o.toast == 0 {
		return
	}
	h := o.iconWindow()
	s := o.scale(h)
	px := int(float64(IndicatorBase(icon, size)) * s)
	key := icon + "|" + hex + "|" + itoa(opacity) + "|" + itoa(px)
	bm := iconCache[key]
	if bm == nil {
		img := RenderIndicator(icon, px, hex, opacity)
		bm = &iconBitmap{px: px, bgr: make([]byte, px*px*4)}
		for i := 0; i < px*px; i++ {
			bm.bgr[i*4+0] = img.Pix[i*4+2]
			bm.bgr[i*4+1] = img.Pix[i*4+1]
			bm.bgr[i*4+2] = img.Pix[i*4+0]
			bm.bgr[i*4+3] = img.Pix[i*4+3]
		}
		if len(iconCache) > 24 {
			iconCache = map[string]*iconBitmap{}
		}
		iconCache[key] = bm
	}
	x, y := o.place(h, px, px, o.badgeOn)

	screen, _, _ := pGetDC.Call(0)
	mem, _, _ := pCreateCompatibleDC.Call(screen)
	bi := bitmapInfoHeader{Width: int32(px), Height: -int32(px) /*top-down*/, Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits uintptr
	dib, _, _ := pCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0 /*DIB_RGB_COLORS*/, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == 0 {
		pDeleteDC.Call(mem)
		pReleaseDC.Call(0, screen)
		return
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(bits)), len(bm.bgr)), bm.bgr)
	old, _, _ := pSelectObject.Call(mem, dib)
	pt := [2]int32{int32(x), int32(y)}
	sz := [2]int32{int32(px), int32(px)}
	src := [2]int32{0, 0}
	blend := blendFunction{Op: 0 /*AC_SRC_OVER*/, Alpha: 255, Format: 1 /*AC_SRC_ALPHA*/}
	pUpdateLayeredWindow.Call(h, screen, uintptr(unsafe.Pointer(&pt)), uintptr(unsafe.Pointer(&sz)), mem,
		uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), 2 /*ULW_ALPHA*/)
	pSelectObject.Call(mem, old)
	pDeleteObject.Call(dib)
	pDeleteDC.Call(mem)
	pReleaseDC.Call(0, screen)

	pSetWindowPos.Call(h, ^uintptr(0) /*HWND_TOPMOST*/, 0, 0, 0, 0, 0x1|0x2|0x10|0x40 /*NOSIZE|NOMOVE|NOACTIVATE|SHOWWINDOW*/)
	pKillTimer.Call(h, 1)
	pSetTimer.Call(h, 1, uintptr(ms), 0)
}

func itoa(n int) string { return strconv.Itoa(n) }
