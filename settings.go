package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hotkey is a global keyboard shortcut. Mods uses the Win32 MOD_* bits
// (Alt=1, Ctrl=2, Shift=4, Win=8); Key is a Windows virtual-key code.
type Hotkey struct {
	Mods uint32 `json:"mods"`
	Key  uint32 `json:"key"`
}

type Settings struct {
	// Capture source
	CaptureMode string `json:"captureMode"` // "auto" (follow the game) | "monitor" | "window"
	MonitorID   string `json:"monitorId"`   // device name like \\.\DISPLAY1 ("" = primary)
	WindowExe   string `json:"windowExe"`   // e.g. "cs2.exe" when CaptureMode == "window"
	AutoTarget  string `json:"autoTarget"`  // automatic mode records the game's "window" or its "monitor"
	ShowCursor  bool   `json:"showCursor"`

	// Video
	Resolution   string `json:"resolution"` // "native" | "2160" | "1440" | "1080" | "900" | "720" | "custom"
	CustomWidth  int    `json:"customWidth"`
	CustomHeight int    `json:"customHeight"`
	Aspect       string `json:"aspect"`    // "source" | "16:9" | "21:9" | "32:9" | "4:3" | "9:16" | "1:1"
	AspectFit    string `json:"aspectFit"` // "fit" | "crop" | "stretch"
	FPS          int    `json:"fps"`
	Codec        string `json:"codec"`       // "h264" | "hevc" | "av1"
	RateControl  string `json:"rateControl"` // "cbr" | "vbr" | "cq"
	BitrateMbps  int    `json:"bitrateMbps"`
	CQ           int    `json:"cq"`
	Preset       int    `json:"preset"` // 1..7 (NVENC p1..p7)
	Container    string `json:"container"`

	// Audio
	SystemAudio    bool   `json:"systemAudio"`
	SystemDeviceID string `json:"systemDeviceId"` // "" = default output
	Mic            bool   `json:"mic"`
	MicDeviceID    string `json:"micDeviceId"`  // "" = default input
	AudioTracks    string `json:"audioTracks"`  // "mixed" | "separate" | "both"
	DiscordAudio   string `json:"discordAudio"` // "separate" (own track) | "mix" (in game audio) | "exclude" (left out)
	AudioKbps      int    `json:"audioKbps"`

	// Replay / recording
	ReplayEnabled    bool   `json:"replayEnabled"`
	ReplayWhen       string `json:"replayWhen"` // "always" | "games" (only while a game is running)
	ReplaySeconds    int    `json:"replaySeconds"`
	OutputDir        string `json:"outputDir"`
	FolderPerGame    bool   `json:"folderPerGame"`
	HotkeySave       Hotkey `json:"hotkeySave"`
	HotkeyRecord     Hotkey `json:"hotkeyRecord"`
	HotkeyToggle     Hotkey `json:"hotkeyToggle"`
	OverlayToasts    bool   `json:"overlayToasts"`
	OverlayRec       bool   `json:"overlayRec"`
	OverlayCorner    string `json:"overlayCorner"` // "tl" | "tr" | "bl" | "br"
	StartWithWindows bool   `json:"startWithWindows"`
	StartMinimized   bool   `json:"startMinimized"`

	// Games: what ReKapture has detected, and the user's corrections.
	Games []GameRule `json:"games"`

	// Appearance
	Accent string `json:"accent"` // #RRGGBB

	// Updates
	AutoUpdate  bool   `json:"autoUpdate"`  // check GitHub for new versions
	LastVersion string `json:"lastVersion"` // version that last ran (to say "Updated to …")

	// Sharing
	Webhooks      []Webhook `json:"webhooks"`
	DiscordMB     int       `json:"discordMB"`     // server upload limit: 20, 50 or 100
	ShareGameName bool      `json:"shareGameName"` // start the post with the game name
	ClipboardMB   int       `json:"clipboardMB"`   // size to fit when copying: 20 (free), 50 (Nitro Basic), 500 (Nitro)
	ShareMode     string    `json:"shareMode"`     // last used: "post" | "copy"
}

// GameRule is one program in the games list. Detected games are added
// automatically; the user can mark any program as a game or not a game.
type GameRule struct {
	Exe    string `json:"exe"`    // e.g. "Cyberpunk2077.exe"
	Name   string `json:"name"`   // e.g. "Cyberpunk 2077"
	IsGame bool   `json:"isGame"` // false = "not a game", never followed
}

const maxGames = 60

const (
	vkF9     = 0x78
	vkF10    = 0x79
	vkX      = 0x58
	modAlt   = 0x1
	modCtrl  = 0x2
	modShift = 0x4
	modWin   = 0x8
)

