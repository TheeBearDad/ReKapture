//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

const (
	outRate     = 48000
	outChannels = 2
	waveFloat   = 3
	wavePCM     = 1
	waveExt     = 0xFFFE
)

// wasapiSource captures audio and streams it to ffmpeg over a loopback
// socket as f32le / 48 kHz / stereo. Kinds:
//
//	loopback            everything an output device plays
//	loopback-nodiscord  everything except Discord (falls back to loopback)
//	discord             only Discord (silent while Discord is closed)
//	mic                 an input device
type wasapiSource struct {
	kind     string
	deviceID string
	notify   func(kind, title, msg string)
	path     string
	ln       net.Listener
	stop     chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

func newWasapiSource(kind, id string, notify func(kind, title, msg string)) *wasapiSource {
	return &wasapiSource{kind: kind, deviceID: id, notify: notify}
}

// errIdle: nothing to capture right now (Discord is closed); keep the track
// alive with silence and check again shortly.
var errIdle = errors.New("idle")

// open starts capture for this source. watch, if set, reports when the
// capture should be reopened (Discord started or closed).
func (w *wasapiSource) open() (dev *captureDevice, watch func() bool, desc string, err error) {
	switch w.kind {
	case "discord":
		pid := currentDiscordPID()
		watch = func() bool { return currentDiscordPID() != pid }
		if pid == 0 || appLoopbackBroken() {
			if appLoopbackBroken() {
				watch = nil
			}
			return nil, watch, "", errIdle
		}
		dev, err = openAppCapture(pid, processLoopbackInclude)
		if err != nil {
			markAppLoopbackBroken(err, w.notify)
			return nil, nil, "", errIdle
		}
		return dev, watch, fmt.Sprintf("Discord only (process %d)", pid), nil
	case "loopback-nodiscord":
		pid := currentDiscordPID()
		if pid != 0 && !appLoopbackBroken() {
			dev, err = openAppCapture(pid, processLoopbackExclude)
			if err == nil {
				return dev, func() bool { return currentDiscordPID() != pid }, fmt.Sprintf("all apps except Discord (process %d)", pid), nil
			}
			markAppLoopbackBroken(err, w.notify)
		}
		dev, err = openCapture("loopback", w.deviceID)
		if err != nil {
			return nil, nil, "", err
		}
		if !appLoopbackBroken() {
			// Discord isn't running: capture the device, and switch to
			// "everything except Discord" as soon as it starts.
			watch = func() bool { return currentDiscordPID() != 0 }
		}
		return dev, watch, "output device (Discord not running)", nil
	}
	dev, err = openCapture(w.kind, w.deviceID)
	return dev, nil, "", err
}

func (w *wasapiSource) Open(t0 time.Time) (string, error) {
	// A loopback-only TCP socket carries the audio to ffmpeg. (Named pipes
	// fail inside ffmpeg's Windows file layer, which reports "Invalid
	// argument".) It's bound to 127.0.0.1, so nothing leaves the PC.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("couldn't open the audio connection: %v", err)
	}
	w.ln = ln
	w.path = fmt.Sprintf("tcp://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	w.stop = make(chan struct{})
	ch := make(chan []byte, 1024) // ~10 s of 10 ms chunks while ffmpeg starts up

	w.wg.Add(2)
	go func() { // writer: hands chunks to ffmpeg once it connects
		defer w.wg.Done()
		conn, err := ln.Accept()
		ln.Close() // one connection only
		if err != nil {
			return
		}
		defer conn.Close()
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			tc.SetWriteBuffer(1 << 20)
		}
		for {
			select {
			case <-w.stop:
				return
			case b := <-ch:
				if _, err := conn.Write(b); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer w.wg.Done()
		w.captureLoop(t0, ch)
	}()
	return w.path, nil
}

func (w *wasapiSource) Close() {
	w.once.Do(func() {
		if w.stop != nil {
			close(w.stop)
		}
		if w.ln != nil {
			w.ln.Close()
		}
	})
	w.wg.Wait()
}

func (w *wasapiSource) stopped() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// captureLoop keeps a device open (reopening it if it's unplugged or the
// default device changes) and always produces a continuous, clock-aligned
// stream — silence is filled in whenever the device gives nothing.
func (w *wasapiSource) captureLoop(t0 time.Time, ch chan []byte) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// S_FALSE (already initialized) is fine
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 {
			log.Printf("audio %s: CoInitializeEx: %v", w.kind, err)
		}
	}
	defer ole.CoUninitialize()

	al := NewAligner(outRate, outChannels)
	emit := func(captured []byte) {
		el := time.Since(t0)
		if el < 0 {
			return // before the shared start instant: discard
		}
		out := al.Process(captured, int64(el.Seconds()*outRate))
		if len(out) == 0 {
			return
		}
		select {
		case ch <- out:
		default:
			// ffmpeg isn't reading (still starting or stalled); the aligner will
			// re-pad the gap so timing stays correct.
		}
	}

	lastErr := ""
	idleLogged := false
	for !w.stopped() {
		dev, watch, desc, err := w.open()
		if err == errIdle {
			if !idleLogged {
				log.Printf("audio %s: Discord isn't running or can't be captured; the track stays silent until it can", w.kind)
				idleLogged = true
			}
			for i := 0; i < 100 && !w.stopped(); i++ { // silence for a second, then look again
				time.Sleep(10 * time.Millisecond)
				emit(nil)
				if i%25 == 24 && watch != nil && watch() {
					break
				}
			}
			continue
		}
		idleLogged = false
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("audio %s: %v (will retry, recording silence)", w.kind, err)
				lastErr = err.Error()
			}
			for i := 0; i < 100 && !w.stopped(); i++ { // retry every second, keep the track alive
				time.Sleep(10 * time.Millisecond)
				emit(nil)
			}
			continue
		}
		lastErr = ""
		if desc != "" {
			desc = ", " + desc
		}
		log.Printf("audio %s: capturing (%d Hz, %d ch, tag %d, %d bit%s)", w.kind, dev.rate, dev.channels, dev.tag, dev.bits, desc)
		err = dev.run(w.stop, emit, watch)
		dev.release()
		if err == errRetarget {
			log.Printf("audio %s: Discord started or closed, switching capture", w.kind)
			continue
		}
		if err != nil && !w.stopped() {
			log.Printf("audio %s: device lost: %v — reopening", w.kind, err)
			time.Sleep(200 * time.Millisecond)
		}
	}
}

