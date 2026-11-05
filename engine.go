package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AudioSource captures one audio device and serves it as raw f32le/48k/stereo
// on a pipe that ffmpeg opens as an input.
type AudioSource interface {
	// Open prepares the pipe and returns the path ffmpeg should read.
	// Capture starts immediately; samples are aligned to t0.
	Open(t0 time.Time) (string, error)
	Close()
}

// Platform is everything OS-specific the engine needs.
type Platform interface {
	ResolveCapture(s Settings) (CaptureSource, error)
	NewAudio(kind, deviceID string) AudioSource
	Command(name string, args ...string) *exec.Cmd
	ForegroundName() string
	Notify(kind, title, msg string)
	HideDir(path string)
	AfterStart(cmd *exec.Cmd)
	// ForegroundApp describes the window in front, for game detection.
	ForegroundApp() ForegroundApp
	ProcessAlive(pid uint32) bool
}

type Clip struct {
	Path   string  `json:"path"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	SizeMB float64 `json:"sizeMB"`
	When   string  `json:"when"`
}

type Status struct {
	State         string  `json:"state"` // off|standby|starting|buffering|recording|waiting|error
	ReplayOn      bool    `json:"replayOn"`
	Recording     bool    `json:"recording"`
	RecordSeconds int     `json:"recordSeconds"`
	BufferSeconds float64 `json:"bufferSeconds"`
	Finalizing    int     `json:"finalizing"`
	Message       string  `json:"message"`
	Error         string  `json:"error"`
	Video         string  `json:"video"`
	Source        string  `json:"source"`
	Game          string  `json:"game"` // the game being followed, if any
	Clips         []Clip  `json:"clips"`
}

type Engine struct {
	mu       sync.Mutex
	plat     Platform
	ffmpeg   string
	settings Settings
	onStatus func(Status)

	// desired state
	replayOn  bool
	recording bool
	recStart  time.Time
	recStream float64
	recName   string

	// running pipeline
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	running   bool
	stopping  bool
	exited    chan struct{}
	t0        time.Time
	buf       *Buffer
	audio     []AudioSource
	source    CaptureSource
	stderr    []string
	restartAt time.Time
	failures  int

	finalizing int
	msg        string
	errMsg     string
	clips      []Clip
	quit       chan struct{}
	shutOnce   sync.Once
	ownDirs    map[string]bool
	videoTest  string // tests: replaces gfxcapture
	encTest    []string

	// game following
	tracker    GameTracker
	game       *ActiveGame // copy of tracker.Current(), under mu
	srcKey     string      // what the running pipeline captures
	tick       int
	onSettings func(Settings) // the engine changed settings (a game was added to the list)
}

func NewEngine(p Platform, ffmpeg string, s Settings, onStatus func(Status), onSettings func(Settings)) *Engine {
	e := &Engine{plat: p, ffmpeg: ffmpeg, settings: s, onStatus: onStatus, onSettings: onSettings, quit: make(chan struct{})}
	e.replayOn = s.ReplayEnabled
	go e.loop()
	go e.RefreshClips()
	return e
}

// RefreshClips reloads the Recent clips list from the clips folder.
func (e *Engine) RefreshClips() {
	dir := e.Settings().OutputDir
	clips := ScanClips(dir, 12)
	e.mu.Lock()
	e.clips = clips
	e.mu.Unlock()
	e.push()
}

func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings
}

// Apply swaps in new settings, restarting the pipeline if needed.
func (e *Engine) Apply(s Settings) error {
	e.mu.Lock()
	old := e.settings
	s.ReplayEnabled = e.replayOn
	if e.recording && VideoChanged(old, s) {
		e.mu.Unlock()
		return errors.New("stop the current recording before changing capture, video or audio settings")
	}
	e.settings = s
	if e.buf != nil {
		e.buf.SetKeep(float64(s.ReplaySeconds))
	}
	restart := e.running && VideoChanged(old, s)
	e.mu.Unlock()
	if old.OutputDir != s.OutputDir {
		go e.RefreshClips()
	}
	if restart {
		e.stopPipeline()
		e.setMsg("Settings applied — capture restarted")
	}
	e.kick()
	return nil
}

func (e *Engine) SetReplay(on bool) {
	e.mu.Lock()
	e.replayOn = on
	e.settings.ReplayEnabled = on
	e.failures = 0
	e.restartAt = time.Time{}
	rec := e.recording
	e.mu.Unlock()
	if !on && !rec {
		e.stopPipeline()
	}
	if on {
		s := e.Settings()
		if s.ReplayWhen == "games" && e.CurrentGame() == nil {
			e.plat.Notify("info", "Instant Replay on", "It starts when you open a game")
		} else {
			e.plat.Notify("info", "Instant Replay on", "Save the last moments with "+HotkeyString(s.HotkeySave))
		}
	} else {
		e.plat.Notify("info", "Instant Replay off", "")
	}
	e.kick()
}

func (e *Engine) ReplayOn() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replayOn
}

func (e *Engine) streamNow() float64 {
	return time.Since(e.t0).Seconds()
}

func (e *Engine) setMsg(m string) {
	e.mu.Lock()
	e.msg = m
	e.mu.Unlock()
	e.push()
}

func (e *Engine) setErr(m string) {
	e.mu.Lock()
	e.errMsg = m
	e.mu.Unlock()
	if m != "" {
		log.Printf("error: %s", m)
	}
	e.push()
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{ReplayOn: e.replayOn, Recording: e.recording, Finalizing: e.finalizing,
		Message: e.msg, Error: e.errMsg, Clips: append([]Clip(nil), e.clips...)}
	if e.game != nil {
		st.Game = e.game.Name
	}
	if e.running {
		st.Video = DescribeVideo(e.settings, e.source)
		st.Source = e.source.Label
	} else {
		st.Video = DescribeVideo(e.settings, CaptureSource{})
	}
	switch {
	case e.recording:
		st.State = "recording"
		st.RecordSeconds = int(time.Since(e.recStart).Seconds())
	case e.running && e.replayOn:
		st.State = "buffering"
	case (e.replayOn || e.recording) && strings.HasPrefix(e.errMsg, "Waiting"):
		st.State = "waiting"
	case e.replayOn && e.errMsg != "":
		st.State = "error"
	case e.replayOn && !e.wantPipelineLocked():
		st.State = "standby" // Instant Replay runs only while a game is open
	case e.replayOn && !e.restartAt.IsZero():
		st.State = "waiting"
	case e.replayOn:
		st.State = "starting"
	default:
		st.State = "off"
	}
	if e.buf != nil && e.running {
		st.BufferSeconds = e.buf.BufferedSeconds()
		if st.BufferSeconds > float64(e.settings.ReplaySeconds) {
			st.BufferSeconds = float64(e.settings.ReplaySeconds)
		}
	}
	return st
}

func (e *Engine) push() {
	if e.onStatus != nil {
		e.onStatus(e.Status())
	}
}

var kickCh = make(chan struct{}, 1)

func (e *Engine) kick() {
	select {
	case kickCh <- struct{}{}:
	default:
	}
}

// loop keeps the pipeline in the desired state and refreshes the buffer.
func (e *Engine) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.quit:
			return
		case <-t.C:
		case <-kickCh:
		}
		e.tick++
		if e.tick%2 == 0 {
			e.sampleGame()
		}
		e.mu.Lock()
		want := e.wantPipelineLocked()
		running := e.running
		recording := e.recording
		retarget := running && !recording && e.srcKey != targetKey(e.effectiveLocked(e.settings))
		wait := !e.restartAt.IsZero() && time.Now().Before(e.restartAt)
		buf := e.buf
		e.mu.Unlock()
		if want && !running && !wait {
			if err := e.startPipeline(); err != nil {
				e.mu.Lock()
				e.failures++
				delay := 3 * time.Second
				if e.failures > 3 {
					delay = 10 * time.Second
				}
				e.restartAt = time.Now().Add(delay)
				e.mu.Unlock()
				e.setErr(err.Error())
			}
		}
		if running && !recording && (!want || retarget) {
			// The game closed (replay runs only while gaming), or a different
			// game should be captured: stop, and the next pass starts again.
			e.stopPipeline()
			continue
		}
		if buf != nil && running {
			buf.Refresh()
		}
		e.push()
	}
}

// wantPipelineLocked: should capture be running right now?
func (e *Engine) wantPipelineLocked() bool {
	if e.recording {
		return true
	}
	return e.replayOn && (e.settings.ReplayWhen != "games" || e.game != nil)
}

// effectiveLocked turns "Automatic" capture into a concrete source: the
// game's window (or its monitor), or the chosen monitor when no game runs.
func (e *Engine) effectiveLocked(s Settings) Settings {
	if s.CaptureMode != "auto" {
		return s
	}
	if e.game == nil {
		s.CaptureMode = "monitor"
		return s
	}
	if s.AutoTarget == "monitor" {
		s.CaptureMode = "monitor"
		if e.game.MonitorID != "" {
			s.MonitorID = e.game.MonitorID
		}
		return s
	}
	s.CaptureMode = "window"
	s.WindowExe = e.game.Exe
	return s
}

func targetKey(s Settings) string {
	if s.CaptureMode == "window" {
		return "window|" + strings.ToLower(s.WindowExe)
	}
	return "monitor|" + s.MonitorID
}

// CurrentGame returns the game being followed, or nil.
func (e *Engine) CurrentGame() *ActiveGame {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.game == nil {
		return nil
	}
	g := *e.game
	return &g
}

// sampleGame looks at the window in front and updates which game is being
// followed. Runs about once a second on the engine loop.
func (e *Engine) sampleGame() {
	s := e.Settings()
	app := e.plat.ForegroundApp()
	isGame, name := ClassifyGame(app, s.Games)
	stopped, started := e.tracker.Step(app, isGame, name, e.plat.ProcessAlive)
	cur := e.tracker.Current()
	e.mu.Lock()
	if cur == nil {
		e.game = nil
	} else {
		g := *cur
		e.game = &g
	}
	replayOn := e.replayOn
	e.mu.Unlock()
	if stopped != nil {
		log.Printf("game closed: %s (%s)", stopped.Name, stopped.Exe)
		if started == nil && replayOn && s.ReplayWhen == "games" {
			e.plat.Notify("info", "Instant Replay paused", "It starts again when you open a game")
		}
	}
	if started == nil {
		return
	}
	log.Printf("game detected: %s (%s)", started.Name, started.Exe)
	known := false
	for _, r := range s.Games {
		if strings.EqualFold(r.Exe, started.Exe) {
			known = true
		}
	}
	if !known {
		e.mu.Lock()
		e.settings.Games = normalizeGames(append([]GameRule{{Exe: started.Exe, Name: started.Name, IsGame: true}}, e.settings.Games...))
		ns := e.settings
		e.mu.Unlock()
		if e.onSettings != nil {
			e.onSettings(ns)
		}
	}
	if replayOn {
		switch {
		case s.CaptureMode == "auto":
			e.plat.Notify("info", started.Name, "Instant Replay is following this game")
		case s.ReplayWhen == "games":
			e.plat.Notify("info", started.Name, "Instant Replay is on")
		}
	}
	e.push()
}

// clipName is the name clips are saved under: the game being followed, or
// the window in front.
func (e *Engine) clipName() string {
	if g := e.CurrentGame(); g != nil {
		return g.Name
	}
	return e.plat.ForegroundName()
}

func (e *Engine) startPipeline() error {
	e.mu.Lock()
	s := e.effectiveLocked(e.settings)
	e.mu.Unlock()
	src, err := e.plat.ResolveCapture(s)
	if err != nil {
		return err
	}
	g := ComputeGeometry(s, src.Width, src.Height)
	if err := ValidateGeometry(s, g); err != nil {
		return err
	}
	session := time.Now().Format("20060102-150405.000")
	root := filepath.Join(s.OutputDir, BufferDirName)
	dir := filepath.Join(root, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("can't write to the output folder: %v", err)
	}
	e.mu.Lock()
	if e.ownDirs == nil {
		e.ownDirs = map[string]bool{}
	}
	e.ownDirs[filepath.Clean(dir)] = true
	e.mu.Unlock()
	e.plat.HideDir(root)

	t0 := time.Now().Add(1200 * time.Millisecond)
	var audio []AudioSource
	var inputs []AudioInput
	addAudio := func(kind, id, title string) error {
		a := e.plat.NewAudio(kind, id)
		path, err := a.Open(t0)
		if err != nil {
			return fmt.Errorf("%s: %v", title, err)
		}
		audio = append(audio, a)
		inputs = append(inputs, AudioInput{Path: path, Title: title})
		return nil
	}
	closeAudio := func() {
		for _, a := range audio {
			a.Close()
		}
	}
	if s.SystemAudio {
		discord := EffectiveDiscord(s)
		kind := "loopback"
		if discord != "mix" {
			kind = "loopback-nodiscord" // everything except Discord
		}
		if err := addAudio(kind, s.SystemDeviceID, TitleGame); err != nil {
			closeAudio()
			return err
		}
		if discord == "separate" {
			if err := addAudio("discord", "", TitleDiscord); err != nil {
				closeAudio()
				return err
			}
		}
	}
	if s.Mic {
		if err := addAudio("mic", s.MicDeviceID, TitleMic); err != nil {
			closeAudio()
			return err
		}
	}

	args := CaptureArgs(s, src, inputs, dir, t0.UnixMicro(), e.videoTest)
	if e.encTest != nil {
		args = replaceEncoder(args, e.encTest)
	}
	log.Printf("starting capture: %s %s", e.ffmpeg, strings.Join(args, " "))
	cmd := e.plat.Command(e.ffmpeg, args...)
	stdin, _ := cmd.StdinPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		closeAudio()
		return fmt.Errorf("couldn't start ffmpeg: %v", err)
	}
	e.plat.AfterStart(cmd)

	buf := NewBuffer(dir, float64(s.ReplaySeconds))
	exited := make(chan struct{})
	e.mu.Lock()
	e.cmd, e.stdin, e.running, e.stopping = cmd, stdin, true, false
	e.exited = exited
	e.t0, e.buf, e.audio, e.source = t0, buf, audio, src
	e.srcKey = targetKey(s)
	e.stderr = nil
	e.errMsg = ""
	e.restartAt = time.Time{}
	recording := e.recording
	e.mu.Unlock()
	if recording {
		// Recording started while the pipeline was down: keep everything.
		buf.SetProtect(0)
		e.mu.Lock()
		e.recStream = 0
		e.mu.Unlock()
		e.writeRecMarker(dir)
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			log.Printf("ffmpeg: %s", line)
			e.mu.Lock()
			e.stderr = append(e.stderr, line)
			if len(e.stderr) > 40 {
				e.stderr = e.stderr[len(e.stderr)-40:]
			}
			e.mu.Unlock()
		}
	}()
	go func() {
		err := cmd.Wait()
		for _, a := range audio {
			a.Close()
		}
		buf.Refresh()
		e.mu.Lock()
		intentional := e.stopping
		uptime := time.Since(t0)
		lines := append([]string(nil), e.stderr...)
		e.running = false
		e.cmd, e.stdin = nil, nil
		e.buf = nil
		wasRecording := e.recording
		if !intentional {
			if uptime > 20*time.Second {
				e.failures = 0
			}
			e.failures++
			delay := 2 * time.Second
			if e.failures > 3 {
				delay = 10 * time.Second
			}
			e.restartAt = time.Now().Add(delay)
		}
		e.mu.Unlock()
		close(exited)
		if !intentional {
			msg := friendlyFFmpegError(lines, err)
			e.setErr(msg)
			if wasRecording {
				// Save what we have; a new pipeline will continue into a new file.
				e.mu.Lock()
				name, start := e.recName, e.recStream
				e.recStart = time.Now()
				e.mu.Unlock()
				e.finalize(buf, start, -1, name, "Recording", false, true)
			} else {
				go e.cleanupLater(buf.Dir())
			}
		}
		e.kick()
	}()
	e.push()
	return nil
}

// stopPipeline asks ffmpeg to finish cleanly (closing the last segment).
func (e *Engine) stopPipeline() *Buffer { return e.stopPipelineOpt(true) }

func (e *Engine) stopPipelineOpt(cleanup bool) *Buffer {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.stopping = true
	stdin, cmd, exited, buf := e.stdin, e.cmd, e.exited, e.buf
	e.mu.Unlock()
	if stdin != nil {
		_, _ = stdin.Write([]byte("q\n"))
		_ = stdin.Close()
	}
	select {
	case <-exited:
	case <-time.After(6 * time.Second):
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-exited
	}
	e.mu.Lock()
	recording := e.recording
	e.mu.Unlock()
	if cleanup && !recording {
		go e.cleanupLater(buf.Dir())
	}
	return buf
}

func (e *Engine) cleanupLater(dir string) {
	// Wait for any in-flight save that still reads from this folder.
	for i := 0; i < 600; i++ {
		e.mu.Lock()
		busy := e.finalizing > 0
		e.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = os.RemoveAll(dir)
}

// waitCoverage waits until a finished segment reaches stream time t.
func (e *Engine) waitCoverage(buf *Buffer, t float64, exited chan struct{}) {
	deadline := time.Now().Add(time.Duration(SegmentSeconds*2+4) * time.Second)
	for time.Now().Before(deadline) {
		buf.Refresh()
		if buf.Latest() >= t {
			return
		}
		select {
		case <-exited:
			buf.Refresh()
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// SaveReplay writes the last N seconds to a file.
func (e *Engine) SaveReplay() {
	e.mu.Lock()
	if !e.running || !e.replayOn {
		on := e.replayOn
		standby := on && !e.wantPipelineLocked()
		e.mu.Unlock()
		if standby {
			e.plat.Notify("info", "No game open", "Instant Replay starts when you open a game")
		} else if on {
			e.plat.Notify("warn", "Replay not ready yet", "The buffer is still starting up")
		} else {
			e.plat.Notify("warn", "Instant Replay is off", "Turn it on to save replays")
		}
		return
	}
	buf, exited := e.buf, e.exited
	reqT := e.streamNow()
	seconds := float64(e.settings.ReplaySeconds)
	e.finalizing++
	e.mu.Unlock()
	name := e.clipName()
	e.plat.Notify("busy", "Saving replay…", fmt.Sprintf("Last %s", humanSeconds(int(seconds))))
	go func() {
		e.waitCoverage(buf, reqT, exited)
		segs := buf.SelectLast(seconds, reqT+SegmentSeconds)
		e.mu.Lock()
		e.finalizing--
		e.mu.Unlock()
		e.writeClip(buf, segs, name, "Replay", true)
	}()
	e.push()
}

func (e *Engine) Recording() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recording
}

func (e *Engine) ToggleRecording() {
	if e.Recording() {
		e.StopRecording()
	} else {
		e.StartRecording()
	}
}

func (e *Engine) StartRecording() {
	name := e.clipName()
	e.mu.Lock()
	if e.recording {
		e.mu.Unlock()
		return
	}
	e.recording = true
	e.recStart = time.Now()
	e.recName = name
	e.failures = 0
	e.restartAt = time.Time{}
	if e.running {
		e.recStream = e.streamNow()
		e.buf.SetProtect(e.recStream - 0.01)
		dir := e.buf.Dir()
		e.mu.Unlock()
		e.writeRecMarker(dir)
	} else {
		e.mu.Unlock()
	}
	e.plat.Notify("rec", "Recording started", "Press "+HotkeyString(e.Settings().HotkeyRecord)+" to stop")
	e.kick()
	e.push()
}

func (e *Engine) StopRecording() {
	e.mu.Lock()
	if !e.recording {
		e.mu.Unlock()
		return
	}
	e.recording = false
	running := e.running
	buf, exited := e.buf, e.exited
	start := e.recStream
	name := e.recName
	stopT := -1.0
	if running {
		stopT = e.streamNow()
	}
	keepRunning := running && e.replayOn && (e.settings.ReplayWhen != "games" || e.game != nil)
	e.finalizing++
	e.mu.Unlock()
	e.plat.Notify("busy", "Saving recording…", "")
	go func() {
		if running && !keepRunning {
			e.stopPipelineOpt(false) // flushes the last segment
		} else if running {
			e.waitCoverage(buf, stopT, exited)
		}
		if buf != nil {
			e.finalize(buf, start, stopT, name, "Recording", !keepRunning, false)
		}
		e.mu.Lock()
		e.finalizing--
		e.mu.Unlock()
		e.push()
	}()
	e.push()
}

func (e *Engine) finalize(buf *Buffer, from, to float64, name, kind string, removeDir, recovered bool) {
	segs := buf.SelectRange(from, to)
	buf.SetProtect(-1)
	_ = os.Remove(filepath.Join(buf.Dir(), "recording.json"))
	if recovered {
		name += " (interrupted)"
	}
	e.writeClip(buf, segs, name, kind, false)
	if removeDir {
		go e.cleanupLater(buf.Dir())
	}
}

func (e *Engine) writeClip(buf *Buffer, segs []Segment, name, kind string, faststart bool) {
	e.mu.Lock()
	e.finalizing++
	s := e.settings
	e.mu.Unlock()
	defer func() {
		buf.Unlock(segs)
		e.mu.Lock()
		e.finalizing--
		e.mu.Unlock()
		e.push()
	}()
	if len(segs) == 0 {
		e.plat.Notify("warn", kind+" failed", "Nothing was captured yet")
		return
	}
	out, err := e.clipPath(s, name, kind, time.Now())
	if err != nil {
		e.plat.Notify("warn", kind+" failed", err.Error())
		e.setErr(err.Error())
		return
	}
	list := filepath.Join(buf.Dir(), fmt.Sprintf("concat_%d.ffconcat", time.Now().UnixNano()))
	if err := WriteConcatList(list, segs); err != nil {
		e.plat.Notify("warn", kind+" failed", err.Error())
		return
	}
	defer os.Remove(list)
	tmp := out + ".part"
	cmd := e.plat.Command(e.ffmpeg, ConcatArgs(list, tmp, s, faststart)...)
	outb, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(string(outb))
		log.Printf("concat failed: %v %s", err, msg)
		e.plat.Notify("warn", kind+" failed", lastLine(msg))
		e.setErr(kind + " couldn't be saved: " + lastLine(msg))
		return
	}
	if err := os.Rename(tmp, out); err != nil {
		e.plat.Notify("warn", kind+" failed", err.Error())
		return
	}
	var size float64
	if fi, err := os.Stat(out); err == nil {
		size = float64(fi.Size()) / (1 << 20)
	}
	dur := segs[len(segs)-1].End - segs[0].Start
	e.mu.Lock()
	e.errMsg = ""
	e.mu.Unlock()
	e.RefreshClips()
	e.plat.Notify("ok", kind+" saved", fmt.Sprintf("%s, %s, %.0f MB", name, humanSeconds(int(dur+0.5)), size))
}

var unsafeName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]+`)