func DefaultSettings(videosDir string) Settings {
	return Settings{
		CaptureMode:   "auto",
		AutoTarget:    "window",
		ShowCursor:    true,
		Resolution:    "native",
		CustomWidth:   1920,
		CustomHeight:  1080,
		Aspect:        "source",
		AspectFit:     "fit",
		FPS:           60,
		Codec:         "h264",
		RateControl:   "vbr",
		BitrateMbps:   50,
		CQ:            23,
		Preset:        5,
		Container:     "mp4",
		SystemAudio:   true,
		Mic:           false,
		AudioTracks:   "both",
		DiscordAudio:  "separate",
		AudioKbps:     192,
		ReplayEnabled: true,
		ReplayWhen:    "always",
		ReplaySeconds: 120,
		OutputDir:     filepath.Join(videosDir, "ReKapture"),
		FolderPerGame: true,
		HotkeySave:    Hotkey{Mods: modAlt | modShift, Key: vkF10},
		HotkeyRecord:  Hotkey{Mods: modAlt | modShift, Key: vkF9},
		HotkeyToggle:  Hotkey{Mods: modAlt | modShift, Key: vkX},
		OverlayToasts: true,
		OverlayRec:    true,
		OverlayCorner: "tr",
		Accent:        DefaultAccent,
		AutoUpdate:    true,
		DiscordMB:     20,
		ShareGameName: true,
		ClipboardMB:   20,
		ShareMode:     "copy",
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func oneOf(v string, allowed []string, def string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// Normalize fixes any out-of-range or unknown values so a hand-edited or
// older settings file can never produce an invalid ffmpeg command.
func (s *Settings) Normalize(def Settings) {
	s.CaptureMode = oneOf(s.CaptureMode, []string{"auto", "monitor", "window"}, "monitor")
	if s.CaptureMode == "window" && strings.TrimSpace(s.WindowExe) == "" {
		s.CaptureMode = "monitor"
	}
	s.AutoTarget = oneOf(s.AutoTarget, []string{"window", "monitor"}, "window")
	s.Resolution = oneOf(s.Resolution, []string{"native", "2160", "1440", "1080", "900", "720", "custom"}, "native")
	s.CustomWidth = clamp(s.CustomWidth, 160, 8192) &^ 1
	s.CustomHeight = clamp(s.CustomHeight, 160, 8192) &^ 1
	s.Aspect = oneOf(s.Aspect, []string{"source", "16:9", "21:9", "32:9", "4:3", "9:16", "1:1"}, "source")
	s.AspectFit = oneOf(s.AspectFit, []string{"fit", "crop", "stretch"}, "fit")
	s.FPS = clamp(s.FPS, 10, 240)
	s.Codec = oneOf(s.Codec, []string{"h264", "hevc", "av1"}, "h264")
	s.RateControl = oneOf(s.RateControl, []string{"cbr", "vbr", "cq"}, "vbr")
	s.BitrateMbps = clamp(s.BitrateMbps, 1, 300)
	s.CQ = clamp(s.CQ, 1, 51)
	s.Preset = clamp(s.Preset, 1, 7)
	s.Container = oneOf(s.Container, []string{"mp4", "mkv"}, "mp4")
	s.AudioTracks = oneOf(s.AudioTracks, []string{"mixed", "separate", "both"}, "both")
	s.DiscordAudio = oneOf(s.DiscordAudio, []string{"separate", "mix", "exclude"}, def.DiscordAudio)
	s.ReplayWhen = oneOf(s.ReplayWhen, []string{"always", "games"}, "always")
	s.Games = normalizeGames(s.Games)
	s.AudioKbps = clamp(s.AudioKbps, 64, 320)
	s.ReplaySeconds = clamp(s.ReplaySeconds, 10, 1200)
	if strings.TrimSpace(s.OutputDir) == "" {
		s.OutputDir = def.OutputDir
	}
	if s.ClipboardMB != 20 && s.ClipboardMB != 50 && s.ClipboardMB != 500 {
		s.ClipboardMB = 20
	}
	s.ShareMode = oneOf(s.ShareMode, []string{"post", "copy"}, "copy")
	if s.DiscordMB != 20 && s.DiscordMB != 50 && s.DiscordMB != 100 {
		s.DiscordMB = 20
	}
	var hooks []Webhook
	for _, w := range s.Webhooks {
		if u, err := NormalizeWebhookURL(w.URL); err == nil && len(hooks) < 10 {
			w.URL = u
			w.Name = strings.TrimSpace(w.Name)
			if w.Name == "" {
				w.Name = "Discord channel"
			}
			hooks = append(hooks, w)
		}
	}
	s.Webhooks = hooks
	if !hexColor.MatchString(s.Accent) {
		s.Accent = DefaultAccent
	}
	s.Accent = strings.ToUpper(s.Accent)
	s.OverlayCorner = oneOf(s.OverlayCorner, []string{"tl", "tr", "bl", "br"}, "tr")
	if s.HotkeySave.Key == 0 {
		s.HotkeySave = def.HotkeySave
	}
	if s.HotkeyRecord.Key == 0 {
		s.HotkeyRecord = def.HotkeyRecord
	}
	if s.HotkeyToggle.Key == 0 {
		s.HotkeyToggle = def.HotkeyToggle
	}
}

func normalizeGames(in []GameRule) []GameRule {
	seen := map[string]bool{}
	var out []GameRule
	for _, g := range in {
		g.Exe = strings.TrimSpace(g.Exe)
		k := strings.ToLower(g.Exe)
		if k == "" || seen[k] || strings.ContainsAny(g.Exe, `\/:*?"<>|`) || len(out) >= maxGames {
			continue
		}
		seen[k] = true
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			g.Name = strings.TrimSuffix(g.Exe, filepath.Ext(g.Exe))
		}
		out = append(out, g)
	}
	return out
}

const DefaultAccent = "#76B900"

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// AccentRGB parses a #RRGGBB accent (falls back to the default green).
func AccentRGB(hexStr string) (r, g, b uint8) {
	if !hexColor.MatchString(hexStr) {
		hexStr = DefaultAccent
	}
	var v uint32
	fmt.Sscanf(hexStr[1:], "%06x", &v)
	return uint8(v >> 16), uint8(v >> 8), uint8(v)
}

func LoadSettings(path string, def Settings) Settings {
	s := def
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			s = def
		}
	}
	s.Normalize(def)
	return s
}

