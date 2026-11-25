//go:build windows

package main

import (
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	xinput          = windows.NewLazySystemDLL("xinput1_4.dll")
	xinputOld       = windows.NewLazySystemDLL("xinput9_1_0.dll")
	pXInputGetState *windows.LazyProc

	setupapi                          = windows.NewLazySystemDLL("setupapi.dll")
	pSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	pSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	pSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	pSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	hidGUID  = windows.GUID{Data1: 0x4D1E55B2, Data2: 0xF16F, Data3: 0x11CF, Data4: [8]byte{0x88, 0xCB, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30}}
	psPathRe = regexp.MustCompile(`vid[_&](?:0002)?054c[_&]pid[_&]([0-9a-f]{4})`)
)

type xinputState struct {
	Packet  uint32
	Buttons uint16
	LT, RT  uint8
	LX, LY  int16
	RX, RY  int16
}

// padManager reads controllers and turns button combos into actions.
type padManager struct {
	onAction func(action string)
	onLive   func(family string, buttons []string, done bool) // while recording a shortcut
	onDevs   func(names []string)

	mu       sync.Mutex
	enabled  bool
	det      PadDetector
	rec      *PadRecorder
	recUntil time.Time
	ps       map[string]*psDevice // by device path
	devNames []string
	stop     chan struct{}
}

type psDevice struct {
	path, name string
	h          windows.Handle
	mu         sync.Mutex
	state      PadSet
	closed     bool
}

func newPadManager(onAction func(string), onLive func(string, []string, bool), onDevs func([]string)) *padManager {
	if xinput.Load() == nil {
		pXInputGetState = xinput.NewProc("XInputGetState")
	} else {
		pXInputGetState = xinputOld.NewProc("XInputGetState")
	}
	return &padManager{onAction: onAction, onLive: onLive, onDevs: onDevs, ps: map[string]*psDevice{}}
}

// Apply takes new settings: bindings, hold time, on/off.
func (m *padManager) Apply(s Settings) {
	m.mu.Lock()
	m.det.Bindings = PadBindingsOf(s)
	m.det.Hold = 0
	if s.PadHold {
		m.det.Hold = 500 * time.Millisecond
	}
	was := m.enabled
	m.enabled = s.PadEnabled
	m.mu.Unlock()
	switch {
	case s.PadEnabled && !was:
		m.stop = make(chan struct{})
		go m.loop(m.stop)
		log.Printf("controller shortcuts on")
	case !s.PadEnabled && was:
		close(m.stop)
		m.closeAll()
		log.Printf("controller shortcuts off")
	}
}

// Record starts recording a new combo for family; "" cancels.
func (m *padManager) Record(family string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if family == "" {
		m.rec = nil
		return
	}
	m.rec = &PadRecorder{Family: family}
	m.recUntil = time.Now().Add(15 * time.Second)
}

func (m *padManager) loop(stop chan struct{}) {
	tick := time.NewTicker(16 * time.Millisecond)
	defer tick.Stop()
	var connected [4]bool
	var lastProbe [4]time.Time
	lastScan := time.Time{}
	var lastLive PadSet
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		now := time.Now()
		if now.Sub(lastScan) > 3*time.Second {
			lastScan = now
			m.scanPS()
		}
		// Xbox: poll XInput. Empty slots are slow to ask, so only every 2 s.
		var xbox PadSet
		var names []string
		for i := 0; i < 4; i++ {
			if !connected[i] && now.Sub(lastProbe[i]) < 2*time.Second {
				continue
			}
			lastProbe[i] = now
			var st xinputState
			r, _, _ := pXInputGetState.Call(uintptr(i), uintptr(unsafe.Pointer(&st)))
			connected[i] = r == 0
			if connected[i] {
				xbox |= XboxState(st.Buttons, st.LT, st.RT)
			}
		}
		for i := 0; i < 4; i++ {
			if connected[i] {
				names = append(names, "Xbox controller")
			}
		}
		var ps PadSet
		m.mu.Lock()
		for _, d := range m.ps {
			d.mu.Lock()
			ps |= d.state
			d.mu.Unlock()
			names = append(names, d.name)
		}
		devChanged := strings.Join(names, "|") != strings.Join(m.devNames, "|")
		if devChanged {
			m.devNames = names
		}
		state := map[string]PadSet{PadXbox: xbox, PadPS: ps}
		var actions []string
		rec := m.rec
		if rec != nil && now.After(m.recUntil) {
			m.rec, rec = nil, nil
			m.mu.Unlock()
			m.onLive("", nil, true) // timed out
			m.mu.Lock()
		}
		var live PadSet
		var combo PadSet
		var done bool
		if rec != nil {
			combo, live, done = rec.Step(state[rec.Family])
			if done {
				m.rec = nil
			}
		} else {
			actions = m.det.Step(now, state)
		}
		m.mu.Unlock()

		if devChanged && m.onDevs != nil {
			m.onDevs(names)
		}
		if rec != nil {
			if done {
				m.onLive(rec.Family, combo.Names(rec.Family), true)
				lastLive = 0
			} else if live != lastLive {
				lastLive = live
				m.onLive(rec.Family, live.Names(rec.Family), false)
			}
		}
		for _, a := range actions {
			log.Printf("controller: %s", a)
			m.onAction(a)
		}
	}
}