func sanitizeName(n string) string {
	n = unsafeName.ReplaceAllString(n, " ")
	n = strings.Join(strings.Fields(n), " ")
	n = strings.Trim(n, ". ")
	if len([]rune(n)) > 48 {
		n = strings.TrimSpace(string([]rune(n)[:48]))
	}
	if n == "" {
		n = "Desktop"
	}
	return n
}

func (e *Engine) clipPath(s Settings, name, kind string, t time.Time) (string, error) {
	name = sanitizeName(name)
	dir := s.OutputDir
	if s.FolderPerGame {
		dir = filepath.Join(dir, strings.TrimSuffix(name, " (interrupted)"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("can't create %s: %v", dir, err)
	}
	base := fmt.Sprintf("%s %s %s", name, t.Format("2006-01-02 15-04-05"), kind)
	p := filepath.Join(dir, base+"."+s.Container)
	for i := 2; fileExists(p); i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d).%s", base, i, s.Container))
	}
	return p, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

type recMarker struct {
	Name  string  `json:"name"`
	Start float64 `json:"start"`
}

func (e *Engine) writeRecMarker(dir string) {
	e.mu.Lock()
	m := recMarker{Name: e.recName, Start: e.recStream}
	e.mu.Unlock()
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "recording.json"), b, 0o644)
}

