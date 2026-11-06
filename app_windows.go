//go:build windows

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui/index.html
var indexHTML string

const appVersion = "2.5.2"

const (
	wmClose     = 0x0010
	wmHotkey    = 0x0312
	wmApp       = 0x8000
	wmTray      = wmApp + 1
	wmShowMain  = wmApp + 7
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmLDblClk   = 0x0203
)

func init() { runtime.LockOSThread() }

type App struct {
	wv             webview2.WebView
	hwnd           uintptr
	host           uintptr
	engine         *Engine
	plat           *winPlatform
	capTesting     atomic.Bool
	settings       Settings
	setPath        string
	logPath        string
	ffmpeg         string
	nvenc          map[string]bool
	nvMu           sync.Mutex
	hkErr          string
	tray           notifyIconData
	trayOK         bool
	taskbarCreated uintptr
	oldProc        uintptr
	status         *coalescer // delivers status to the window (see coalesce.go)
	statusOnce     sync.Once
	lastSt         atomic.Value
	quitting       int32
	hidOnce        bool
	visible        bool
	media          *MediaServer
	outDir         atomic.Value // string; read by the media server goroutines
	editor         *Editor
	sharer         *Sharer
	discordMB      atomic.Int32
	updater        *Updater
}

var app *App

// singleInstance claims the "ReKapture is running" mutex, waiting up to
// wait for another copy to close. It returns false if one is still running.
func singleInstance(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		h, _, e := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(utf16(`Local\ReKaptureApp`))))
		if e != windows.ERROR_ALREADY_EXISTS {
			return true // keep the handle for as long as the app runs
		}
		if h != 0 {
			windows.CloseHandle(windows.Handle(h)) // or the mutex never goes away
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func main() {
	startInTray, afterUpdate := false, false
	for _, a := range os.Args[1:] {
		switch a {
		case "--tray":
			startInTray = true
		case "--updated":
			afterUpdate = true // a portable update relaunched us; the old copy is closing
		}
	}
	pSetProcessDpiAwarenessCtx.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)

	// One copy at a time: a second launch just brings the first one forward.
	wait := time.Duration(0)
	if afterUpdate {
		wait = 20 * time.Second
	}
	if !singleInstance(wait) {
		if h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(utf16("webview"))), uintptr(unsafe.Pointer(utf16("ReKapture")))); h != 0 {
			pPostMessageW.Call(h, wmShowMain, 0, 0)
		}
		return
	}

	os.MkdirAll(localDataDir(), 0o755)
	logPath := filepath.Join(localDataDir(), "rekapture.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 5<<20 {
		os.Rename(logPath, logPath+".old")
	}
	if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(lf))
	}
	log.Printf("ReKapture %s starting", appVersion)
	if portable() {
		log.Printf("portable mode: settings and logs are in %s", appDataDir())
	}

	ffmpeg := findFFmpeg()
	if ffmpeg == "" {
		messageBox("ReKapture", "ffmpeg.exe is missing.\n\nKeep the \"ffmpeg\" folder next to ReKapture.exe (re-extract the zip if needed).", 0x10)
		return
	}
	initKillJob()
	ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)

	def := DefaultSettings(videosDir())
	setPath := filepath.Join(appDataDir(), "settings.json")
	migrateOldNames(setPath)
	s := LoadSettings(setPath, def)
	if dir := migrateClipsFolder(s.OutputDir); dir != s.OutputDir {
		s.OutputDir = dir
		SaveSettings(setPath, s)
	}

	os.Setenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR", "FF050606")
	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:         os.Getenv("REKAPTURE_DEBUG") != "",
		AutoFocus:     true,
		DataPath:      filepath.Join(localDataDir(), "WebView2"),
		WindowOptions: webview2.WindowOptions{Title: "ReKapture", Width: 1100, Height: 760, IconId: 1, Center: true},
	})
	if wv == nil {
		if messageBox("ReKapture", "ReKapture needs the Microsoft Edge WebView2 Runtime, which is missing on this PC.\n\nOpen the download page now?", 0x34) == 6 {
			shellOpen("open", "https://developer.microsoft.com/microsoft-edge/webview2/", "")
		}
		return
	}
	a := &App{wv: wv, hwnd: uintptr(wv.Window()), settings: s, setPath: setPath, logPath: logPath, ffmpeg: ffmpeg, nvenc: map[string]bool{}}
	app = a
	setDarkTitleBar(a.hwnd, s.Accent)
	a.outDir.Store(s.OutputDir)
	wv.SetSize(820, 560, webview2.HintMin)
	a.subclassMain()
	a.initHost()
	ov.corner = s.OverlayCorner
	ov.setAccent(s.Accent)
	ov.init()

	plat := &winPlatform{notify: a.notify}
	a.plat = plat
	if m, err := NewMediaServer(ffmpeg, filepath.Join(localDataDir(), "previews"), plat.Command,
		func() []string { d, _ := a.outDir.Load().(string); return []string{d} },
		func() bool { a.nvMu.Lock(); defer a.nvMu.Unlock(); return len(a.nvenc) == 0 || a.nvenc["h264"] }); err == nil {
		a.media = m
	} else {
		log.Printf("clip preview server unavailable: %v", err)
	}
	a.engine = NewEngine(plat, ffmpeg, s, a.onStatus, a.onEngineSettings)
	a.discordMB.Store(int32(s.DiscordMB))
	a.sharer = &Sharer{FFmpeg: ffmpeg, Command: plat.Command, TempDir: filepath.Join(localDataDir(), "share"),
		ClipDir: filepath.Join(localDataDir(), "clipboard"),
		LimitMB: func() int { return int(a.discordMB.Load()) },
		NVENC:   func() bool { a.nvMu.Lock(); defer a.nvMu.Unlock(); return len(a.nvenc) == 0 || a.nvenc["h264"] }}
	os.RemoveAll(filepath.Join(localDataDir(), "share")) // leftovers from a crash
	a.editor = &Editor{FFmpeg: ffmpeg, Command: plat.Command,
		NVENC: func() bool { a.nvMu.Lock(); defer a.nvMu.Unlock(); return len(a.nvenc) == 0 || a.nvenc["h264"] }}
	a.bind()
	if a.media != nil {
		a.media.UI = []byte(indexHTML)
		wv.Navigate(a.media.Base() + "/")
	} else {
		wv.SetHtml(indexHTML)
	}
	a.registerHotkeys()
	a.addTray()
	a.visible = true
	if startInTray && s.StartMinimized {
		a.hide()
	}
	if !startInTray && s.StartWithWindows {
		setStartWithWindows(true) // keep the path current if the app was moved
	}
	a.noteVersionChange()
	a.setupUpdater()
	go a.engine.RecoverStale()
	go a.probeNVENC()
	go a.ticker()

	wv.Run()

	log.Printf("shutting down")
	a.removeTray()
	a.engine.Shutdown()
}