type captureDevice struct {
	enum     *wca.IMMDeviceEnumerator
	dev      *wca.IMMDevice
	client   *wca.IAudioClient
	capture  *wca.IAudioCaptureClient
	rate     int
	channels int
	bits     int
	tag      int // waveFloat / wavePCM (after resolving EXTENSIBLE)
	convert  bool
	resamp   *resampler
	event    windows.Handle // app capture in event mode (signalled, not waited on)
}

func openCapture(kind, id string) (*captureDevice, error) {
	d := &captureDevice{}
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &d.enum); err != nil {
		return nil, fmt.Errorf("audio system unavailable: %v", err)
	}
	flow := uint32(wca.ERender)
	if kind == "mic" {
		flow = uint32(wca.ECapture)
	}
	var err error
	if id != "" {
		d.dev, err = getDeviceByID(d.enum, id)
		if err != nil {
			// The chosen device is gone — fall back to the default one.
			d.dev = nil
		}
	}
	if d.dev == nil {
		if err := d.enum.GetDefaultAudioEndpoint(flow, wca.EConsole, &d.dev); err != nil {
			d.release()
			if kind == "mic" {
				return nil, fmt.Errorf("no microphone found")
			}
			return nil, fmt.Errorf("no audio output device found")
		}
	}
	if err := activateAudioClient(d.dev, &d.client); err != nil {
		d.release()
		return nil, fmt.Errorf("couldn't open device: %v", err)
	}

	flags := uint32(0)
	if kind == "loopback" {
		flags |= wca.AUDCLNT_STREAMFLAGS_LOOPBACK
	}
	// Ask Windows to convert to exactly what we want (works on Windows 10+).
	want := wca.WAVEFORMATEX{WFormatTag: waveFloat, NChannels: outChannels, NSamplesPerSec: outRate,
		WBitsPerSample: 32, NBlockAlign: 4 * outChannels, NAvgBytesPerSec: outRate * 4 * outChannels}
	err = d.client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED,
		flags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM|wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
		2000000 /* 200 ms */, 0, &want, nil)
	if err == nil {
		d.rate, d.channels, d.bits, d.tag = outRate, outChannels, 32, waveFloat
	} else {
		// Fallback: use the device's own mix format and convert ourselves.
		d.client.Release()
		d.client = nil
		if err := activateAudioClient(d.dev, &d.client); err != nil {
			d.release()
			return nil, fmt.Errorf("couldn't open device: %v", err)
		}
		var mix *wca.WAVEFORMATEX
		if err := d.client.GetMixFormat(&mix); err != nil {
			d.release()
			return nil, fmt.Errorf("couldn't read device format: %v", err)
		}
		d.rate, d.channels, d.bits = int(mix.NSamplesPerSec), int(mix.NChannels), int(mix.WBitsPerSample)
		d.tag = int(mix.WFormatTag)
		if d.tag == waveExt && mix.CbSize >= 22 {
			// WAVEFORMATEXTENSIBLE: the sub-format GUID's first 2 bytes are the real tag.
			ext := (*[40]byte)(unsafe.Pointer(mix))
			d.tag = int(binary.LittleEndian.Uint16(ext[24:26]))
		}
		err = d.client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, 2000000, 0, mix, nil)
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
		if err != nil {
			d.release()
			return nil, fmt.Errorf("couldn't start device: %v", err)
		}
		if !(d.tag == waveFloat && d.bits == 32) && !(d.tag == wavePCM && (d.bits == 16 || d.bits == 24 || d.bits == 32)) {
			d.release()
			return nil, fmt.Errorf("unsupported device format (tag %d, %d bit)", d.tag, d.bits)
		}
		d.convert = true
		if d.rate != outRate {
			d.resamp = &resampler{ratio: float64(d.rate) / outRate}
		}
	}
	if err := d.client.GetService(wca.IID_IAudioCaptureClient, &d.capture); err != nil {
		d.release()
		return nil, fmt.Errorf("couldn't start capture: %v", err)
	}
	if err := d.client.Start(); err != nil {
		d.release()
		return nil, fmt.Errorf("couldn't start capture: %v", err)
	}
	return d, nil
}