// RecoverStale turns recordings left behind by a crash into files, then
// clears old buffer folders. Call once at startup.
func (e *Engine) RecoverStale() {
	s := e.Settings()
	// Also check the folder names used before the app was renamed.
	for _, name := range []string{BufferDirName, ".kapture-buffer", ".instareplay-buffer", ".greenlight-buffer"} {
		e.recoverIn(filepath.Join(s.OutputDir, name))
		if name != BufferDirName {
			_ = os.Remove(filepath.Join(s.OutputDir, name)) // only if empty
		}
	}
}

// BufferDirName is the hidden folder (inside the clips folder) that holds
// the rolling replay buffer.
const BufferDirName = ".rekapture-buffer"

func (e *Engine) recoverIn(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, en := range entries {
		dir := filepath.Join(root, en.Name())
		e.mu.Lock()
		mine := e.ownDirs[filepath.Clean(dir)]
		e.mu.Unlock()
		if mine {
			continue // belongs to this run, not a leftover
		}
		if !en.IsDir() {
			_ = os.Remove(dir)
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, "recording.json")); err == nil {
			var m recMarker
			if json.Unmarshal(b, &m) == nil {
				buf := NewBuffer(dir, 1e9)
				buf.Refresh()
				if buf.Latest() > 0 {
					log.Printf("recovering interrupted recording in %s", dir)
					e.finalize(buf, m.Start, -1, m.Name, "Recording", false, true)
				}
			}
		}
		_ = os.RemoveAll(dir)
	}
}

