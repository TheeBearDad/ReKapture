//go:build windows

package main

// Per-app audio capture ("process loopback", Windows 10 2004+ / 11). Windows
// can hand over the sound of one program, or of everything except one
// program, before it's mixed into your speakers or headset. ReKapture uses it
// to put Discord on its own track and keep it out of the game audio.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

var (
	mmdevapi                     = windows.NewLazySystemDLL("mmdevapi.dll")
	pActivateAudioInterfaceAsync = mmdevapi.NewProc("ActivateAudioInterfaceAsync")

	iidIUnknown                  = ole.NewGUID("{00000000-0000-0000-C000-000000000046}")
	iidIAgileObject              = ole.NewGUID("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}")
	iidActivateCompletionHandler = ole.NewGUID("{41D949AB-9862-444A-80F6-C261334DA5EB}")
	errAppLoopbackUnsupported    = errors.New("this version of Windows can't capture a single app's audio (needs Windows 10 version 2004 or newer)")
	appLoopbackPath              = utf16(`VAD\Process_Loopback`)
)

const (
	processLoopbackInclude = 0 // PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE
	processLoopbackExclude = 1 // PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE
	vtBlob                 = 65
	eNoInterface           = 0x80004002
)

// AUDIOCLIENT_ACTIVATION_PARAMS (type 1 = process loopback).
type activationParams struct {
	ActivationType uint32
	TargetPID      uint32
	Mode           uint32
}

// PROPVARIANT holding a BLOB, as laid out on 64-bit Windows.
type propVariantBlob struct {
	vt         uint16
	r1, r2, r3 uint16
	size       uint32
	_          uint32
	data       *activationParams
}

// A minimal COM object implementing IActivateAudioInterfaceCompletionHandler
// (and IAgileObject, which Windows requires so it can call us from any thread).
type completionVtbl struct{ QueryInterface, AddRef, Release, ActivateCompleted uintptr }

type completionHandler struct {
	vtbl *completionVtbl // must be the first field: it's what COM sees
	done chan struct{}
	once sync.Once
}

var (
	complVtblOnce sync.Once
	complVtbl     *completionVtbl
	handlersMu    sync.Mutex
	handlers      = map[uintptr]*completionHandler{}
	// If Windows doesn't answer in time it may still use these later, so
	// they are kept alive for good (a few bytes, and it never normally happens).
	abandoned []interface{}
)

func completionVtable() *completionVtbl {
	complVtblOnce.Do(func() {
		complVtbl = &completionVtbl{
			QueryInterface: syscall.NewCallback(func(this uintptr, riid *ole.GUID, ppv *uintptr) uintptr {
				if ole.IsEqualGUID(riid, iidIUnknown) || ole.IsEqualGUID(riid, iidIAgileObject) || ole.IsEqualGUID(riid, iidActivateCompletionHandler) {
					*ppv = this
					return 0
				}
				*ppv = 0
				return eNoInterface
			}),
			AddRef:  syscall.NewCallback(func(this uintptr) uintptr { return 1 }),
			Release: syscall.NewCallback(func(this uintptr) uintptr { return 1 }),
			ActivateCompleted: syscall.NewCallback(func(this, op uintptr) uintptr {
				handlersMu.Lock()
				h := handlers[this]
				handlersMu.Unlock()
				if h != nil {
					h.once.Do(func() { close(h.done) })
				}
				return 0
			}),
		}
	})
	return complVtbl
}

