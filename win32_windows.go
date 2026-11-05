//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")

	pEnumDisplayMonitors       = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	pEnumDisplaySettingsW      = user32.NewProc("EnumDisplaySettingsW")
	pEnumDisplayDevicesW       = user32.NewProc("EnumDisplayDevicesW")
	pGetWindowTextW            = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW      = user32.NewProc("GetWindowTextLengthW")
	pIsWindowVisible           = user32.NewProc("IsWindowVisible")
	pIsIconic                  = user32.NewProc("IsIconic")
	pGetClientRect             = user32.NewProc("GetClientRect")
	pGetWindowLongPtrW         = user32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtrW         = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProcW           = user32.NewProc("CallWindowProcW")
	pGetClassNameW             = user32.NewProc("GetClassNameW")
	pShowWindow                = user32.NewProc("ShowWindow")
	pSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
	pRegisterHotKey            = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey          = user32.NewProc("UnregisterHotKey")
	pRegisterClassExW          = user32.NewProc("RegisterClassExW")
	pCreateWindowExW           = user32.NewProc("CreateWindowExW")
	pDefWindowProcW            = user32.NewProc("DefWindowProcW")
	pSetWindowPos              = user32.NewProc("SetWindowPos")
	pSetLayeredWindowAttrs     = user32.NewProc("SetLayeredWindowAttributes")
	pSetWindowRgn              = user32.NewProc("SetWindowRgn")
	pBeginPaint                = user32.NewProc("BeginPaint")
	pEndPaint                  = user32.NewProc("EndPaint")
	pFillRect                  = user32.NewProc("FillRect")
	pDrawTextW                 = user32.NewProc("DrawTextW")
	pInvalidateRect            = user32.NewProc("InvalidateRect")
	pSetTimer                  = user32.NewProc("SetTimer")
	pKillTimer                 = user32.NewProc("KillTimer")
	pGetDpiForWindow           = user32.NewProc("GetDpiForWindow")
	pMonitorFromWindow         = user32.NewProc("MonitorFromWindow")
	pMonitorFromPoint          = user32.NewProc("MonitorFromPoint")
	pSetWindowDisplayAffinity  = user32.NewProc("SetWindowDisplayAffinity")
	pCreatePopupMenu           = user32.NewProc("CreatePopupMenu")
	pAppendMenuW               = user32.NewProc("AppendMenuW")
	pTrackPopupMenu            = user32.NewProc("TrackPopupMenu")
	pDestroyMenu               = user32.NewProc("DestroyMenu")
	pGetCursorPos              = user32.NewProc("GetCursorPos")
	pPostMessageW              = user32.NewProc("PostMessageW")
	pLoadImageW                = user32.NewProc("LoadImageW")
	pRegisterWindowMessageW    = user32.NewProc("RegisterWindowMessageW")
	pMessageBoxW               = user32.NewProc("MessageBoxW")
	pFindWindowW               = user32.NewProc("FindWindowW")
	pIsWindow                  = user32.NewProc("IsWindow")
	pSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")

	pCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pCreateFontW        = gdi32.NewProc("CreateFontW")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pSetTextColor       = gdi32.NewProc("SetTextColor")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	pEllipse            = gdi32.NewProc("Ellipse")
	pGetStockObject     = gdi32.NewProc("GetStockObject")

	pShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW    = shell32.NewProc("ShellExecuteW")

	pDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	pDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")

	pCreateMutexW = kernel32.NewProc("CreateMutexW")

	pCoCreateInstance = ole32.NewProc("CoCreateInstance")
	pCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }

func utf16(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }

func messageBox(title, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16(text))), uintptr(unsafe.Pointer(utf16(title))), flags)
	return int(r)
}

// ---------- monitors ----------

