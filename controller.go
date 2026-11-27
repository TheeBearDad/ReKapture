package main

// Controller shortcuts: press a combination of buttons on an Xbox or
// PlayStation (DualSense / DualShock 4) controller to save a replay, start
// or stop recording, or turn Instant Replay on or off.
//
// Xbox controllers are read through XInput, PlayStation controllers by
// reading their HID input reports. ReKapture only reads; it never sends
// anything to a controller. Games still see the button presses.

import (
	"strings"
	"time"
)

const (
	PadXbox = "xbox"
	PadPS   = "ps"
)

// PadActions in the order the UI lists them.
var PadActions = []string{"save", "record", "toggle", "shot"}

// Button names, in the order the UI shows them.
var (
	XboxButtons = []string{"A", "B", "X", "Y", "LB", "RB", "LT", "RT", "View", "Menu", "LS", "RS", "Up", "Down", "Left", "Right"}
	PSButtons   = []string{"Cross", "Circle", "Square", "Triangle", "L1", "R1", "L2", "R2", "Create", "Options", "PS", "Touchpad", "Mute", "L3", "R3", "Up", "Down", "Left", "Right"}
)

// PadSet is a set of pressed buttons as a bit mask (index into the button
// list for its family).
type PadSet uint32

func padIndex(family, name string) int {
	list := XboxButtons
	if family == PadPS {
		list = PSButtons
	}
	for i, b := range list {
		if strings.EqualFold(b, name) {
			return i
		}
	}
	return -1
}

// PadSetOf turns button names into a set (unknown names are ignored).
func PadSetOf(family string, names []string) PadSet {
	var s PadSet
	for _, n := range names {
		if i := padIndex(family, n); i >= 0 {
			s |= 1 << uint(i)
		}
	}
	return s
}

// Names lists the buttons in a set, in display order.
func (s PadSet) Names(family string) []string {
	list := XboxButtons
	if family == PadPS {
		list = PSButtons
	}
	var out []string
	for i, b := range list {
		if s&(1<<uint(i)) != 0 {
			out = append(out, b)
		}
	}
	return out
}

func (s PadSet) Count() int {
	n := 0
	for ; s != 0; s &= s - 1 {
		n++
	}
	return n
}