// activateAppLoopback asks Windows for an audio client that captures the
// audio of process pid and its children (include), or of everything but them.
func activateAppLoopback(pid uint32, mode uint32) (*wca.IAudioClient, error) {
	if pActivateAudioInterfaceAsync.Find() != nil {
		return nil, errAppLoopbackUnsupported
	}
	h := &completionHandler{vtbl: completionVtable(), done: make(chan struct{})}
	key := uintptr(unsafe.Pointer(h))
	handlersMu.Lock()
	handlers[key] = h
	handlersMu.Unlock()
	finished := false
	defer func() {
		// Runs after op.Release below (defers run last-in first-out), so
		// Windows is done with the handler before it's dropped.
		if finished {
			handlersMu.Lock()
			delete(handlers, key)
			handlersMu.Unlock()
		}
	}()
	params := &activationParams{ActivationType: 1, TargetPID: pid, Mode: mode}
	pv := &propVariantBlob{vt: vtBlob, size: uint32(unsafe.Sizeof(*params)), data: params}

	var op *ole.IUnknown // IActivateAudioInterfaceAsyncOperation
	hr, _, _ := pActivateAudioInterfaceAsync.Call(
		uintptr(unsafe.Pointer(appLoopbackPath)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		uintptr(unsafe.Pointer(pv)),
		uintptr(unsafe.Pointer(h)),
		uintptr(unsafe.Pointer(&op)))
	if hr != 0 {
		finished = true
		return nil, fmt.Errorf("couldn't start app audio capture (0x%08X)", uint32(hr))
	}
	select {
	case <-h.done:
		finished = true
		defer op.Release()
	case <-time.After(5 * time.Second):
		handlersMu.Lock()
		abandoned = append(abandoned, h, params, pv, op)
		handlersMu.Unlock()
		return nil, errors.New("Windows didn't answer the app audio request")
	}
	// IActivateAudioInterfaceAsyncOperation::GetActivateResult is method 3.
	vt := (*[4]uintptr)(unsafe.Pointer(op.RawVTable))
	var actHR uint32
	var unk *ole.IUnknown
	r, _, _ := syscall.SyscallN(vt[3], uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(&actHR)), uintptr(unsafe.Pointer(&unk)))
	runtime.KeepAlive(params)
	runtime.KeepAlive(pv)
	runtime.KeepAlive(h)
	if r != 0 {
		return nil, fmt.Errorf("app audio capture failed (0x%08X)", uint32(r))
	}
	if actHR != 0 || unk == nil {
		if unk != nil {
			unk.Release()
		}
		return nil, fmt.Errorf("%w (0x%08X)", errAppLoopbackUnsupported, actHR)
	}
	defer unk.Release()
	var client *wca.IAudioClient
	if r, _, _ := syscall.SyscallN(unk.VTable().QueryInterface, uintptr(unsafe.Pointer(unk)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)), uintptr(unsafe.Pointer(&client))); r != 0 || client == nil {
		return nil, fmt.Errorf("app audio capture failed (0x%08X)", uint32(r))
	}
	return client, nil
}

// Capture formats to try. App capture doesn't report a mix format and
// doesn't accept the auto-convert flags, so ask for 48 kHz stereo float
// directly (Windows converts), with plainer fallbacks.
type loopbackFormat struct {
	raw  [10]uint32 // 40 bytes, 4-byte aligned
	tag  int
	bits int
}