// scanPS finds PlayStation controllers and starts reading new ones.
func (m *padManager) scanPS() {
	for _, path := range hidDevicePaths() {
		low := strings.ToLower(path)
		mm := psPathRe.FindStringSubmatch(low)
		if mm == nil {
			continue
		}
		pid, _ := strconv.ParseUint(mm[1], 16, 16)
		model := PSModel(uint16(pid))
		if model == "" {
			continue
		}
		m.mu.Lock()
		_, have := m.ps[low]
		m.mu.Unlock()
		if have {
			continue
		}
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue // in use exclusively (DS4Windows with HidHide), or not a readable collection
		}
		conn := "USB"
		if strings.Contains(low, "{00001124") || strings.Contains(low, "{00001812") {
			conn = "Bluetooth"
		}
		d := &psDevice{path: low, name: model + " (" + conn + ")", h: h}
		m.mu.Lock()
		m.ps[low] = d
		m.mu.Unlock()
		log.Printf("controller: %s connected", d.name)
		go m.readPS(d, model)
	}
}

func (m *padManager) readPS(d *psDevice, model string) {
	buf := make([]byte, 512)
	for {
		var n uint32
		err := windows.ReadFile(d.h, buf, &n, nil)
		if err != nil {
			break
		}
		if st, ok := ParsePSReport(model, buf[:n]); ok {
			d.mu.Lock()
			d.state = st
			d.mu.Unlock()
		}
	}
	d.mu.Lock()
	wasClosed := d.closed
	d.closed = true
	d.mu.Unlock()
	if !wasClosed {
		windows.CloseHandle(d.h)
	}
	m.mu.Lock()
	if m.ps[d.path] == d {
		delete(m.ps, d.path)
	}
	m.mu.Unlock()
	log.Printf("controller: %s disconnected", d.name)
}

func (m *padManager) closeAll() {
	m.mu.Lock()
	devs := m.ps
	m.ps = map[string]*psDevice{}
	m.devNames = nil
	m.mu.Unlock()
	for _, d := range devs {
		d.mu.Lock()
		if !d.closed {
			d.closed = true
			windows.CancelIoEx(d.h, nil)
			windows.CloseHandle(d.h)
		}
		d.mu.Unlock()
	}
	if m.onDevs != nil {
		m.onDevs(nil)
	}
}

// Devices lists connected controllers.
func (m *padManager) Devices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.devNames...)
}

// hidDevicePaths lists the device paths of present HID devices.
func hidDevicePaths() []string {
	h, _, _ := pSetupDiGetClassDevsW.Call(uintptr(unsafe.Pointer(&hidGUID)), 0, 0, 0x02|0x10 /*DIGCF_PRESENT|DIGCF_DEVICEINTERFACE*/)
	if h == 0 || h == ^uintptr(0) {
		return nil
	}
	defer pSetupDiDestroyDeviceInfoList.Call(h)
	type ifaceData struct {
		Size     uint32
		Class    windows.GUID
		Flags    uint32
		Reserved uintptr
	}
	var out []string
	for i := 0; i < 512; i++ {
		var id ifaceData
		id.Size = uint32(unsafe.Sizeof(id))
		if r, _, _ := pSetupDiEnumDeviceInterfaces.Call(h, 0, uintptr(unsafe.Pointer(&hidGUID)), uintptr(i), uintptr(unsafe.Pointer(&id))); r == 0 {
			break
		}
		var need uint32
		pSetupDiGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&id)), 0, 0, uintptr(unsafe.Pointer(&need)), 0)
		if need < 8 || need > 4096 {
			continue
		}
		buf := make([]uint16, (need+1)/2+1)
		*(*uint32)(unsafe.Pointer(&buf[0])) = 8 // sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W) on 64-bit
		if r, _, _ := pSetupDiGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&buf[0])), uintptr(need), 0, 0); r == 0 {
			continue
		}
		out = append(out, windows.UTF16ToString(buf[2:]))
	}
	return out
}