// NormalizeCombo keeps known buttons, in display order, at most 4.
func NormalizeCombo(family string, names []string) []string {
	out := PadSetOf(family, names).Names(family)
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// ---------- Xbox (XInput) ----------

// XInput button bits.
const (
	xiUp, xiDown, xiLeft, xiRight = 0x0001, 0x0002, 0x0004, 0x0008
	xiStart, xiBack, xiLS, xiRS   = 0x0010, 0x0020, 0x0040, 0x0080
	xiLB, xiRB                    = 0x0100, 0x0200
	xiA, xiB, xiX, xiY            = 0x1000, 0x2000, 0x4000, 0x8000
	triggerPressed                = 100 // of 255
)

// XboxState turns an XInput gamepad state into a button set.
func XboxState(buttons uint16, lt, rt uint8) PadSet {
	m := map[uint16]string{xiA: "A", xiB: "B", xiX: "X", xiY: "Y", xiLB: "LB", xiRB: "RB", xiBack: "View", xiStart: "Menu",
		xiLS: "LS", xiRS: "RS", xiUp: "Up", xiDown: "Down", xiLeft: "Left", xiRight: "Right"}
	var names []string
	for bit, n := range m {
		if buttons&bit != 0 {
			names = append(names, n)
		}
	}
	if lt >= triggerPressed {
		names = append(names, "LT")
	}
	if rt >= triggerPressed {
		names = append(names, "RT")
	}
	return PadSetOf(PadXbox, names)
}

// ---------- PlayStation (HID input reports) ----------

// PSModel tells DualSense and DualShock 4 apart by USB product ID.
func PSModel(pid uint16) string {
	switch pid {
	case 0x0CE6:
		return "DualSense"
	case 0x0DF2:
		return "DualSense Edge"
	case 0x05C4, 0x09CC, 0x0BA0:
		return "DualShock 4"
	}
	return ""
}

// ParsePSReport reads the buttons from a DualSense or DualShock 4 input
// report (USB or Bluetooth). ok is false for reports without buttons.
func ParsePSReport(model string, r []byte) (PadSet, bool) {
	if len(r) < 10 {
		return 0, false
	}
	// Where the three button bytes start, by report type:
	//   DualSense USB 0x01 (64 bytes)       buttons at 8
	//   DualSense Bluetooth 0x31 (78 bytes) buttons at 9
	//   Basic Bluetooth 0x01 (10 bytes)     buttons at 5 (DualSense before Steam/games switch it to full reports, and DS4 over USB)
	//   DualShock 4 Bluetooth 0x11          buttons at 7
	off := -1
	ds := strings.HasPrefix(model, "DualSense")
	switch {
	case ds && r[0] == 0x01 && len(r) >= 64:
		off = 8
	case ds && r[0] == 0x31 && len(r) >= 12:
		off = 9
	case r[0] == 0x01:
		off = 5
	case !ds && r[0] == 0x11 && len(r) >= 10:
		off = 7
	}
	if off < 0 || off+2 >= len(r) {
		return 0, false
	}
	b0, b1, b2 := r[off], r[off+1], r[off+2]
	var names []string
	switch b0 & 0x0F { // d-pad as a hat: 0 = up, clockwise, 8 = released
	case 0:
		names = append(names, "Up")
	case 1:
		names = append(names, "Up", "Right")
	case 2:
		names = append(names, "Right")
	case 3:
		names = append(names, "Down", "Right")
	case 4:
		names = append(names, "Down")
	case 5:
		names = append(names, "Down", "Left")
	case 6:
		names = append(names, "Left")
	case 7:
		names = append(names, "Up", "Left")
	}
	bits := []struct {
		b    byte
		mask byte
		name string
	}{
		{b0, 0x10, "Square"}, {b0, 0x20, "Cross"}, {b0, 0x40, "Circle"}, {b0, 0x80, "Triangle"},
		{b1, 0x01, "L1"}, {b1, 0x02, "R1"}, {b1, 0x04, "L2"}, {b1, 0x08, "R2"},
		{b1, 0x10, "Create"}, {b1, 0x20, "Options"}, {b1, 0x40, "L3"}, {b1, 0x80, "R3"},
		{b2, 0x01, "PS"}, {b2, 0x02, "Touchpad"},
	}
	if ds && off != 5 {
		bits = append(bits, struct {
			b    byte
			mask byte
			name string
		}{b2, 0x04, "Mute"})
	}
	for _, x := range bits {
		if x.b&x.mask != 0 {
			names = append(names, x.name)
		}
	}
	return PadSetOf(PadPS, names), true
}

// ---------- combo detection ----------

// PadBindings maps action -> combo for one controller family.
type PadBindings map[string]PadSet

// PadDetector turns controller states into actions. An action fires once
// when all of its buttons are down together (other buttons may also be
// held, so it works while you steer or aim). With hold set, they must stay
// down that long. The same action never fires twice within a second, so
// a PlayStation controller that Steam also shows as an Xbox controller
// doesn't save two replays.
type PadDetector struct {
	Hold     time.Duration
	Bindings map[string]PadBindings // family -> bindings

	since map[string]time.Time // family/action -> when the combo became held
	fired map[string]bool
	last  map[string]time.Time // action -> last time it fired
}

func (d *PadDetector) init() {
	if d.since == nil {
		d.since, d.fired, d.last = map[string]time.Time{}, map[string]bool{}, map[string]time.Time{}
	}
}

// Step takes the current buttons for each family and returns actions to run.
func (d *PadDetector) Step(now time.Time, state map[string]PadSet) []string {
	d.init()
	var out []string
	for _, fam := range []string{PadXbox, PadPS} {
		pressed := state[fam]
		for _, act := range PadActions {
			combo := d.Bindings[fam][act]
			key := fam + "/" + act
			if combo == 0 || pressed&combo != combo {
				delete(d.since, key)
				d.fired[key] = false
				continue
			}
			if _, ok := d.since[key]; !ok {
				d.since[key] = now
			}
			if d.fired[key] || now.Sub(d.since[key]) < d.Hold {
				continue
			}
			d.fired[key] = true
			if now.Sub(d.last[act]) < time.Second {
				continue
			}
			d.last[act] = now
			out = append(out, act)
		}
	}
	return out
}

// PadRecorder records a combo while the user sets a shortcut: it collects
// every button held at the same time, and finishes when all are let go.
type PadRecorder struct {
	Family string
	best   PadSet
	cur    PadSet
}

// Step feeds the family's current buttons. done is true when a combo has
// been pressed and fully released.
func (r *PadRecorder) Step(pressed PadSet) (combo PadSet, live PadSet, done bool) {
	r.cur = pressed
	if pressed.Count() > r.best.Count() || (pressed != 0 && pressed&r.best == r.best) {
		r.best = pressed
	}
	if pressed == 0 && r.best != 0 {
		return r.best, 0, true
	}
	return 0, pressed, false
}