func SaveSettings(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// VideoChanged reports whether a change between two settings requires the
// capture pipeline to restart (anything that affects the ffmpeg command).
func VideoChanged(a, b Settings) bool {
	type key struct {
		CaptureMode, MonitorID, WindowExe string
		AutoTarget                        string
		ShowCursor                        bool
		Resolution                        string
		CW, CH                            int
		Aspect, AspectFit                 string
		FPS                               int
		Codec, RC                         string
		Br, CQ, Preset                    int
		Sys                               bool
		SysID                             string
		Mic                               bool
		MicID                             string
		Tracks, Discord                   string
		AKbps                             int
		OutputDir                         string
	}
	k := func(s Settings) key {
		return key{s.CaptureMode, s.MonitorID, s.WindowExe, s.AutoTarget, s.ShowCursor, s.Resolution,
			s.CustomWidth, s.CustomHeight, s.Aspect, s.AspectFit, s.FPS, s.Codec, s.RateControl,
			s.BitrateMbps, s.CQ, s.Preset, s.SystemAudio, s.SystemDeviceID, s.Mic, s.MicDeviceID,
			s.AudioTracks, s.DiscordAudio, s.AudioKbps, s.OutputDir}
	}
	return k(a) != k(b)
}

func HotkeyString(h Hotkey) string {
	var parts []string
	if h.Mods&modCtrl != 0 {
		parts = append(parts, "Ctrl")
	}
	if h.Mods&modAlt != 0 {
		parts = append(parts, "Alt")
	}
	if h.Mods&modShift != 0 {
		parts = append(parts, "Shift")
	}
	if h.Mods&modWin != 0 {
		parts = append(parts, "Win")
	}
	parts = append(parts, vkName(h.Key))
	return strings.Join(parts, "+")
}

func vkName(vk uint32) string {
	switch {
	case vk >= 0x70 && vk <= 0x87:
		return fmt.Sprintf("F%d", vk-0x6F)
	case vk >= 0x30 && vk <= 0x39, vk >= 0x41 && vk <= 0x5A:
		return string(rune(vk))
	case vk >= 0x60 && vk <= 0x69:
		return fmt.Sprintf("Num%d", vk-0x60)
	}
	names := map[uint32]string{0x20: "Space", 0x2C: "PrtSc", 0x13: "Pause", 0x91: "ScrollLock",
		0x2D: "Insert", 0x2E: "Delete", 0x24: "Home", 0x23: "End", 0x21: "PgUp", 0x22: "PgDn",
		0xC0: "`", 0xBD: "-", 0xBB: "=", 0xDB: "[", 0xDD: "]", 0xDC: "\\", 0xBA: ";", 0xDE: "'",
		0xBC: ",", 0xBE: ".", 0xBF: "/"}
	if n, ok := names[vk]; ok {
		return n
	}
	return fmt.Sprintf("Key%d", vk)
}