func appLoopbackFormats() []*loopbackFormat {
	mk := func(tag, bits int, extensible bool) *loopbackFormat {
		f := &loopbackFormat{tag: tag, bits: bits}
		b := (*[40]byte)(unsafe.Pointer(&f.raw))
		block := 2 * bits / 8
		wtag := tag
		if extensible {
			wtag = waveExt
		}
		binary.LittleEndian.PutUint16(b[0:], uint16(wtag))
		binary.LittleEndian.PutUint16(b[2:], 2)
		binary.LittleEndian.PutUint32(b[4:], outRate)
		binary.LittleEndian.PutUint32(b[8:], uint32(outRate*block))
		binary.LittleEndian.PutUint16(b[12:], uint16(block))
		binary.LittleEndian.PutUint16(b[14:], uint16(bits))
		if extensible {
			binary.LittleEndian.PutUint16(b[16:], 22)           // cbSize
			binary.LittleEndian.PutUint16(b[18:], uint16(bits)) // valid bits
			binary.LittleEndian.PutUint32(b[20:], 3)            // front left + right
			// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT / _PCM: {0000000X-0000-0010-8000-00AA00389B71}
			binary.LittleEndian.PutUint32(b[24:], uint32(tag))
			binary.LittleEndian.PutUint16(b[28:], 0)
			binary.LittleEndian.PutUint16(b[30:], 0x10)
			copy(b[32:], []byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
		}
		return f
	}
	return []*loopbackFormat{mk(waveFloat, 32, true), mk(waveFloat, 32, false), mk(wavePCM, 16, false)}
}

// openAppCapture opens app audio capture for pid (include or exclude mode).
func openAppCapture(pid uint32, mode uint32) (*captureDevice, error) {
	var lastErr error
	type attempt struct {
		f     *loopbackFormat
		event bool
	}
	var attempts []attempt
	for _, f := range appLoopbackFormats() {
		// Event mode is what Microsoft's own sample uses; plain polling is
		// the fallback. ReKapture polls every 10 ms either way.
		attempts = append(attempts, attempt{f, true}, attempt{f, false})
	}
	for _, at := range attempts {
		f := at.f
		client, err := activateAppLoopback(pid, mode)
		if err != nil {
			return nil, err
		}
		d := &captureDevice{client: client, rate: outRate, channels: 2, bits: f.bits, tag: f.tag}
		flags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK)
		if at.event {
			flags |= wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK
		}
		err = client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags,
			2000000 /* 200 ms */, 0, (*wca.WAVEFORMATEX)(unsafe.Pointer(&f.raw)), nil)
		runtime.KeepAlive(f)
		if err == nil && at.event {
			ev, e2 := windows.CreateEvent(nil, 0, 0, nil)
			if e2 == nil {
				d.event = ev
				err = client.SetEventHandle(uintptr(ev))
			} else {
				err = e2
			}
		}
		if err != nil {
			lastErr = err
			d.release()
			continue
		}
		d.convert = !(f.tag == waveFloat && f.bits == 32)
		if err := client.GetService(wca.IID_IAudioCaptureClient, &d.capture); err != nil {
			d.release()
			return nil, fmt.Errorf("couldn't start app audio capture: %v", err)
		}
		if err := client.Start(); err != nil {
			d.release()
			return nil, fmt.Errorf("couldn't start app audio capture: %v", err)
		}
		return d, nil
	}
	return nil, fmt.Errorf("app audio capture refused every format: %v", lastErr)
}

// ---------- finding Discord ----------

var discordCache struct {
	sync.Mutex
	at  time.Time
	pid uint32
}

// currentDiscordPID returns the main Discord process, or 0. Checked at most
// once a second (a process snapshot takes about a millisecond).
func currentDiscordPID() uint32 {
	discordCache.Lock()
	defer discordCache.Unlock()
	if time.Since(discordCache.at) < time.Second {
		return discordCache.pid
	}
	discordCache.at = time.Now()
	discordCache.pid = DiscordRoot(listProcesses())
	return discordCache.pid
}

func listProcesses() []Proc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []Proc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out = append(out, Proc{PID: e.ProcessID, PPID: e.ParentProcessID, Exe: windows.UTF16ToString(e.ExeFile[:])})
	}
	return out
}

// appLoopbackState remembers if app capture failed on this PC, so ReKapture
// says so once and falls back instead of retrying every second.
var appLoopbackState struct {
	sync.Mutex
	broken bool
	told   bool
}

func appLoopbackBroken() bool {
	appLoopbackState.Lock()
	defer appLoopbackState.Unlock()
	return appLoopbackState.broken
}

func markAppLoopbackBroken(err error, notify func(kind, title, msg string)) {
	appLoopbackState.Lock()
	first := !appLoopbackState.told
	appLoopbackState.broken, appLoopbackState.told = true, true
	appLoopbackState.Unlock()
	log.Printf("audio: separate Discord audio isn't available: %v", err)
	if first && notify != nil {
		msg := "Discord stays in the game audio"
		if strings.Contains(err.Error(), "version") {
			msg = "This needs Windows 10 version 2004 or newer. Discord stays in the game audio"
		}
		notify("warn", "Couldn't separate Discord audio", msg)
	}
}
