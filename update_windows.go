//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// setupUpdater creates the updater and starts the background checks.
func (a *App) setupUpdater() {
	dir := filepath.Join(localDataDir(), "updates")
	a.updater = &Updater{Current: appVersion, Portable: portable(), Dir: dir}
	a.updater.CleanDownloads() // anything left there is from before this version
	if portable() {
		RemoveOldFiles(exeDir()) // the previous version's files, moved aside during the update
	}
	toldAbout := ""
	a.updater.OnChange = func(st UpdateState) {
		a.wv.Dispatch(func() { a.evalJSON("__glUpdate", st) })
		if st.Phase == "available" && st.Latest != toldAbout {
			toldAbout = st.Latest
			log.Printf("update available: %s", st.Latest)
			ov.Toast("info", "ReKapture "+st.Latest+" is available", "Open ReKapture and go to About to install it")
		}
	}
	go func() {
		time.Sleep(20 * time.Second) // let startup and capture settle first
		for {
			if a.settings.AutoUpdate {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if _, err := a.updater.Check(ctx); err != nil {
					log.Printf("update check: %v", err)
				}
				cancel()
			}
			time.Sleep(6 * time.Hour)
		}
	}()
}

// noteVersionChange says "Updated to X" once after an update.
func (a *App) noteVersionChange() {
	last := a.settings.LastVersion
	if last == appVersion {
		return
	}
	a.settings.LastVersion = appVersion
	SaveSettings(a.setPath, a.settings)
	if last != "" && NewerVersion(appVersion, last) {
		log.Printf("updated from %s to %s", last, appVersion)
		go func() {
			time.Sleep(2 * time.Second)
			ov.Toast("ok", "ReKapture updated to "+appVersion, "")
		}()
	}
}

// installUpdate downloads the update, then hands over to the installer (or
// swaps the portable files) and closes ReKapture. It runs off the UI thread.
func (a *App) installUpdate() error {
	if a.engine.Recording() || a.engine.Status().Finalizing > 0 {
		return fmt.Errorf("finish your recording first, then install the update")
	}
	path, err := a.updater.Download()
	if err != nil {
		return err
	}
	a.updater.set(func(s *UpdateState) { s.Phase = "installing" })
	if portable() {
		a.engine.Shutdown() // ffmpeg.exe is about to be replaced
		if err := ApplyPortableUpdate(path, exeDir()); err != nil {
			a.updater.set(func(s *UpdateState) { s.Phase, s.Error = "error", err.Error() })
			log.Printf("portable update failed: %v", err)
			// The old files are still in place; start fresh so capture comes back.
			relaunch()
			a.quit()
			return err
		}
		log.Printf("portable update applied, restarting")
		if err := relaunch(); err != nil {
			return err
		}
		a.quit()
		return nil
	}
	// Installed copy: run the installer silently in this folder. It waits for
	// ReKapture to close, installs, and starts it again.
	params := fmt.Sprintf(`/S /UPDATE /D=%s`, exeDir())
	if err := runElevated(path, params); err != nil {
		a.updater.set(func(s *UpdateState) { s.Phase = "ready" })
		return err
	}
	log.Printf("installer started for %s, closing", a.updater.State().Latest)
	a.quit()
	return nil
}

// relaunch starts ReKapture again; the new copy waits for this one to close.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--updated")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 /* DETACHED_PROCESS */}
	return cmd.Start()
}

// runElevated starts a program with administrator rights (Windows asks the
// user once). It fails if the user says no.
func runElevated(file, params string) error {
	r, _, _ := pShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16("runas"))), uintptr(unsafe.Pointer(utf16(file))),
		uintptr(unsafe.Pointer(utf16(params))), 0, 1)
	if r <= 32 {
		if r == 5 { // SE_ERR_ACCESSDENIED: the user said no to the Windows prompt
			return fmt.Errorf("the update was cancelled. Click Install again when you're ready")
		}
		return fmt.Errorf("couldn't start the installer (error %d)", r)
	}
	return nil
}