// Shutdown stops everything, finishing any recording first.
func (e *Engine) Shutdown() { e.shutOnce.Do(e.shutdown) }

func (e *Engine) shutdown() {
	if e.Recording() {
		e.StopRecording()
	}
	e.mu.Lock()
	e.replayOn = false
	e.mu.Unlock()
	e.stopPipeline()
	for i := 0; i < 1200; i++ { // up to 10 min for a long recording to finish writing
		e.mu.Lock()
		busy := e.finalizing > 0
		e.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	close(e.quit)
}

func friendlyFFmpegError(lines []string, err error) string {
	all := strings.ToLower(strings.Join(lines, "\n"))
	switch {
	case strings.Contains(all, "no capable devices found") || strings.Contains(all, "cannot load nvcuda") ||
		strings.Contains(all, "openencodesessionex failed") || strings.Contains(all, "driver does not support"):
		return "NVENC isn't available. Make sure the NVIDIA driver is up to date and no other app is using all encoder sessions."
	case strings.Contains(all, "no window found") || strings.Contains(all, "could not find") && strings.Contains(all, "window"):
		return "The selected window isn't open. Waiting for it…"
	case strings.Contains(all, "eof") && strings.Contains(all, "gfxcapture"):
		return "The captured window or monitor went away. Restarting…"
	}
	if len(lines) > 0 {
		return "Capture stopped: " + bestErrorLines(lines)
	}
	if err != nil {
		return "Capture stopped: " + err.Error()
	}
	return "Capture stopped unexpectedly"
}

// bestErrorLines picks the ffmpeg lines that actually explain a failure
// (the generic "Error opening input files" line alone says nothing useful).
func bestErrorLines(lines []string) string {
	var picked []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		low := strings.ToLower(t)
		if t == "" || strings.Contains(low, "guessed channel layout") {
			continue
		}
		if strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "invalid") ||
			strings.Contains(low, "cannot") || strings.Contains(low, "could not") || strings.Contains(low, "unable") {
			if i := strings.Index(t, "] "); strings.HasPrefix(t, "[") && i > 0 {
				t = t[i+2:]
			}
			if len(picked) == 0 || picked[len(picked)-1] != t {
				picked = append(picked, t)
			}
		}
	}
	if len(picked) == 0 {
		return lastLine(strings.Join(lines, "\n"))
	}
	if len(picked) > 3 {
		picked = picked[len(picked)-3:]
	}
	out := strings.Join(picked, " — ")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(ls) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(ls[i]); t != "" {
			if len(t) > 200 {
				t = t[:200]
			}
			return t
		}
	}
	return "unknown error"
}

func humanSeconds(s int) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s%60 == 0 {
		return fmt.Sprintf("%d min", s/60)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// replaceEncoder swaps the NVENC args for a software encoder (tests only).
func replaceEncoder(args []string, enc []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "-c:v" {
			out = append(out, enc...)
			// skip until the next arg that isn't an nvenc option
			j := i + 2
			for j < len(args) && isNvencOpt(args[j]) {
				j += 2
			}
			i = j - 1
			continue
		}
		if args[i] == "-profile:v" || args[i] == "-forced-idr" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func isNvencOpt(a string) bool {
	switch a {
	case "-preset", "-tune", "-rc", "-b:v", "-maxrate", "-bufsize", "-cq", "-forced-idr", "-profile:v":
		return true
	}
	return false
}