// ---------- window ----------

func (a *App) subclassMain() {
	proc := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		switch msg {
		case wmClose:
			if atomic.LoadInt32(&a.quitting) == 0 {
				a.hide()
				if !a.hidOnce {
					a.hidOnce = true
					ov.Toast("info", "ReKapture is still running", "Find it in the system tray. Right-click it to quit.")
				}
				return 0
			}
		case wmShowMain:
			a.show()
			return 0
		case 0x0016: // WM_ENDSESSION: Windows is signing out or shutting down
			if wp != 0 {
				log.Printf("windows session ending; saving")
				a.removeTray()
				a.engine.Shutdown()
			}
			return 0
		}
		r, _, _ := pCallWindowProcW.Call(a.oldProc, hwnd, msg, wp, lp)
		return r
	})
	a.oldProc, _, _ = pSetWindowLongPtrW.Call(a.hwnd, ^uintptr(3) /*GWLP_WNDPROC = -4*/, proc)
}

func (a *App) hide() {
	pShowWindow.Call(a.hwnd, 0)
	a.visible = false
}

func (a *App) show() {
	pShowWindow.Call(a.hwnd, 9) // SW_RESTORE
	pShowWindow.Call(a.hwnd, 5)
	pSetForegroundWindow.Call(a.hwnd)
	a.visible = true
	a.pushStatus(a.engine.Status())
}

