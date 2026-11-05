//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type winPlatform struct {
	notify func(kind, title, msg string)
}

func (p *winPlatform) ResolveCapture(s Settings) (CaptureSource, error) {
	if s.CaptureMode == "window" {
		want := strings.ToLower(strings.TrimSpace(s.WindowExe))
		for _, w := range ListWindows() {
			if strings.ToLower(w.Exe) == want {
				return CaptureSource{Kind: "window", Handle: uintptr(w.hwnd), Width: w.w &^ 1, Height: w.h &^ 1,
					Label: fmt.Sprintf("%s (%s)", w.Title, w.Exe)}, nil
			}
		}
		return CaptureSource{}, fmt.Errorf("Waiting for %s to open…", s.WindowExe)
	}
	mons := ListMonitors()
	if len(mons) == 0 {
		return CaptureSource{}, fmt.Errorf("no monitors found")
	}
	pick := mons[0]
	for _, m := range mons {
		if m.Primary {
			pick = m
		}
	}
	for _, m := range mons {
		if s.MonitorID != "" && strings.EqualFold(m.ID, s.MonitorID) {
			pick = m
		}
	}
	return CaptureSource{Kind: "monitor", Handle: pick.handle, Width: pick.Width, Height: pick.Height, Label: pick.Name}, nil
}

func (p *winPlatform) NewAudio(kind, id string) AudioSource {
	return newWasapiSource(kind, id, p.notify)
}

func (p *winPlatform) Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 /* CREATE_NO_WINDOW */}
	return cmd
}

func (p *winPlatform) ForegroundName() string { return CleanTitle(ForegroundTitle()) }

func (p *winPlatform) Notify(kind, title, msg string) {
	if p.notify != nil {
		p.notify(kind, title, msg)
	}
}

func (p *winPlatform) HideDir(path string) {
	if ptr, err := windows.UTF16PtrFromString(path); err == nil {
		attrs, err := windows.GetFileAttributes(ptr)
		if err == nil {
			windows.SetFileAttributes(ptr, attrs|windows.FILE_ATTRIBUTE_HIDDEN)
		}
	}
}

// A job object that kills ffmpeg if ReKapture exits or crashes, so an
// encoder is never left running in the background. Only ffmpeg goes in it —
// apps the user opens from ReKapture (player, Explorer) are not affected.
var killJob windows.Handle

func initKillJob() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return
	}
	killJob = job
}

func (p *winPlatform) AfterStart(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	if killJob != 0 {
		_ = windows.AssignProcessToJobObject(killJob, h)
	}
	boostCapturePriority(h)
}

var pD3DKMTSetProcessSchedulingPriorityClass = gdi32.NewProc("D3DKMTSetProcessSchedulingPriorityClass")

// boostCapturePriority lets the capture/encode process cut ahead of the game
// on the GPU and CPU. When a game keeps the GPU at ~100%, capture work at
// normal priority runs late, Windows drops frames, and the recording
// stutters. (OBS raises its GPU priority for the same reason.)
func boostCapturePriority(h windows.Handle) {
	cpu := "normal"
	if windows.SetPriorityClass(h, windows.ABOVE_NORMAL_PRIORITY_CLASS) == nil {
		cpu = "above normal"
	}
	gpu := "normal (not allowed)"
	if pD3DKMTSetProcessSchedulingPriorityClass.Find() == nil {
		// D3DKMT_SCHEDULINGPRIORITYCLASS: 3 = above normal, 4 = high, 5 = realtime (admin only)
		for _, c := range []struct {
			v    uintptr
			name string
		}{{5, "realtime"}, {4, "high"}, {3, "above normal"}} {
			if st, _, _ := pD3DKMTSetProcessSchedulingPriorityClass.Call(uintptr(h), c.v); st == 0 {
				gpu = c.name
				break
			}
		}
	}
	log.Printf("capture priority: GPU %s, CPU %s", gpu, cpu)
}

var portableData = sync.OnceValue(func() string { return portableDataIn(exeDir()) })

func portable() bool { return portableData() != "" }

func appDataDir() string {
	if p := portableData(); p != "" {
		return p
	}
	d, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "ReKapture")
}

func localDataDir() string {
	if p := portableData(); p != "" {
		return filepath.Join(p, "Cache")
	}
	d := os.Getenv("LOCALAPPDATA")
	if d == "" {
		d = appDataDir()
	}
	return filepath.Join(d, "ReKapture")
}

func videosDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Videos, 0); err == nil && p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Videos")
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func findFFmpeg() string {
	d := exeDir()
	for _, c := range []string{filepath.Join(d, "ffmpeg", "ffmpeg.exe"), filepath.Join(d, "ffmpeg.exe")} {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func setStartWithWindows(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for _, n := range previousNames {
		k.DeleteValue(n) // entries from before the renames
	}
	if !on {
		err := k.DeleteValue("ReKapture")
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue("ReKapture", fmt.Sprintf(`"%s" --tray`, exe))
}

// Names this app had before, newest first.
var previousNames = []string{"Kapture", "InstaReplay", "GreenLight"}

// migrateOldNames carries settings over from the app's earlier names and
// moves the "Start with Windows" entry to the new name.
func migrateOldNames(newSettings string) {
	if portable() {
		return // a portable copy never takes over an installed copy's settings
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return
	}
	if !fileExists(newSettings) {
		for _, n := range previousNames {
			old := filepath.Join(d, n, "settings.json")
			if b, err := os.ReadFile(old); err == nil {
				os.MkdirAll(filepath.Dir(newSettings), 0o755)
				if os.WriteFile(newSettings, b, 0o644) == nil {
					log.Printf("migrated settings from %s", old)
				}
				break
			}
		}
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE); err == nil {
		had := false
		for _, n := range previousNames {
			if _, _, err := k.GetStringValue(n); err == nil {
				had = true
			}
		}
		k.Close()
		if had {
			setStartWithWindows(true) // writes the new entry and removes the old ones
		}
	}
}

// migrateClipsFolder renames an old default clips folder (Videos\Kapture,
// Videos\InstaReplay or Videos\GreenLight) to Videos\ReKapture. A folder the user picked is left
// alone. Returns the folder to use.
func migrateClipsFolder(current string) string {
	newDir := filepath.Join(videosDir(), "ReKapture")
	for _, n := range previousNames {
		oldDefault := filepath.Join(videosDir(), n)
		if !strings.EqualFold(filepath.Clean(current), filepath.Clean(oldDefault)) {
			continue
		}
		if !fileExists(oldDefault) {
			return newDir
		}
		if fileExists(newDir) {
			return current // both exist: don't merge automatically
		}
		if err := os.Rename(oldDefault, newDir); err != nil {
			log.Printf("couldn't rename %s to %s: %v (keeping the old folder)", oldDefault, newDir, err)
			return current
		}
		log.Printf("renamed clips folder %s -> %s", oldDefault, newDir)
		return newDir
	}
	return current
}

var (
	pGetWindowRect = user32.NewProc("GetWindowRect")
	pIsZoomed      = user32.NewProc("IsZoomed")
)

// ForegroundApp describes the window in front, for game detection. It only
// reads the window's title, size and the program's path (the access Task
// Manager uses), never the game's memory.
func (p *winPlatform) ForegroundApp() ForegroundApp {
	h := windows.GetForegroundWindow()
	if h == 0 {
		return ForegroundApp{Shell: true}
	}
	var pid uint32
	windows.GetWindowThreadProcessId(h, &pid)
	app := ForegroundApp{PID: pid, Title: windowText(h)}
	app.Path = processPath(pid)
	app.Exe = filepath.Base(app.Path)
	if app.Path == "" {
		app.Exe = ""
	}
	cls := className(h)
	if pid == uint32(windows.GetCurrentProcessId()) || cls == "Progman" || cls == "WorkerW" ||
		cls == "Shell_TrayWnd" || cls == "Shell_SecondaryTrayWnd" || strings.EqualFold(app.Exe, "explorer.exe") {
		app.Shell = true
		return app
	}
	// Full screen: the window covers its whole monitor and has no title bar
	// (borderless or exclusive full screen). Maximized windows don't count.
	hmon, _, _ := pMonitorFromWindow.Call(uintptr(h), 2 /* MONITOR_DEFAULTTONEAREST */)
	var mi [104]byte // MONITORINFOEXW
	binary.LittleEndian.PutUint32(mi[0:], 104)
	if r, _, _ := pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi[0]))); r != 0 {
		app.MonitorID = windows.UTF16ToString((*[32]uint16)(unsafe.Pointer(&mi[40]))[:])
		ml, mt := int32(binary.LittleEndian.Uint32(mi[4:])), int32(binary.LittleEndian.Uint32(mi[8:]))
		mr, mb := int32(binary.LittleEndian.Uint32(mi[12:])), int32(binary.LittleEndian.Uint32(mi[16:]))
		var wr rect
		pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&wr)))
		style, _, _ := pGetWindowLongPtrW.Call(uintptr(h), ^uintptr(15) /* GWL_STYLE = -16 */)
		zoomed, _, _ := pIsZoomed.Call(uintptr(h))
		covers := wr.Left <= ml && wr.Top <= mt && wr.Right >= mr && wr.Bottom >= mb
		app.Fullscreen = covers && style&0x00C00000 != 0x00C00000 /* WS_CAPTION */ && zoomed == 0
	}
	return app
}

// ProcessAlive reports whether a process is still running. If Windows
// won't say (a protected game), it's assumed to still run.
func (p *winPlatform) ProcessAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// processPath is the full path of a process's program, or "".
func processPath(pid uint32) string {
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(ph)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(ph, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