type MonitorInfo struct {
	ID      string `json:"id"` // \\.\DISPLAY1
	Name    string `json:"name"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Hz      int    `json:"hz"`
	Primary bool   `json:"primary"`
	handle  uintptr
}

// Windows allows only a limited number of Go callbacks per process, so the
// enumeration callbacks are created once and share state under a lock
// (creating one per call would crash the app after ~2000 calls).
var (
	enumMu      sync.Mutex
	enumMonFn   func(hmon uintptr)
	enumWinFn   func(hwnd uintptr)
	enumMonProc = syscall.NewCallback(func(hmon, hdc, lprc, lparam uintptr) uintptr { enumMonFn(hmon); return 1 })
	enumWinProc = syscall.NewCallback(func(hwnd, lparam uintptr) uintptr { enumWinFn(hwnd); return 1 })
)

func ListMonitors() []MonitorInfo {
	var out []MonitorInfo
	enumMu.Lock()
	defer enumMu.Unlock()
	enumMonFn = func(hmon uintptr) {
		var mi [104]byte // MONITORINFOEXW
		binary.LittleEndian.PutUint32(mi[0:], 104)
		if r, _, _ := pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi[0]))); r == 0 {
			return
		}
		l := int32(binary.LittleEndian.Uint32(mi[4:]))
		t := int32(binary.LittleEndian.Uint32(mi[8:]))
		rr := int32(binary.LittleEndian.Uint32(mi[12:]))
		b := int32(binary.LittleEndian.Uint32(mi[16:]))
		flags := binary.LittleEndian.Uint32(mi[36:])
		dev := windows.UTF16ToString((*[32]uint16)(unsafe.Pointer(&mi[40]))[:])
		m := MonitorInfo{ID: dev, Width: int(rr - l), Height: int(b - t), Primary: flags&1 != 0, handle: hmon}
		// Physical mode (independent of DPI scaling) and refresh rate.
		var dm [220]byte
		binary.LittleEndian.PutUint16(dm[68:], 220)
		if r, _, _ := pEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(utf16(dev))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&dm[0]))); r != 0 {
			if w := int(binary.LittleEndian.Uint32(dm[172:])); w > 0 {
				m.Width = w
				m.Height = int(binary.LittleEndian.Uint32(dm[176:]))
			}
			m.Hz = int(binary.LittleEndian.Uint32(dm[184:]))
		}
		out = append(out, m)
	}
	pEnumDisplayMonitors.Call(0, 0, enumMonProc, 0)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i := range out {
		n := strings.TrimPrefix(out[i].ID, `\\.\DISPLAY`)
		out[i].Name = fmt.Sprintf("Display %s — %d×%d", n, out[i].Width, out[i].Height)
		if out[i].Hz > 1 {
			out[i].Name += fmt.Sprintf(" @ %d Hz", out[i].Hz)
		}
		if out[i].Primary {
			out[i].Name += " (main)"
		}
	}
	return out
}

// ---------- windows ----------

type WindowInfo struct {
	Exe   string `json:"exe"`
	Title string `json:"title"`
	hwnd  windows.HWND
	area  int
	w, h  int
}

func windowText(h windows.HWND) string {
	n, _, _ := pGetWindowTextLengthW.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
}

func className(h windows.HWND) string {
	buf := make([]uint16, 256)
	pGetClassNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func processExe(h windows.HWND) (string, uint32) {
	var pid uint32
	windows.GetWindowThreadProcessId(h, &pid)
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", pid
	}
	defer windows.CloseHandle(ph)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(ph, 0, &buf[0], &n); err != nil {
		return "", pid
	}
	return filepath.Base(windows.UTF16ToString(buf[:n])), pid
}

func isCloaked(h windows.HWND) bool {
	var v uint32
	r, _, _ := pDwmGetWindowAttribute.Call(uintptr(h), 14 /*DWMWA_CLOAKED*/, uintptr(unsafe.Pointer(&v)), 4)
	return r == 0 && v != 0
}

var skipExes = map[string]bool{"explorer.exe": true, "textinputhost.exe": true, "applicationframehost.exe": true,
	"shellexperiencehost.exe": true, "searchhost.exe": true, "startmenuexperiencehost.exe": true,
	"nvidia overlay.exe": true, "nvidia app.exe": true, "systemsettings.exe": false}

// ListWindows returns capturable app windows, one per program (largest wins).
func ListWindows() []WindowInfo {
	self := uint32(windows.GetCurrentProcessId())
	byExe := map[string]WindowInfo{}
	enumMu.Lock()
	defer enumMu.Unlock()
	enumWinFn = func(hwnd uintptr) {
		h := windows.HWND(hwnd)
		if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 || isCloaked(h) {
			return
		}
		ex, _, _ := pGetWindowLongPtrW.Call(hwnd, ^uintptr(19) /* GWL_EXSTYLE = -20 */)
		if ex&0x80 != 0 { // WS_EX_TOOLWINDOW
			return
		}
		title := windowText(h)
		if strings.TrimSpace(title) == "" {
			return
		}
		exe, pid := processExe(h)
		if exe == "" || pid == self || skipExes[strings.ToLower(exe)] {
			return
		}
		var r rect
		pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
		w, hh := int(r.Right-r.Left), int(r.Bottom-r.Top)
		if w < 160 || hh < 120 {
			if ic, _, _ := pIsIconic.Call(hwnd); ic == 0 {
				return
			}
		}
		key := strings.ToLower(exe)
		if cur, ok := byExe[key]; !ok || w*hh > cur.area {
			byExe[key] = WindowInfo{Exe: exe, Title: title, hwnd: h, area: w * hh, w: w, h: hh}
		}
	}
	windows.EnumWindows(enumWinProc, nil)
	var out []WindowInfo
	for _, v := range byExe {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// ForegroundTitle names what the user is looking at, for clip names/folders.
func ForegroundTitle() string {
	h := windows.GetForegroundWindow()
	if h == 0 {
		return "Desktop"
	}
	exe, pid := processExe(h)
	cls := className(h)
	if pid == uint32(windows.GetCurrentProcessId()) || cls == "Progman" || cls == "WorkerW" ||
		cls == "Shell_TrayWnd" || strings.EqualFold(exe, "explorer.exe") {
		return "Desktop"
	}
	t := strings.TrimSpace(windowText(h))
	if t == "" {
		t = strings.TrimSuffix(exe, filepath.Ext(exe))
	}
	return t
}

// ---------- GPU ----------

func GPUName() string {
	var names []string
	for i := uint32(0); i < 16; i++ {
		var dd [840]byte
		binary.LittleEndian.PutUint32(dd[0:], 840)
		if r, _, _ := pEnumDisplayDevicesW.Call(0, uintptr(i), uintptr(unsafe.Pointer(&dd[0])), 0); r == 0 {
			break
		}
		s := windows.UTF16ToString((*[128]uint16)(unsafe.Pointer(&dd[68]))[:])
		if s != "" {
			names = append(names, s)
		}
	}
	for _, n := range names {
		if strings.Contains(strings.ToUpper(n), "NVIDIA") {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return "Unknown GPU"
}

// ---------- misc ----------

func shellOpen(verb, file, params string) {
	var pv, pp uintptr
	if verb != "" {
		pv = uintptr(unsafe.Pointer(utf16(verb)))
	}
	if params != "" {
		pp = uintptr(unsafe.Pointer(utf16(params)))
	}
	pShellExecuteW.Call(0, pv, uintptr(unsafe.Pointer(utf16(file))), pp, 0, 1)
}

func setDarkTitleBar(hwnd uintptr, accent string) {
	on := uint32(1)
	pDwmSetWindowAttribute.Call(hwnd, 20 /*DWMWA_USE_IMMERSIVE_DARK_MODE*/, uintptr(unsafe.Pointer(&on)), 4)
	caption := uint32(0x00141414) // COLORREF is 0x00BBGGRR
	pDwmSetWindowAttribute.Call(hwnd, 35 /*DWMWA_CAPTION_COLOR*/, uintptr(unsafe.Pointer(&caption)), 4)
	text := uint32(0x00F0F0F0)
	pDwmSetWindowAttribute.Call(hwnd, 36 /*DWMWA_TEXT_COLOR*/, uintptr(unsafe.Pointer(&text)), 4)
	setBorderColor(hwnd, accent)
}

// setBorderColor tints the Windows 11 window border with the accent color.
func setBorderColor(hwnd uintptr, accent string) {
	r, g, b := AccentRGB(accent)
	border := uint32(r) | uint32(g)<<8 | uint32(b)<<16 // COLORREF 0x00BBGGRR
	pDwmSetWindowAttribute.Call(hwnd, 34 /*DWMWA_BORDER_COLOR*/, uintptr(unsafe.Pointer(&border)), 4)
}

// pickFolder shows the modern Windows folder picker (IFileOpenDialog).
func pickFolder(owner uintptr, start string) (string, bool) {
	clsid := windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4dde, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iid := windows.GUID{Data1: 0xd57c7288, Data2: 0xd4ad, Data3: 0x4768, Data4: [8]byte{0xbe, 0x02, 0x9d, 0x96, 0x95, 0x32, 0xd9, 0x60}}
	var dlg uintptr
	if hr, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsid)), 0, 1 /*CLSCTX_INPROC_SERVER*/, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&dlg))); hr != 0 {
		return "", false
	}
	vt := *(*[27]uintptr)(unsafe.Pointer(*(*uintptr)(unsafe.Pointer(dlg))))
	defer syscall.SyscallN(vt[2], dlg) // Release
	var opts uint32
	syscall.SyscallN(vt[10], dlg, uintptr(unsafe.Pointer(&opts)))
	syscall.SyscallN(vt[9], dlg, uintptr(opts|0x20|0x40)) // FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM
	syscall.SyscallN(vt[17], dlg, uintptr(unsafe.Pointer(utf16("Choose where to save clips"))))
	if hr, _, _ := syscall.SyscallN(vt[3], dlg, owner); hr != 0 { // Show
		return "", false
	}
	var item uintptr
	if hr, _, _ := syscall.SyscallN(vt[20], dlg, uintptr(unsafe.Pointer(&item))); hr != 0 || item == 0 { // GetResult
		return "", false
	}
	ivt := *(*[6]uintptr)(unsafe.Pointer(*(*uintptr)(unsafe.Pointer(item))))
	defer syscall.SyscallN(ivt[2], item)
	var p *uint16
	if hr, _, _ := syscall.SyscallN(ivt[5], item, 0x80058000 /*SIGDN_FILESYSPATH*/, uintptr(unsafe.Pointer(&p))); hr != 0 || p == nil {
		return "", false
	}
	defer pCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return windows.UTF16PtrToString(p), true
}
