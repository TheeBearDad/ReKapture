package main

import (
	"strings"
	"testing"
)

func TestClassifyGame(t *testing.T) {
	steam := ForegroundApp{PID: 10, Exe: "Cyberpunk2077.exe", Title: "Cyberpunk 2077 (C) 2020 by CD Projekt RED",
		Path: `D:\SteamLibrary\steamapps\common\Cyberpunk 2077\bin\x64\Cyberpunk2077.exe`}
	cases := []struct {
		name string
		app  ForegroundApp
		want bool
	}{
		{"steam game, windowed", steam, true},
		{"epic game", ForegroundApp{Exe: "FortniteClient-Win64-Shipping.exe", Path: `C:\Program Files\Epic Games\Fortnite\FortniteGame\Binaries\Win64\FortniteClient-Win64-Shipping.exe`}, true},
		{"xbox game", ForegroundApp{Exe: "Forza.exe", Path: `C:\XboxGames\Forza Horizon 5\Content\ForzaHorizon5.exe`}, true},
		{"unknown full screen", ForegroundApp{Exe: "Overwatch.exe", Path: `C:\Program Files (x86)\Overwatch\_retail_\Overwatch.exe`, Fullscreen: true}, true},
		{"unknown windowed", ForegroundApp{Exe: "tool.exe", Path: `C:\Tools\tool.exe`}, false},
		{"browser full screen", ForegroundApp{Exe: "chrome.exe", Path: `C:\Program Files\Google\Chrome\Application\chrome.exe`, Fullscreen: true}, false},
		{"steam client", ForegroundApp{Exe: "steamwebhelper.exe", Path: `C:\Program Files (x86)\Steam\bin\cef\steamwebhelper.exe`, Fullscreen: true}, false},
		{"riot launcher in library folder", ForegroundApp{Exe: "RiotClientUx.exe", Path: `C:\Riot Games\Riot Client\RiotClientUx.exe`}, false},
		{"windows app full screen", ForegroundApp{Exe: "something.exe", Path: `C:\Windows\System32\something.exe`, Fullscreen: true}, false},
		{"desktop", ForegroundApp{Shell: true, Exe: "explorer.exe", Fullscreen: true}, false},
		{"premiere", ForegroundApp{Exe: "Adobe Premiere Pro.exe", Fullscreen: true}, false},
	}
	for _, c := range cases {
		if got, _ := ClassifyGame(c.app, nil); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if _, n := ClassifyGame(steam, nil); n != "Cyberpunk 2077" {
		t.Errorf("name %q", n)
	}
	// The user's list wins both ways.
	rules := []GameRule{{Exe: "cyberpunk2077.exe", Name: "CP", IsGame: false}, {Exe: "tool.exe", Name: "My Tool", IsGame: true}}
	if g, _ := ClassifyGame(steam, rules); g {
		t.Error("'not a game' rule ignored")
	}
	if g, n := ClassifyGame(ForegroundApp{Exe: "TOOL.exe"}, rules); !g || n != "My Tool" {
		t.Errorf("'game' rule ignored: %v %q", g, n)
	}
}

func TestGameTracker(t *testing.T) {
	alive := map[uint32]bool{1: true, 2: true}
	isAlive := func(p uint32) bool { return alive[p] }
	var tr GameTracker
	game := ForegroundApp{PID: 1, Exe: "a.exe"}
	other := ForegroundApp{PID: 2, Exe: "b.exe"}
	discord := ForegroundApp{PID: 3, Exe: "Discord.exe"}
	step := func(app ForegroundApp, isGame bool) (string, string) {
		s, st := tr.Step(app, isGame, strings.TrimSuffix(app.Exe, ".exe"), isAlive)
		n := func(g *ActiveGame) string {
			if g == nil {
				return ""
			}
			return g.Exe
		}
		return n(s), n(st)
	}
	if _, st := step(game, true); st != "" {
		t.Fatal("started on the first sample (should wait ~2 s)")
	}
	if _, st := step(game, true); st != "a.exe" {
		t.Fatal("didn't start after 2 samples")
	}
	// Alt-tab to Discord: keep following the game.
	for i := 0; i < 5; i++ {
		if s, st := step(discord, false); s != "" || st != "" {
			t.Fatal("alt-tab stopped the game")
		}
	}
	// Another game in front briefly doesn't steal it; staying in front does.
	step(other, true)
	step(other, true)
	if tr.Current().Exe != "a.exe" {
		t.Fatal("switched too quickly")
	}
	if s, st := step(other, true); s != "a.exe" || st != "b.exe" {
		t.Fatalf("switch: stopped %q started %q", s, st)
	}
	// Game closes while the desktop is in front.
	alive[2] = false
	if s, st := step(ForegroundApp{Shell: true}, false); s != "b.exe" || st != "" || tr.Current() != nil {
		t.Fatalf("close: stopped %q started %q", s, st)
	}
}

func TestDiscordRoot(t *testing.T) {
	procs := []Proc{
		{PID: 4, PPID: 0, Exe: "System"},
		{PID: 100, PPID: 50, Exe: "explorer.exe"},
		{PID: 200, PPID: 100, Exe: "Discord.exe"}, // main
		{PID: 201, PPID: 200, Exe: "Discord.exe"}, // GPU
		{PID: 202, PPID: 200, Exe: "Discord.exe"}, // voice / utility
		{PID: 300, PPID: 100, Exe: "game.exe"},
	}
	if got := DiscordRoot(procs); got != 200 {
		t.Errorf("root = %d, want 200", got)
	}
	if got := DiscordRoot(procs[:2]); got != 0 {
		t.Errorf("no Discord: %d", got)
	}
	ptb := append(procs, Proc{PID: 400, PPID: 100, Exe: "DiscordPTB.exe"})
	if got := DiscordRoot(ptb); got != 200 {
		t.Errorf("two Discords should pick the busier one, got %d", got)
	}
}

func TestEffectiveDiscord(t *testing.T) {
	s := DefaultSettings("/v")
	if s.DiscordAudio != "separate" || EffectiveDiscord(s) != "separate" {
		t.Error("default should be a separate Discord track")
	}
	s.AudioTracks = "mixed"
	if EffectiveDiscord(s) != "mix" {
		t.Error("a single mixed track has nothing to separate")
	}
	s.DiscordAudio = "exclude"
	if EffectiveDiscord(s) != "exclude" {
		t.Error("leaving Discord out still applies to one track")
	}
	s.SystemAudio = false
	if EffectiveDiscord(s) != "mix" {
		t.Error("no game audio means no Discord handling")
	}
}

func TestAudioTrackLayout(t *testing.T) {
	in := []AudioInput{{"a", TitleGame}, {"b", TitleDiscord}, {"c", TitleMic}}
	tracks, graph := AudioTrackLayout("both", in)
	var titles []string
	for _, tr := range tracks {
		titles = append(titles, tr.Title)
	}
	if strings.Join(titles, "|") != "Game + Discord + Mic|Game audio|Discord|Microphone" {
		t.Errorf("both: %v", titles)
	}
	if !strings.Contains(strings.Join(graph, ";"), "amix=inputs=3") {
		t.Errorf("both graph: %v", graph)
	}
	tracks, _ = AudioTrackLayout("separate", in)
	if len(tracks) != 3 || tracks[1].Label != "1:a" {
		t.Errorf("separate: %+v", tracks)
	}
	tracks, graph = AudioTrackLayout("mixed", in[:2])
	if len(tracks) != 1 || tracks[0].Title != "Game + Discord" || graph[0] != "[0:a][1:a]amix=inputs=2:duration=longest:dropout_transition=0:normalize=0[amix]" {
		t.Errorf("mixed: %+v %v", tracks, graph)
	}
	// Game + mic keeps the title older clips use, so the editor still knows them.
	if MixTitle([]string{TitleGame, TitleMic}) != "Game + Mic" {
		t.Error("compat title")
	}
}

func TestNormalizeGames(t *testing.T) {
	in := []GameRule{{Exe: " a.exe ", Name: ""}, {Exe: "A.EXE", Name: "dup"}, {Exe: `..\bad.exe`}, {Exe: ""}}
	out := normalizeGames(in)
	if len(out) != 1 || out[0].Exe != "a.exe" || out[0].Name != "a" {
		t.Errorf("%+v", out)
	}
	s := Settings{CaptureMode: "nonsense", DiscordAudio: "x", ReplayWhen: "x", AutoTarget: "x"}
	s.Normalize(DefaultSettings("/v"))
	if s.CaptureMode != "monitor" || s.DiscordAudio != "separate" || s.ReplayWhen != "always" || s.AutoTarget != "window" {
		t.Errorf("normalize: %+v", s)
	}
}
