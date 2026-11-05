package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ---------- probing ----------

type AudioTrack struct {
	Index int    `json:"index"` // audio stream number (0:a:N)
	Title string `json:"title"`
	Codec string `json:"codec"`
}

type ClipInfo struct {
	Duration   float64      `json:"duration"`
	Width      int          `json:"width"`
	Height     int          `json:"height"`
	FPS        float64      `json:"fps"`
	VideoCodec string       `json:"videoCodec"`
	Audio      []AudioTrack `json:"audio"`
	// Mixable: the file has ReKapture's layout of a mixed track plus the
	// single-source tracks it was made from (game, Discord, mic), so the mix
	// can be rebuilt from those after volume changes.
	Mixable bool  `json:"mixable"`
	MixIdx  int   `json:"-"`
	Parts   []int `json:"-"` // audio indexes of the tracks the mix is made from
}

var (
	reDuration = regexp.MustCompile(`Duration: (\d+):(\d+):(\d+(?:\.\d+)?)`)
	reStream   = regexp.MustCompile(`^\s*Stream #0:\d+[^:]*: (Video|Audio|Subtitle|Data|Attachment): (\w+)(.*)$`)
	reSize     = regexp.MustCompile(`, (\d{2,5})x(\d{2,5})[ ,\[]`)
	reFPS      = regexp.MustCompile(`, ([\d.]+) fps`)
	reMetaKV   = regexp.MustCompile(`^\s+(title|name|handler_name)\s*:\s*(.+)$`)
)

// ParseProbe reads the stream summary ffmpeg prints for "ffmpeg -i file".
// (We ship ffmpeg.exe only, so this stands in for ffprobe.)
func ParseProbe(out string) (ClipInfo, error) {
	var ci ClipInfo
	ci.MixIdx = -1
	if m := reDuration.FindStringSubmatch(out); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		s, _ := strconv.ParseFloat(m[3], 64)
		ci.Duration = float64(h*3600+mi*60) + s
	}
	cur := ""
	audioN := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := reStream.FindStringSubmatch(line); m != nil {
			cur = m[1]
			switch cur {
			case "Video":
				if ci.VideoCodec == "" && !strings.Contains(m[3], "attached pic") {
					ci.VideoCodec = m[2]
					if s := reSize.FindStringSubmatch(m[3] + " "); s != nil {
						ci.Width, _ = strconv.Atoi(s[1])
						ci.Height, _ = strconv.Atoi(s[2])
					}
					if f := reFPS.FindStringSubmatch(m[3]); f != nil {
						ci.FPS, _ = strconv.ParseFloat(f[1], 64)
					}
				} else {
					cur = "" // ignore extra video streams' metadata
				}
			case "Audio":
				ci.Audio = append(ci.Audio, AudioTrack{Index: audioN, Codec: m[2], Title: fmt.Sprintf("Track %d", audioN+1)})
				audioN++
			}
			continue
		}
		if cur == "Audio" {
			if m := reMetaKV.FindStringSubmatch(line); m != nil {
				v := strings.TrimSpace(m[2])
				t := &ci.Audio[len(ci.Audio)-1]
				generic := strings.HasPrefix(t.Title, "Track ")
				if m[1] != "handler_name" || (generic && v != "SoundHandler" && v != "Core Media Audio") {
					t.Title = v
				}
			}
		}
	}
	if ci.VideoCodec == "" {
		return ci, errors.New("this file has no video")
	}
	for _, a := range ci.Audio {
		switch {
		case isMixTitle(a.Title) && ci.MixIdx < 0:
			ci.MixIdx = a.Index
		case sourceTitles[a.Title] != "":
			ci.Parts = append(ci.Parts, a.Index)
		}
	}
	ci.Mixable = ci.MixIdx >= 0 && len(ci.Parts) >= 2
	return ci, nil
}

func ProbeClip(ffmpeg, path string, command func(string, ...string) *exec.Cmd) (ClipInfo, error) {
	out, _ := command(ffmpeg, "-hide_banner", "-i", path).CombinedOutput() // exits 1 ("no output"), that's expected
	ci, err := ParseProbe(string(out))
	if err != nil && strings.Contains(string(out), "No such file") {
		return ci, errors.New("the clip was moved or deleted")
	}
	return ci, err
}

// ---------- editing ----------

type TrackEdit struct {
	Index  int     `json:"index"`
	Volume float64 `json:"volume"` // 1 = unchanged, 0..2
	Mute   bool    `json:"mute"`
}

type EditSpec struct {
	Path    string      `json:"path"`
	Start   float64     `json:"start"`
	End     float64     `json:"end"`
	Exact   bool        `json:"exact"`   // re-encode video for frame-accurate cuts
	Replace bool        `json:"replace"` // overwrite the original instead of saving a copy
	Tracks  []TrackEdit `json:"tracks"`
}

func (t TrackEdit) gain() float64 {
	if t.Mute {
		return 0
	}
	return math.Max(0, math.Min(2, t.Volume))
}

func audioChanged(spec EditSpec) bool {
	for _, t := range spec.Tracks {
		if t.Mute || math.Abs(t.Volume-1) > 0.005 {
			return true
		}
	}
	return false
}

