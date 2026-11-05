//go:build windows

package main

import (
	"errors"
	"time"
	"unsafe"
)

var (
	pOpenClipboard         = user32.NewProc("OpenClipboard")
	pCloseClipboard        = user32.NewProc("CloseClipboard")
	pEmptyClipboard        = user32.NewProc("EmptyClipboard")
	pSetClipboardData      = user32.NewProc("SetClipboardData")
	pRegisterClipboardFmtW = user32.NewProc("RegisterClipboardFormatW")
	pGlobalAlloc           = kernel32.NewProc("GlobalAlloc")
	pGlobalLock            = kernel32.NewProc("GlobalLock")
	pGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
	pGlobalFree            = kernel32.NewProc("GlobalFree")
)

func globalBytes(b []byte) uintptr {
	h, _, _ := pGlobalAlloc.Call(0x0002 /*GMEM_MOVEABLE*/, uintptr(len(b)))
	if h == 0 {
		return 0
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(b)), b)
	pGlobalUnlock.Call(h)
	return h
}

// setClipboardFile puts a file on the clipboard the way Explorer's Copy does,
// so Ctrl+V in Discord (or Explorer) attaches/pastes the file itself.
func setClipboardFile(owner uintptr, path string) error {
	var ok uintptr
	for i := 0; i < 10; i++ { // another app may hold the clipboard for a moment
		if ok, _, _ = pOpenClipboard.Call(owner); ok != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ok == 0 {
		return errors.New("another app is using the clipboard; try again")
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h := globalBytes(DropFilesData(path))
	if h == 0 {
		return errors.New("not enough memory to copy")
	}
	if r, _, _ := pSetClipboardData.Call(15 /*CF_HDROP*/, h); r == 0 {
		pGlobalFree.Call(h)
		return errors.New("couldn't put the clip on the clipboard")
	}
	// Tell paste targets it's a copy, not a move.
	if f, _, _ := pRegisterClipboardFmtW.Call(uintptr(unsafe.Pointer(utf16("Preferred DropEffect")))); f != 0 {
		if hh := globalBytes([]byte{1, 0, 0, 0} /*DROPEFFECT_COPY*/); hh != 0 {
			if r, _, _ := pSetClipboardData.Call(f, hh); r == 0 {
				pGlobalFree.Call(hh)
			}
		}
	}
	return nil
}