var errRetarget = errors.New("retarget")

func (d *captureDevice) run(stop chan struct{}, emit func([]byte), watch func() bool) error {
	blockAlign := d.channels * d.bits / 8
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
		if watch != nil && n%100 == 0 && watch() {
			return errRetarget
		}
		var chunk []byte
		for {
			var next uint32
			if err := d.capture.GetNextPacketSize(&next); err != nil {
				return err
			}
			if next == 0 {
				break
			}
			var data *byte
			var frames, flags uint32
			var devPos, qpcPos uint64
			if err := d.capture.GetBuffer(&data, &frames, &flags, &devPos, &qpcPos); err != nil {
				return err
			}
			n := int(frames) * blockAlign
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				chunk = append(chunk, make([]byte, n)...)
			} else {
				chunk = append(chunk, unsafe.Slice(data, n)...)
			}
			if err := d.capture.ReleaseBuffer(frames); err != nil {
				return err
			}
		}
		if d.convert && len(chunk) > 0 {
			chunk = d.toStereoFloat(chunk)
		}
		emit(chunk)
	}
}

// toStereoFloat converts the device's native format to f32 stereo 48 kHz.
func (d *captureDevice) toStereoFloat(in []byte) []byte {
	bps := d.bits / 8
	frames := len(in) / (bps * d.channels)
	l := make([]float32, frames)
	r := make([]float32, frames)
	sample := func(off int) float32 {
		switch {
		case d.tag == waveFloat:
			return math.Float32frombits(binary.LittleEndian.Uint32(in[off:]))
		case d.bits == 16:
			return float32(int16(binary.LittleEndian.Uint16(in[off:]))) / 32768
		case d.bits == 24:
			v := int32(in[off]) | int32(in[off+1])<<8 | int32(int8(in[off+2]))<<16
			return float32(v) / 8388608
		default:
			return float32(int32(binary.LittleEndian.Uint32(in[off:]))) / 2147483648
		}
	}
	for i := 0; i < frames; i++ {
		base := i * bps * d.channels
		a := sample(base)
		b := a
		if d.channels > 1 {
			b = sample(base + bps)
		}
		l[i], r[i] = a, b
	}
	if d.resamp != nil {
		l, r = d.resamp.process(l, r)
	}
	out := make([]byte, len(l)*8)
	for i := range l {
		binary.LittleEndian.PutUint32(out[i*8:], math.Float32bits(l[i]))
		binary.LittleEndian.PutUint32(out[i*8+4:], math.Float32bits(r[i]))
	}
	return out
}

