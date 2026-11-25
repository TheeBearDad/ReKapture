package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestXboxState(t *testing.T) {
	got := XboxState(xiBack|xiStart|xiA, 0, 200).Names(PadXbox)
	if strings.Join(got, ",") != "A,RT,View,Menu" {
		t.Errorf("got %v", got)
	}
	if XboxState(0, 99, 0) != 0 {
		t.Error("a lightly pressed trigger isn't a press")
	}
}

func TestParsePSReport(t *testing.T) {
	// DualSense over USB: report 0x01, 64 bytes, buttons at 8..10.
	usb := make([]byte, 64)
	usb[0] = 0x01
	usb[8] = 0x08 | 0x20  // d-pad released, Cross
	usb[9] = 0x10 | 0x20  // Create, Options
	usb[10] = 0x01 | 0x04 // PS, Mute
	got, ok := ParsePSReport("DualSense", usb)
	if !ok || strings.Join(got.Names(PadPS), ",") != "Cross,Create,Options,PS,Mute" {
		t.Errorf("USB: %v %v", got.Names(PadPS), ok)
	}
	// DualSense over Bluetooth: report 0x31, buttons at 9..11.
	bt := make([]byte, 78)
	bt[0] = 0x31
	bt[9] = 0x02 | 0x80 // d-pad right, Triangle
	bt[10] = 0x01       // L1
	got, ok = ParsePSReport("DualSense", bt)
	if !ok || strings.Join(got.Names(PadPS), ",") != "Triangle,L1,Right" {
		t.Errorf("BT: %v %v", got.Names(PadPS), ok)
	}
	// Basic Bluetooth report (before full reports are switched on), and DS4 USB: buttons at 5.
	basic := []byte{0x01, 128, 128, 128, 128, 0x08 | 0x40, 0x20, 0x00, 0, 0}
	got, ok = ParsePSReport("DualSense", basic)
	if !ok || strings.Join(got.Names(PadPS), ",") != "Circle,Options" {
		t.Errorf("basic: %v %v", got.Names(PadPS), ok)
	}
	got, ok = ParsePSReport("DualShock 4", append(basic[:0:0], basic...))
	if !ok || strings.Join(got.Names(PadPS), ",") != "Circle,Options" {
		t.Errorf("DS4 USB: %v", got.Names(PadPS))
	}
	// DS4 over Bluetooth: report 0x11, buttons at 7.
	ds4bt := make([]byte, 78)
	ds4bt[0] = 0x11
	ds4bt[7] = 0x00 // d-pad up
	ds4bt[8] = 0x10 // Share (shown as Create)
	if got, ok = ParsePSReport("DualShock 4", ds4bt); !ok || strings.Join(got.Names(PadPS), ",") != "Create,Up" {
		t.Errorf("DS4 BT: %v", got.Names(PadPS))
	}
	if _, ok := ParsePSReport("DualSense", []byte{0x05, 1, 2}); ok {
		t.Error("short or unknown reports must be ignored")
	}
}

func TestPadDetector(t *testing.T) {
	s := DefaultSettings("/v")
	d := &PadDetector{Bindings: PadBindingsOf(s)}
	t0 := time.Unix(100, 0)
	view, menu, a := PadSetOf(PadXbox, []string{"View"}), PadSetOf(PadXbox, []string{"Menu"}), PadSetOf(PadXbox, []string{"A"})
	step := func(dt time.Duration, x PadSet, ps PadSet) []string {
		return d.Step(t0.Add(dt), map[string]PadSet{PadXbox: x, PadPS: ps})
	}
	if got := step(0, view, 0); len(got) != 0 {
		t.Fatal("one button of the combo must not fire")
	}
	if got := step(16*time.Millisecond, view|menu|a, 0); !reflect.DeepEqual(got, []string{"save"}) {
		t.Fatalf("combo (with an extra button held) should save: %v", got)
	}
	if got := step(32*time.Millisecond, view|menu, 0); len(got) != 0 {
		t.Fatal("holding the combo must fire only once")
	}
	step(48*time.Millisecond, 0, 0)
	// Steam shows the DualSense as an Xbox pad too: Create+Options and
	// View+Menu arrive together. Only one replay.
	both := step(2*time.Second, view|menu, PadSetOf(PadPS, []string{"Create", "Options"}))
	if !reflect.DeepEqual(both, []string{"save"}) {
		t.Fatalf("doubled controller should save once: %v", both)
	}
	step(3*time.Second, 0, 0)
	if got := step(4*time.Second, 0, PadSetOf(PadPS, []string{"Create", "Options"})); !reflect.DeepEqual(got, []string{"save"}) {
		t.Fatalf("PlayStation combo: %v", got)
	}

	// Hold mode: needs half a second.
	h := &PadDetector{Hold: 500 * time.Millisecond, Bindings: PadBindingsOf(s)}
	if got := h.Step(t0, map[string]PadSet{PadXbox: view | menu}); len(got) != 0 {
		t.Fatal("hold mode fired at once")
	}
	if got := h.Step(t0.Add(300*time.Millisecond), map[string]PadSet{PadXbox: view | menu}); len(got) != 0 {
		t.Fatal("hold mode fired too early")
	}
	if got := h.Step(t0.Add(520*time.Millisecond), map[string]PadSet{PadXbox: view | menu}); !reflect.DeepEqual(got, []string{"save"}) {
		t.Fatalf("hold mode: %v", got)
	}
}

func TestPadRecorder(t *testing.T) {
	r := &PadRecorder{Family: PadPS}
	l1, sq := PadSetOf(PadPS, []string{"L1"}), PadSetOf(PadPS, []string{"Square"})
	for _, st := range []PadSet{l1, l1 | sq, sq} { // press L1, add Square, let go of L1
		if _, _, done := r.Step(st); done {
			t.Fatal("finished while buttons are held")
		}
	}
	combo, _, done := r.Step(0)
	if !done || strings.Join(combo.Names(PadPS), ",") != "Square,L1" {
		t.Errorf("recorded %v %v", combo.Names(PadPS), done)
	}
}

func TestPadSettingsPersist(t *testing.T) {
	def := DefaultSettings("/v")
	if strings.Join(def.PadXbox["save"], "+") != "View+Menu" || strings.Join(def.PadPS["save"], "+") != "Create+Options" {
		t.Fatalf("defaults %v %v", def.PadXbox, def.PadPS)
	}
	s := def
	s.PadXbox = map[string][]string{"save": nil, "record": {"rb", "LB", "nonsense"}, "bogus": {"A"}}
	s.Normalize(DefaultSettings("/v"))
	if len(s.PadXbox["save"]) != 0 || strings.Join(s.PadXbox["record"], "+") != "LB+RB" || s.PadXbox["bogus"] != nil {
		t.Errorf("normalize: %v", s.PadXbox)
	}
	// A cleared shortcut must stay cleared after saving and loading.
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	got := LoadSettings(p, DefaultSettings("/v"))
	if len(got.PadXbox["save"]) != 0 {
		t.Errorf("cleared Xbox save came back after reload: %v (file: %s)", got.PadXbox, raw["padXbox"])
	}
	if strings.Join(got.PadPS["save"], "+") != "Create+Options" {
		t.Errorf("PS default lost: %v", got.PadPS)
	}
}