func fmtSec(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

// EditArgs builds the ffmpeg command for an edit. videoEnc is used when
// re-encoding (Exact mode).
func EditArgs(info ClipInfo, spec EditSpec, out string, videoEnc []string) []string {
	dur := spec.End - spec.Start
	a := []string{"-hide_banner", "-nostats", "-loglevel", "error", "-y", "-progress", "pipe:1"}
	// Seeking before -i is fast; with re-encoding it's also frame accurate.
	if spec.Exact && len(videoEnc) > 1 && strings.HasSuffix(videoEnc[1], "_nvenc") {
		// Decode on the GPU too (FFmpeg falls back to the CPU on its own if it can't).
		a = append(a, "-hwaccel", "auto")
	}
	if spec.Start > 0.001 {
		a = append(a, "-ss", fmtSec(spec.Start))
	}
	a = append(a, "-i", spec.Path, "-t", fmtSec(dur), "-map_metadata", "0", "-map", "0:v:0")

	gains := map[int]float64{}
	for _, t := range spec.Tracks {
		gains[t.Index] = t.gain()
	}
	g := func(i int) float64 {
		if v, ok := gains[i]; ok {
			return v
		}
		return 1
	}
	changed := audioChanged(spec)
	var graph []string
	var maps []string
	var titles []string
	if !changed {
		for _, t := range info.Audio {
			maps = append(maps, fmt.Sprintf("0:a:%d", t.Index))
			titles = append(titles, t.Title)
		}
	} else if info.Mixable {
		// Rebuild the mixed track from the adjusted single-source tracks.
		part := map[int]bool{}
		var mixIn string
		for _, i := range info.Parts {
			part[i] = true
			graph = append(graph, fmt.Sprintf("[0:a:%d]volume=%.3f,asplit=2[p%d][q%d]", i, g(i), i, i))
			mixIn += fmt.Sprintf("[q%d]", i)
		}
		graph = append(graph, fmt.Sprintf("%samix=inputs=%d:duration=longest:dropout_transition=0:normalize=0[mix]", mixIn, len(info.Parts)))
		for _, t := range info.Audio {
			switch {
			case t.Index == info.MixIdx:
				maps = append(maps, "[mix]")
			case part[t.Index]:
				maps = append(maps, fmt.Sprintf("[p%d]", t.Index))
			default:
				label := fmt.Sprintf("[x%d]", t.Index)
				graph = append(graph, fmt.Sprintf("[0:a:%d]volume=%.3f%s", t.Index, g(t.Index), label))
				maps = append(maps, label)
			}
			titles = append(titles, t.Title)
		}
	} else {
		for _, t := range info.Audio {
			label := fmt.Sprintf("[a%d]", t.Index)
			graph = append(graph, fmt.Sprintf("[0:a:%d]volume=%.3f%s", t.Index, g(t.Index), label))
			maps = append(maps, label)
			titles = append(titles, t.Title)
		}
	}
	if len(graph) > 0 {
		a = append(a, "-filter_complex", strings.Join(graph, ";"))
	}
	for _, m := range maps {
		a = append(a, "-map", m)
	}
	if spec.Exact {
		a = append(a, videoEnc...)
	} else {
		a = append(a, "-c:v", "copy")
	}
	if len(maps) > 0 {
		// Exact mode re-encodes audio too: FFmpeg 8 doesn't apply the input
		// seek to copied audio when the video is re-encoded (audio would start
		// at 0 while video starts at the cut).
		if changed || spec.Exact {
			a = append(a, "-c:a", "aac", "-b:a", "192k")
		} else {
			a = append(a, "-c:a", "copy")
		}
		for i, t := range titles {
			a = append(a, fmt.Sprintf("-metadata:s:a:%d", i), "title="+t)
		}
	}
	a = append(a, "-avoid_negative_ts", "make_zero")
	ext := strings.ToLower(filepath.Ext(strings.TrimSuffix(out, ".part")))
	if ext == ".mp4" {
		if info.VideoCodec == "hevc" {
			a = append(a, "-tag:v", "hvc1")
		}
		a = append(a, "-movflags", "+faststart", "-f", "mp4")
	} else {
		a = append(a, "-f", "matroska")
	}
	return append(a, out)
}

// VideoEncoderFor picks a re-encoder matching the source codec, on NVENC.
func VideoEncoderFor(codec string, nvenc bool) []string {
	if !nvenc {
		return []string{"-c:v", "libopenh264", "-b:v", "20M", "-pix_fmt", "yuv420p"}
	}
	enc := "h264_nvenc"
	switch codec {
	case "hevc":
		enc = "hevc_nvenc"
	case "av1":
		enc = "av1_nvenc"
	}
	return []string{"-c:v", enc, "-preset", "p6", "-tune", "hq", "-rc", "vbr", "-cq", "18", "-b:v", "0", "-pix_fmt", "yuv420p"}
}

// EditedPath is where a "save as new clip" goes: "<name> (edited).mp4".
func EditedPath(src string) string {
	ext := filepath.Ext(src)
	base := strings.TrimSuffix(src, ext)
	base = regexp.MustCompile(` \(edited(?: \d+)?\)$`).ReplaceAllString(base, "")
	p := base + " (edited)" + ext
	for i := 2; fileExists(p); i++ {
		p = fmt.Sprintf("%s (edited %d)%s", base, i, ext)
	}
	return p
}

// Editor runs one edit at a time, reporting progress and allowing cancel.
type Editor struct {
	FFmpeg  string
	Command func(string, ...string) *exec.Cmd
	NVENC   func() bool

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (ed *Editor) Busy() bool {
	ed.mu.Lock()
	defer ed.mu.Unlock()
	return ed.cancel != nil
}

func (ed *Editor) Cancel() {
	ed.mu.Lock()
	c := ed.cancel
	ed.mu.Unlock()
	if c != nil {
		c()
	}
}

var ErrCanceled = errors.New("canceled")

// Run performs the edit and returns the saved file's path.
func (ed *Editor) Run(spec EditSpec, progress func(float64)) (string, error) {
	ed.mu.Lock()
	if ed.cancel != nil {
		ed.mu.Unlock()
		return "", errors.New("another edit is still saving")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ed.cancel = cancel
	ed.mu.Unlock()
	defer func() {
		ed.mu.Lock()
		ed.cancel = nil
		ed.mu.Unlock()
		cancel()
	}()

	info, err := ProbeClip(ed.FFmpeg, spec.Path, ed.Command)
	if err != nil {
		return "", err
	}
	if spec.End <= 0 || spec.End > info.Duration {
		spec.End = info.Duration
	}
	if spec.Start < 0 {
		spec.Start = 0
	}
	if spec.End-spec.Start < 0.2 {
		return "", errors.New("the selection is too short; pick at least a fraction of a second")
	}
	final := EditedPath(spec.Path)
	if spec.Replace {
		final = spec.Path
	}
	tmp := strings.TrimSuffix(final, filepath.Ext(final)) + ".editing" + filepath.Ext(final) + ".part"
	defer os.Remove(tmp)

	tryEnc := [][]string{nil}
	if spec.Exact {
		tryEnc = [][]string{VideoEncoderFor(info.VideoCodec, ed.NVENC == nil || ed.NVENC())}
		if ed.NVENC == nil || ed.NVENC() {
			tryEnc = append(tryEnc, VideoEncoderFor(info.VideoCodec, false))
		}
	}
	var lastErr error
	for _, enc := range tryEnc {
		args := EditArgs(info, spec, tmp, enc)
		lastErr = ed.run(ctx, args, spec.End-spec.Start, progress)
		if lastErr == nil || errors.Is(lastErr, ErrCanceled) {
			break
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	if spec.Replace {
		// Keep the original until the new file is safely in place.
		bak := spec.Path + ".bak"
		if err := os.Rename(spec.Path, bak); err != nil {
			return "", fmt.Errorf("couldn't replace the original (is it open in a player?): %v", err)
		}
		if err := os.Rename(tmp, final); err != nil {
			os.Rename(bak, spec.Path)
			return "", fmt.Errorf("couldn't replace the original: %v", err)
		}
		os.Remove(bak)
	} else if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	progress(1)
	return final, nil
}

func (ed *Editor) run(ctx context.Context, args []string, dur float64, progress func(float64)) error {
	err := runFFmpegProgress(ctx, ed.Command, ed.FFmpeg, args, dur, progress)
	if err != nil && !errors.Is(err, ErrCanceled) {
		return fmt.Errorf("couldn't save the edit: %s", strings.TrimPrefix(err.Error(), "ffmpeg: "))
	}
	return err
}

// runFFmpegProgress runs ffmpeg (with -progress pipe:1 in args), reports
// 0..0.99 progress over dur seconds, and stops if ctx is canceled.
func runFFmpegProgress(ctx context.Context, command func(string, ...string) *exec.Cmd, ffmpeg string, args []string, dur float64, progress func(float64)) error {
	cmd := command(ffmpeg, args...)
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "out_time_us=") || strings.HasPrefix(line, "out_time_ms=") {
			if us, err := strconv.ParseFloat(strings.SplitN(line, "=", 2)[1], 64); err == nil && dur > 0 {
				p := us / 1e6 / dur
				if p >= 0 && p < 1 {
					progress(p * 0.99)
				}
			}
		}
	}
	err := cmd.Wait()
	close(done)
	if ctx.Err() != nil {
		return ErrCanceled
	}
	if err != nil {
		return fmt.Errorf("ffmpeg: %s", lastLine(stderr.String()))
	}
	return nil
}

// FilmstripArgs makes one wide JPEG of n evenly spaced keyframes for the timeline.
func FilmstripArgs(src, out string, duration float64, n int) []string {
	if duration <= 0 {
		duration = 60
	}
	return []string{"-hide_banner", "-loglevel", "error", "-y", "-skip_frame", "nokey", "-i", src,
		"-vf", fmt.Sprintf("fps=%d/%s,scale=-2:72,tile=%dx1", n, fmtSec(duration), n),
		"-frames:v", "1", "-q:v", "5", "-f", "image2", out}
}