func (a *App) quit() {
	if !atomic.CompareAndSwapInt32(&a.quitting, 0, 1) {
		return
	}
	a.hide()
	if a.engine.Recording() || a.engine.Status().Finalizing > 0 {
		ov.Toast("busy", "Finishing your recording…", "ReKapture will close when it's saved")
	}
	go func() {
		a.engine.Shutdown()
		a.wv.Dispatch(func() { a.wv.Terminate() })
	}()
}

// ---------- host window: hotkeys + tray ----------

func (a *App) initHost() {
	registerClass("ReKaptureHost", syscall.NewCallback(a.hostProc), 0)
	a.host = createWindow("ReKaptureHost", exTool, wsPopup)
	a.taskbarCreated, _, _ = pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
}

func (a *App) hostProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch {
	case msg == wmHotkey:
		switch wp {
		case 1:
			go a.engine.SaveReplay()
		case 2:
			go a.engine.ToggleRecording()
		case 3:
			go a.toggleReplay()
		}
		return 0
	case msg == wmTray:
		switch lp & 0xFFFF {
		case wmLButtonUp, wmLDblClk:
			a.show()
		case wmRButtonUp:
			a.trayMenu()
		}
		return 0
	case msg == a.taskbarCreated && msg != 0:
		a.trayOK = false
		a.addTray()
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (a *App) toggleReplay() {
	on := !a.engine.ReplayOn()
	a.engine.SetReplay(on)
	a.wv.Dispatch(func() {
		a.settings.ReplayEnabled = on
		SaveSettings(a.setPath, a.settings)
		a.evalJSON("__glSettings", a.settings)
	})
}

func (a *App) registerHotkeys() {
	a.unregisterHotkeys()
	var errs []string
	used := map[Hotkey]bool{}
	for i, h := range []struct {
		hk   Hotkey
		name string
	}{{a.settings.HotkeySave, "Save replay"}, {a.settings.HotkeyRecord, "Recording"}, {a.settings.HotkeyToggle, "Instant Replay on/off"}} {
		if h.hk.Key == 0 {
			continue
		}
		if used[h.hk] {
			errs = append(errs, fmt.Sprintf("%s uses the same keys as another shortcut.", h.name))
			continue
		}
		used[h.hk] = true
		r, _, _ := pRegisterHotKey.Call(a.host, uintptr(i+1), uintptr(h.hk.Mods|0x4000 /*MOD_NOREPEAT*/), uintptr(h.hk.Key))
		if r == 0 {
			errs = append(errs, fmt.Sprintf("%s is already used by another app, so \"%s\" won't work. Pick different keys.", HotkeyString(h.hk), h.name))
		}
	}
	a.hkErr = strings.Join(errs, " ")
	if a.hkErr != "" {
		log.Printf("hotkeys: %s", a.hkErr)
	}
}

func (a *App) unregisterHotkeys() {
	for i := 1; i <= 3; i++ {
		pUnregisterHotKey.Call(a.host, uintptr(i))
	}
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

func (a *App) addTray() {
	var hinst windows.Handle
	windows.GetModuleHandleEx(0, nil, &hinst)
	icon, _, _ := pLoadImageW.Call(uintptr(hinst), 1, 1 /*IMAGE_ICON*/, 0, 0, 0x40 /*LR_DEFAULTSIZE*/)
	if sz, _, _ := user32.NewProc("GetSystemMetrics").Call(49); sz > 0 {
		if i2, _, _ := pLoadImageW.Call(uintptr(hinst), 1, 1, sz, sz, 0); i2 != 0 {
			icon = i2
		}
	}
	a.tray = notifyIconData{HWnd: a.host, UID: 1, UFlags: 0x1 | 0x2 | 0x4, UCallbackMessage: wmTray, HIcon: icon}
	a.tray.CbSize = uint32(unsafe.Sizeof(a.tray))
	copy(a.tray.SzTip[:], windows.StringToUTF16("ReKapture"))
	r, _, _ := pShellNotifyIconW.Call(0 /*NIM_ADD*/, uintptr(unsafe.Pointer(&a.tray)))
	a.trayOK = r != 0
}

func (a *App) setTrayTip(t string) {
	if !a.trayOK {
		return
	}
	u := windows.StringToUTF16(t)
	if len(u) > 127 {
		u = append(u[:126], 0)
	}
	a.tray.SzTip = [128]uint16{}
	copy(a.tray.SzTip[:], u)
	a.tray.UFlags = 0x4
	pShellNotifyIconW.Call(1 /*NIM_MODIFY*/, uintptr(unsafe.Pointer(&a.tray)))
}

func (a *App) removeTray() {
	if a.trayOK {
		pShellNotifyIconW.Call(2 /*NIM_DELETE*/, uintptr(unsafe.Pointer(&a.tray)))
		a.trayOK = false
	}
}

func (a *App) trayMenu() {
	st := a.engine.Status()
	m, _, _ := pCreatePopupMenu.Call()
	add := func(id uintptr, text string, flags uintptr) {
		pAppendMenuW.Call(m, flags, id, uintptr(unsafe.Pointer(utf16(text))))
	}
	add(1, "Open ReKapture", 0)
	add(0, "", 0x800)
	saveFlags := uintptr(0)
	if !st.ReplayOn {
		saveFlags = 1 // grayed
	}
	add(2, "Save replay\t"+HotkeyString(a.settings.HotkeySave), saveFlags)
	if st.Recording {
		add(3, "Stop recording\t"+HotkeyString(a.settings.HotkeyRecord), 0)
	} else {
		add(3, "Start recording\t"+HotkeyString(a.settings.HotkeyRecord), 0)
	}
	rf := uintptr(0)
	if st.ReplayOn {
		rf = 0x8 // checked
	}
	add(4, "Instant Replay", rf)
	add(5, "Open clips folder", 0)
	add(0, "", 0x800)
	add(6, "Quit", 0)
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(a.host)
	cmd, _, _ := pTrackPopupMenu.Call(m, 0x100|0x2|0x20, uintptr(pt.X), uintptr(pt.Y), 0, a.host, 0)
	pPostMessageW.Call(a.host, 0, 0, 0)
	pDestroyMenu.Call(m)
	switch cmd {
	case 1:
		a.show()
	case 2:
		go a.engine.SaveReplay()
	case 3:
		go a.engine.ToggleRecording()
	case 4:
		go a.toggleReplay()
	case 5:
		os.MkdirAll(a.settings.OutputDir, 0o755)
		shellOpen("open", a.settings.OutputDir, "")
	case 6:
		a.quit()
	}
}

// ---------- status & notices ----------

func (a *App) notify(kind, title, msg string) {
	a.wv.Dispatch(func() {
		if a.settings.OverlayToasts {
			ov.Toast(kind, title, msg)
		} else if a.visible && kind != "busy" {
			t := title
			if msg != "" {
				t += ". " + msg
			}
			a.evalJSON2("__glFlash", t, kind == "warn")
		}
	})
}

func (a *App) onStatus(st Status) {
	a.lastSt.Store(st)
	a.statusOnce.Do(func() {
		a.status = &coalescer{
			dispatch: a.wv.Dispatch,
			deliver: func() {
				if v, ok := a.lastSt.Load().(Status); ok {
					a.pushStatus(v)
				}
			},
			now:        time.Now,
			retryAfter: time.Second,
		}
	})
	a.status.Trigger()
}

func (a *App) pushStatus(st Status) {
	if a.visible {
		a.evalJSON("__glStatus", st)
	}
	tip := "ReKapture: Instant Replay off"
	switch st.State {
	case "buffering":
		tip = "ReKapture: Instant Replay on"
	case "recording":
		tip = "ReKapture: recording " + clockText(st.RecordSeconds)
	case "error", "waiting":
		tip = "ReKapture: " + st.Error
	}
	a.setTrayTip(tip)
	if st.Recording && a.settings.OverlayRec {
		ov.Badge("REC  " + clockText(st.RecordSeconds))
	} else {
		ov.Badge("")
	}
}

func (a *App) ticker() {
	for range time.Tick(time.Second) {
		if a.engine.Recording() {
			a.onStatus(a.engine.Status())
		}
	}
}

func clockText(s int) string {
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// onEngineSettings: the engine added a detected game to the games list.
// Save it and show it in the UI (on the UI thread, like every settings change).
func (a *App) onEngineSettings(s Settings) {
	a.wv.Dispatch(func() {
		a.settings.Games = s.Games
		if err := SaveSettings(a.setPath, a.settings); err != nil {
			log.Printf("couldn't save the games list: %v", err)
		}
		a.evalJSON("__glGames", s.Games)
	})
}

func (a *App) evalJSON(fn string, v interface{}) {
	b, _ := json.Marshal(v)
	a.wv.Eval(fmt.Sprintf("window.%s && window.%s(%s)", fn, fn, b))
}

func (a *App) evalJSON2(fn string, v1, v2 interface{}) {
	b1, _ := json.Marshal(v1)
	b2, _ := json.Marshal(v2)
	a.wv.Eval(fmt.Sprintf("window.%s && window.%s(%s,%s)", fn, fn, b1, b2))
}

// probeNVENC checks which NVENC codecs this GPU and driver can encode.
func (a *App) probeNVENC() {
	res := map[string]bool{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, c := range []string{"h264", "hevc", "av1"} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			cmd := exec.Command(a.ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=1280x720:r=30:d=0.3",
				"-c:v", EncoderName(c), "-f", "null", "-")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
			out, err := cmd.CombinedOutput()
			mu.Lock()
			res[c] = err == nil
			mu.Unlock()
			if err != nil {
				log.Printf("NVENC %s unavailable: %s", c, strings.TrimSpace(string(out)))
			}
		}(c)
	}
	wg.Wait()
	a.nvMu.Lock()
	a.nvenc = res
	a.nvMu.Unlock()
	info := map[string]bool{"checked": true, "h264": res["h264"], "hevc": res["hevc"], "av1": res["av1"]}
	a.wv.Dispatch(func() {
		a.evalJSON("__glNvenc", info)
		if !res["h264"] && !res["hevc"] && !res["av1"] {
			ov.Toast("warn", "NVENC isn't available", "Update your NVIDIA driver, then restart ReKapture")
		} else if !res[a.settings.Codec] {
			// Fall back to a codec this GPU supports.
			for _, c := range []string{"h264", "hevc"} {
				if res[c] {
					a.settings.Codec = c
					SaveSettings(a.setPath, a.settings)
					go a.engine.Apply(a.settings)
					a.evalJSON("__glSettings", a.settings)
					break
				}
			}
		}
	})
}

// ---------- JS bindings ----------

func (a *App) sources() map[string]interface{} {
	outs, ins := ListAudioDevices()
	return map[string]interface{}{"monitors": ListMonitors(), "windows": ListWindows(), "outputs": outs, "inputs": ins}
}

func (a *App) bind() {
	w := a.wv
	w.Bind("glGetInit", func() map[string]interface{} {
		m := a.sources()
		m["settings"] = a.settings
		m["status"] = a.engine.Status()
		m["gpu"] = GPUName()
		m["version"] = appVersion
		m["portable"] = portable()
		m["hotkeyErrors"] = a.hkErr
		if a.media != nil {
			m["mediaBase"] = a.media.Base()
		}
		a.nvMu.Lock()
		if len(a.nvenc) > 0 {
			m["nvenc"] = map[string]bool{"checked": true, "h264": a.nvenc["h264"], "hevc": a.nvenc["hevc"], "av1": a.nvenc["av1"]}
		}
		a.nvMu.Unlock()
		return m
	})
	w.Bind("glRefreshSources", func() map[string]interface{} { return a.sources() })
	w.Bind("glSaveSettings", func(s Settings) map[string]interface{} {
		s.Normalize(DefaultSettings(videosDir()))
		s.ReplayEnabled = a.engine.ReplayOn()
		old := a.settings
		if a.engine.Recording() && VideoChanged(old, s) {
			return map[string]interface{}{"settings": old, "error": "Stop recording before changing capture, video or audio settings.", "hotkeyErrors": a.hkErr}
		}
		errMsg := ""
		if s.StartWithWindows != old.StartWithWindows {
			if err := setStartWithWindows(s.StartWithWindows); err != nil {
				errMsg = "Couldn't change the startup setting: " + err.Error()
				s.StartWithWindows = old.StartWithWindows
			}
		}
		if s.OutputDir != old.OutputDir {
			if err := os.MkdirAll(s.OutputDir, 0o755); err != nil {
				errMsg = "Can't save clips there: " + err.Error()
				s.OutputDir = old.OutputDir
			}
		}
		a.settings = s
		a.outDir.Store(s.OutputDir)
		a.discordMB.Store(int32(s.DiscordMB))
		ov.corner = s.OverlayCorner
		if s.Accent != old.Accent {
			setBorderColor(a.hwnd, s.Accent)
			ov.setAccent(s.Accent)
		}
		if err := SaveSettings(a.setPath, s); err != nil {
			errMsg = "Couldn't save settings: " + err.Error()
		}
		if old.HotkeySave != s.HotkeySave || old.HotkeyRecord != s.HotkeyRecord || old.HotkeyToggle != s.HotkeyToggle {
			a.registerHotkeys()
		}
		go a.engine.Apply(s)
		return map[string]interface{}{"settings": s, "error": errMsg, "hotkeyErrors": a.hkErr}
	})
	w.Bind("glSetReplay", func(on bool) {
		a.settings.ReplayEnabled = on
		SaveSettings(a.setPath, a.settings)
		go a.engine.SetReplay(on)
	})
	w.Bind("glSaveReplay", func() { go a.engine.SaveReplay() })
	w.Bind("glToggleRecord", func() { go a.engine.ToggleRecording() })
	w.Bind("glPickFolder", func() {
		// Run the modal dialog from the message loop, not inside the WebView2
		// callback (nested loops inside WebView2 events can deadlock).
		a.wv.Dispatch(func() {
			if p, ok := pickFolder(a.hwnd, a.settings.OutputDir); ok {
				a.evalJSON("__glPicked", p)
			}
		})
	})
	w.Bind("glOpenPath", func(path, mode string) {
		switch mode {
		case "folder":
			os.MkdirAll(path, 0o755)
			shellOpen("open", path, "")
		case "select":
			shellOpen("open", "explorer.exe", `/select,"`+path+`"`)
		case "play":
			shellOpen("open", path, "")
		}
	})
	w.Bind("glOpenLog", func() { shellOpen("open", a.logPath, "") })
	w.Bind("glHide", func() { a.hide() })
	w.Bind("glQuit", func() { a.quit() })
	// ----- editor -----
	inClips := func(p string) bool {
		if a.media == nil {
			return false
		}
		_, ok := a.media.allowed(p)
		return ok
	}
	w.Bind("glProbeClip", func(path string) (ClipInfo, error) {
		if !inClips(path) {
			return ClipInfo{}, fmt.Errorf("only clips in your clips folder can be edited")
		}
		return ProbeClip(a.ffmpeg, path, exec.Command)
	})
	w.Bind("glEditSave", func(spec EditSpec) error {
		if !inClips(spec.Path) {
			return fmt.Errorf("only clips in your clips folder can be edited")
		}
		if a.editor.Busy() {
			return fmt.Errorf("another edit is still saving")
		}
		go func() {
			var last time.Time
			out, err := a.editor.Run(spec, func(p float64) {
				if time.Since(last) > 150*time.Millisecond || p >= 1 {
					last = time.Now()
					a.wv.Dispatch(func() { a.evalJSON("__glEditProgress", p) })
				}
			})
			res := map[string]interface{}{"ok": err == nil, "path": out}
			if err != nil {
				res["error"] = err.Error()
				res["canceled"] = err == ErrCanceled
				log.Printf("edit failed: %v", err)
			} else {
				log.Printf("edit saved: %s", out)
			}
			a.engine.RefreshClips()
			a.wv.Dispatch(func() { a.evalJSON("__glEditDone", res) })
		}()
		return nil
	})
	w.Bind("glEditCancel", func() { a.editor.Cancel() })

	// ----- sharing -----
	w.Bind("glWebhookCheck", func(raw string) {
		go func() {
			hook, name, err := CheckWebhook(context.Background(), raw)
			res := map[string]interface{}{"ok": err == nil, "webhook": hook, "name": name}
			if err != nil {
				res["error"] = err.Error()
			}
			a.wv.Dispatch(func() { a.evalJSON("__glWebhookChecked", res) })
		}()
	})
	w.Bind("glSharePlan", func(path string, mb int) (ShrinkPlan, error) {
		if !inClips(path) {
			return ShrinkPlan{}, fmt.Errorf("only clips in your clips folder can be shared")
		}
		return a.sharer.Plan(path, mb) // mb 0 = the server limit for posts
	})
	w.Bind("glCopyClip", func(path string, mb int) error {
		if !inClips(path) {
			return fmt.Errorf("only clips in your clips folder can be shared")
		}
		if a.sharer.Busy() {
			return fmt.Errorf("another clip is still being shared")
		}
		go func() {
			var last time.Time
			out, size, err := a.sharer.CopyReady(path, mb, func(p float64) {
				if time.Since(last) > 150*time.Millisecond || p >= 1 {
					last = time.Now()
					a.wv.Dispatch(func() { a.evalJSON("__glShareProgress", map[string]interface{}{"stage": "prepare", "p": p}) })
				}
			})
			a.wv.Dispatch(func() {
				res := map[string]interface{}{"ok": err == nil, "sizeMB": size}
				if err == nil {
					err = setClipboardFile(a.hwnd, out)
				}
				if err != nil {
					res["ok"] = false
					res["error"] = err.Error()
					res["canceled"] = err == ErrCanceled
					log.Printf("copy for Discord failed: %v", err)
				} else {
					log.Printf("copied %s for Discord (%.1f MB)", filepath.Base(out), size)
					ov.Toast("ok", "Clip copied", "Press Ctrl+V in any Discord chat")
				}
				a.evalJSON("__glCopyDone", res)
			})
		}()
		return nil
	})
	w.Bind("glShare", func(path string, hookIndex int, message string) error {
		if !inClips(path) {
			return fmt.Errorf("only clips in your clips folder can be shared")
		}
		if hookIndex < 0 || hookIndex >= len(a.settings.Webhooks) {
			return fmt.Errorf("add a Discord channel on the Sharing page first")
		}
		if a.sharer.Busy() {
			return fmt.Errorf("another clip is still being shared")
		}
		req := ShareRequest{Path: path, Webhook: a.settings.Webhooks[hookIndex], Message: message}
		go func() {
			var last time.Time
			res, err := a.sharer.Share(req, func(stage string, p float64) {
				if time.Since(last) > 150*time.Millisecond || p >= 1 {
					last = time.Now()
					a.wv.Dispatch(func() { a.evalJSON("__glShareProgress", map[string]interface{}{"stage": stage, "p": p}) })
				}
			})
			out := map[string]interface{}{"ok": err == nil, "messageUrl": res.MessageURL, "sizeMB": res.SizeMB, "channel": req.Webhook.Name}
			if err != nil {
				out["error"] = err.Error()
				out["canceled"] = err == ErrCanceled
				log.Printf("share to %s failed: %v", redact(req.Webhook.URL), err)
			} else {
				log.Printf("shared %s to %s (%.1f MB)", filepath.Base(path), redact(req.Webhook.URL), res.SizeMB)
			}
			a.wv.Dispatch(func() {
				a.evalJSON("__glShareDone", out)
				if err == nil {
					ov.Toast("ok", "Posted to "+req.Webhook.Name, "")
				}
			})
		}()
		return nil
	})
	w.Bind("glShareCancel", func() { a.sharer.Cancel() })
	// ----- updates -----
	w.Bind("glUpdateState", func() UpdateState { return a.updater.State() })
	w.Bind("glUpdateCheck", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := a.updater.Check(ctx); err != nil {
				log.Printf("update check: %v", err)
			}
		}()
	})
	w.Bind("glUpdateInstall", func() {
		go func() {
			if err := a.installUpdate(); err != nil {
				log.Printf("update: %v", err)
				a.wv.Dispatch(func() { a.evalJSON("__glUpdateError", err.Error()) })
			}
		}()
	})
	w.Bind("glUpdateCancel", func() { a.updater.CancelDownload() })
	w.Bind("glOpenReleases", func() { shellOpen("open", "https://github.com/"+UpdateRepo+"/releases", "") })
	w.Bind("glCaptureTest", func() error {
		if !a.capTesting.CompareAndSwap(false, true) {
			return fmt.Errorf("a capture test is already running")
		}
		go a.runCaptureTest()
		return nil
	})
	w.Bind("glOpenURL", func(u string) {
		if strings.HasPrefix(u, "https://discord.com/") || strings.HasPrefix(u, "https://support.discord.com/") {
			shellOpen("open", u, "")
		}
	})
	w.Bind("glSetHotkeyCapture", func(on bool) {
		if on {
			a.unregisterHotkeys()
		} else {
			a.registerHotkeys()
		}
	})
}