func (d *captureDevice) release() {
	if d.client != nil {
		d.client.Stop()
	}
	if d.capture != nil {
		d.capture.Release()
		d.capture = nil
	}
	if d.client != nil {
		d.client.Release()
		d.client = nil
	}
	if d.dev != nil {
		d.dev.Release()
		d.dev = nil
	}
	if d.enum != nil {
		d.enum.Release()
		d.enum = nil
	}
	if d.event != 0 {
		windows.CloseHandle(d.event)
		d.event = 0
	}
}

// resampler is a simple linear resampler, only used on the rare devices
// where Windows can't convert for us.
type resampler struct {
	ratio float64 // in rate / out rate
	pos   float64
	lastL float32
	lastR float32
}

func (r *resampler) process(l, rr []float32) ([]float32, []float32) {
	var ol, or []float32
	n := float64(len(l))
	for r.pos < n {
		i := int(r.pos)
		f := float32(r.pos - float64(i))
		var l0, r0 float32
		if i == 0 {
			l0, r0 = r.lastL, r.lastR
		} else {
			l0, r0 = l[i-1], rr[i-1]
		}
		ol = append(ol, l0+(l[i]-l0)*f)
		or = append(or, r0+(rr[i]-r0)*f)
		r.pos += r.ratio
	}
	r.pos -= n
	if len(l) > 0 {
		r.lastL, r.lastR = l[len(l)-1], rr[len(rr)-1]
	}
	return ol, or
}

// activateAudioClient calls IMMDevice::Activate with correct argument types.
func activateAudioClient(dev *wca.IMMDevice, out **wca.IAudioClient) error {
	hr, _, _ := syscall.SyscallN(dev.VTable().Activate,
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		uintptr(wca.CLSCTX_ALL),
		0,
		uintptr(unsafe.Pointer(out)))
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

// getDeviceByID calls IMMDeviceEnumerator::GetDevice (not wrapped by go-wca).
func getDeviceByID(enum *wca.IMMDeviceEnumerator, id string) (*wca.IMMDevice, error) {
	p, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	var dev *wca.IMMDevice
	hr, _, _ := syscall.SyscallN(enum.VTable().GetDevice,
		uintptr(unsafe.Pointer(enum)), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&dev)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	return dev, nil
}

type AudioDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListAudioDevices returns active outputs (for game audio) and inputs (mics).
func ListAudioDevices() (outputs, inputs []AudioDevice) {
	type result struct{ outs, ins []AudioDevice }
	done := make(chan result, 1)
	go func() {
		var outputs, inputs []AudioDevice
		defer func() { done <- result{outputs, inputs} }()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		defer ole.CoUninitialize()
		var enum *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enum); err != nil {
			return
		}
		defer enum.Release()
		list := func(flow uint32) []AudioDevice {
			var out []AudioDevice
			var col *wca.IMMDeviceCollection
			if err := enum.EnumAudioEndpoints(flow, wca.DEVICE_STATE_ACTIVE, &col); err != nil {
				return nil
			}
			defer col.Release()
			var count uint32
			col.GetCount(&count)
			for i := uint32(0); i < count; i++ {
				var dev *wca.IMMDevice
				if err := col.Item(i, &dev); err != nil {
					continue
				}
				var id string
				dev.GetId(&id)
				name := id
				var ps *wca.IPropertyStore
				if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err == nil {
					var pv wca.PROPVARIANT
					if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err == nil && pv.Val != 0 {
						name = pv.String()
					}
					ps.Release()
				}
				dev.Release()
				out = append(out, AudioDevice{ID: id, Name: name})
			}
			return out
		}
		outputs = list(uint32(wca.ERender))
		inputs = list(uint32(wca.ECapture))
	}()
	select {
	case r := <-done:
		return r.outs, r.ins
	case <-time.After(5 * time.Second):
		return nil, nil
	}
}
