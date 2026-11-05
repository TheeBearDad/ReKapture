package main

import (
	"path/filepath"
	"strings"
)

// ForegroundApp describes the window the user is looking at. It is gathered
// with the same harmless calls Task Manager uses (window title, size and the
// program's path); ReKapture never opens a game's memory or modules, so
// anti-cheat has nothing to object to.
type ForegroundApp struct {
	PID        uint32
	Exe        string // file name, e.g. "Cyberpunk2077.exe"
	Path       string // full path to the program
	Title      string
	Fullscreen bool   // the window covers its whole monitor
	Shell      bool   // desktop, taskbar, Explorer or ReKapture itself
	MonitorID  string // monitor the window is on, e.g. \\.\DISPLAY1
}

// ActiveGame is the game ReKapture is following.
type ActiveGame struct {
	Exe       string `json:"exe"`
	Name      string `json:"name"`
	PID       uint32 `json:"-"`
	MonitorID string `json:"-"`
}

// Folders game stores install into. Anything running from one of these is
// a game, even in a small window.
var gameLibraries = []string{
	`\steamapps\common\`, `\epic games\`, `\xboxgames\`, `\gog galaxy\games\`, `\gog games\`,
	`\ubisoft game launcher\games\`, `\ea games\`, `\origin games\`, `\riot games\`,
	`\rockstar games\`, `\battlestate games\`, `\amazon games\library\`, `\itch\apps\`,
}

// Programs that are never games, even full screen (browsers playing video,
// launchers, chat, recorders, presentations).
var notGames = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`
		chrome.exe msedge.exe firefox.exe opera.exe opera_gx.exe brave.exe vivaldi.exe arc.exe iexplore.exe
		discord.exe discordptb.exe discordcanary.exe teams.exe ms-teams.exe slack.exe zoom.exe skype.exe whatsapp.exe telegram.exe
		steam.exe steamwebhelper.exe epicgameslauncher.exe eadesktop.exe origin.exe battle.net.exe upc.exe ubisoftconnect.exe
		galaxyclient.exe riotclientservices.exe riotclientux.exe riotclientuxrender.exe rockstarlauncher.exe launcher.exe
		xboxpcapp.exe gamingservices.exe playnite.desktopapp.exe playnite.fullscreenapp.exe
		obs64.exe obs32.exe rekapture.exe ffmpeg.exe nvidia\ app.exe nvidia\ overlay.exe nvcontainer.exe streamlabs\ obs.exe
		vlc.exe mpc-hc64.exe mpc-hc.exe mpc-be64.exe potplayermini64.exe wmplayer.exe video.ui.exe microsoft.media.player.exe
		spotify.exe applemusic.exe itunes.exe netflix.exe plex.exe
		explorer.exe dwm.exe searchhost.exe startmenuexperiencehost.exe shellexperiencehost.exe lockapp.exe taskmgr.exe
		applicationframehost.exe systemsettings.exe textinputhost.exe windowsterminal.exe cmd.exe powershell.exe pwsh.exe
		code.exe devenv.exe rider64.exe idea64.exe notepad.exe notepad++.exe winword.exe excel.exe powerpnt.exe outlook.exe onenote.exe
		acrobat.exe acrord32.exe photoshop.exe afterfx.exe "adobe premiere pro.exe" resolve.exe blender.exe
		mstsc.exe vmware.exe virtualboxvm.exe parsecd.exe anydesk.exe teamviewer.exe moonlight.exe
	`) {
		notGames[e] = true
	}
	for _, e := range []string{"nvidia app.exe", "nvidia overlay.exe", "streamlabs obs.exe", "adobe premiere pro.exe"} {
		notGames[e] = true
	}
}

// ClassifyGame decides whether the foreground app is a game, and what to
// call it. The user's own list always wins.
func ClassifyGame(app ForegroundApp, rules []GameRule) (bool, string) {
	if app.Shell || app.Exe == "" {
		return false, ""
	}
	exe := strings.ToLower(app.Exe)
	for _, r := range rules {
		if strings.EqualFold(r.Exe, app.Exe) {
			return r.IsGame, r.Name
		}
	}
	if notGames[exe] || strings.HasPrefix(exe, "rekapture") {
		return false, ""
	}
	name := GameName(app)
	path := strings.ToLower(filepath.ToSlash(app.Path))
	path = strings.ReplaceAll(path, "/", `\`)
	for _, lib := range gameLibraries {
		if strings.Contains(path, lib) {
			return true, name
		}
	}
	if app.Fullscreen && !strings.HasPrefix(path, `c:\windows\`) {
		return true, name
	}
	return false, ""
}

// GameName is a clean, folder-safe name for a game: its window title without
// trademark signs or version numbers, or the program name.
func GameName(app ForegroundApp) string {
	n := CleanTitle(app.Title)
	if n == "" || n == "Desktop" {
		n = strings.TrimSuffix(app.Exe, filepath.Ext(app.Exe))
	}
	return n
}

// GameTracker follows one game at a time. A game is picked up after it has
// been in front for a couple of seconds, and kept until its program closes,
// so alt-tabbing to Discord or a browser doesn't stop anything. Another game
// takes over only after it has been in front a little longer.
type GameTracker struct {
	cur      *ActiveGame
	pending  string
	pendingN int
}

const (
	gameStartSamples  = 2 // samples are about a second apart
	gameSwitchSamples = 3
)

// Current returns the game being followed, or nil.
func (t *GameTracker) Current() *ActiveGame { return t.cur }

// Step feeds one foreground sample. It returns the game that stopped and the
// game that started (either may be nil; both are set when one game replaces
// another).
func (t *GameTracker) Step(app ForegroundApp, isGame bool, name string, alive func(uint32) bool) (stopped, started *ActiveGame) {
	if t.cur != nil && !alive(t.cur.PID) {
		stopped, t.cur = t.cur, nil
	}
	if !isGame {
		t.pending, t.pendingN = "", 0
		return stopped, nil
	}
	key := strings.ToLower(app.Exe)
	if t.cur != nil && strings.ToLower(t.cur.Exe) == key {
		t.cur.PID = app.PID // same game (it may have restarted itself)
		t.cur.MonitorID = app.MonitorID
		t.pending, t.pendingN = "", 0
		return stopped, nil
	}
	if t.pending == key {
		t.pendingN++
	} else {
		t.pending, t.pendingN = key, 1
	}
	need := gameStartSamples
	if t.cur != nil {
		need = gameSwitchSamples
	}
	if t.pendingN < need {
		return stopped, nil
	}
	if t.cur != nil {
		stopped = t.cur
	}
	t.cur = &ActiveGame{Exe: app.Exe, Name: name, PID: app.PID, MonitorID: app.MonitorID}
	t.pending, t.pendingN = "", 0
	return stopped, t.cur
}

// Proc is one running process.
type Proc struct {
	PID, PPID uint32
	Exe       string
}

var discordExes = map[string]bool{"discord.exe": true, "discordptb.exe": true, "discordcanary.exe": true, "discorddevelopment.exe": true}

// DiscordRoot finds the main Discord process: a Discord process whose parent
// isn't Discord. Its children (including the one that plays voice chat) are
// captured with it. Returns 0 when Discord isn't running.
func DiscordRoot(procs []Proc) uint32 {
	isDiscord := map[uint32]bool{}
	for _, p := range procs {
		if discordExes[strings.ToLower(p.Exe)] {
			isDiscord[p.PID] = true
		}
	}
	best, bestKids := uint32(0), -1
	for _, p := range procs {
		if !isDiscord[p.PID] || isDiscord[p.PPID] {
			continue
		}
		kids := 0
		for _, c := range procs {
			if c.PPID == p.PID && isDiscord[c.PID] {
				kids++
			}
		}
		if kids > bestKids { // two Discords (e.g. stable + PTB): the busier one
			best, bestKids = p.PID, kids
		}
	}
	return best
}

// EffectiveDiscord is how Discord audio is actually handled. With a single
// mixed track there's nothing to separate, so it stays in the game audio.
func EffectiveDiscord(s Settings) string {
	if !s.SystemAudio {
		return "mix"
	}
	if s.DiscordAudio == "separate" && s.AudioTracks == "mixed" {
		return "mix"
	}
	return s.DiscordAudio
}