// runCaptureTest measures how evenly Windows delivers frames from the
// selected source, with frame repeating off and nothing encoded. It gives the
// player 5 seconds to switch back to the game, then samples for 10 seconds.
func (a *App) runCaptureTest() {
	defer a.capTesting.Store(false)
	const wait, secs = 5, 10
	progress := func(phase string, left int) {
		a.wv.Dispatch(func() { a.evalJSON("__glCapTestProgress", map[string]interface{}{"phase": phase, "left": left}) })
	}
	done := func(res map[string]interface{}) {
		a.wv.Dispatch(func() { a.evalJSON("__glCapTestDone", res) })
	}
	s := a.engine.Settings()
	src, err := a.plat.ResolveCapture(s)
	if err != nil {
		done(map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	for i := wait; i > 0; i-- {
		progress("wait", i)
		time.Sleep(time.Second)
	}
	ov.Toast("info", "Capture test running", "Keep moving the camera for 10 seconds")
	progress("run", secs)
	cmd := a.plat.Command(a.ffmpeg, CaptureTestArgs(GfxSource(s, src, 500, false), secs)...)
	out, runErr := cmd.CombinedOutput()
	r := AnalyzeCaptureTimes(ParseShowinfoTimes(string(out)), s.FPS)
	if r.Frames < 3 && runErr != nil {
		lines := strings.Split(string(out), "\n")
		log.Printf("capture test failed: %v: %s", runErr, bestErrorLines(lines))
		done(map[string]interface{}{"ok": false, "error": "The capture test couldn't run. " + bestErrorLines(lines)})
		return
	}
	log.Printf("capture test (%s, target %d fps): %s", src.Kind, s.FPS, r.Summary)
	kind := "ok"
	if strings.Contains(r.Summary, "bottleneck") {
		kind = "warn"
	}
	ov.Toast(kind, "Capture test finished", fmt.Sprintf("Windows delivered %.0f fps. Details are in ReKapture.", r.FPS))
	done(map[string]interface{}{"ok": true, "report": r})
}
