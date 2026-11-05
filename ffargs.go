package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// SegmentSeconds is the length of each replay-buffer chunk. Keyframes are
// placed exactly on these boundaries so chunks can be joined without
// re-encoding. Smaller = finer replay trimming, slightly higher bitrate.
const SegmentSeconds = 2

// CaptureSource is a resolved, concrete thing to capture.
type CaptureSource struct {
	Kind   string  // "monitor" | "window" | "test"
	Handle uintptr // HMONITOR or HWND
	Width  int     // native pixel size (0 if unknown)
	Height int
	Label  string // human readable, e.g. "DISPLAY1 2560×1440" or "cs2.exe"
}

// Geometry is the final output canvas and how the source is fitted into it.
type Geometry struct {
	Width, Height              int    // 0 = use source size
	ResizeMode                 string // gfxcapture resize_mode: crop|scale|scale_aspect
	CropL, CropT, CropR, CropB int
}

func even(v float64) int {
	n := int(math.Round(v))
	if n%2 != 0 {
		n--
	}
	if n < 2 {
		n = 2
	}
	return n
}

func parseAspect(a string) (float64, bool) {
	parts := strings.Split(a, ":")
	if len(parts) != 2 {
		return 0, false
	}
	w, err1 := strconv.ParseFloat(parts[0], 64)
	h, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, false
	}
	return w / h, true
}

// ComputeGeometry turns the user's resolution / aspect choices into a canvas.
// srcW/srcH may be 0 when the source size is unknown.
func ComputeGeometry(s Settings, srcW, srcH int) Geometry {
	g := Geometry{ResizeMode: "scale_aspect"}
	switch s.AspectFit {
	case "stretch":
		g.ResizeMode = "scale"
	case "crop":
		g.ResizeMode = "scale"
	}

	srcAspect := 16.0 / 9.0
	if srcW > 0 && srcH > 0 {
		srcAspect = float64(srcW) / float64(srcH)
	}
	target, ok := parseAspect(s.Aspect)
	if !ok {
		target = srcAspect
	}

	if s.Resolution == "custom" {
		g.Width, g.Height = s.CustomWidth&^1, s.CustomHeight&^1
		target = float64(g.Width) / float64(g.Height)
	} else if s.Resolution == "native" {
		if srcW == 0 || srcH == 0 {
			// Unknown source size: let gfxcapture use the source, rounded to even.
			g.Width, g.Height = -2, -2
			if s.Aspect != "source" {
				g.Width, g.Height = 1920, even(1920/target)
			}
			g.ResizeMode = "scale_aspect"
			return g
		}
		if s.Aspect == "source" {
			g.Width, g.Height = srcW&^1, srcH&^1
		} else if target > srcAspect {
			g.Width, g.Height = srcW&^1, even(float64(srcW)/target)
		} else {
			g.Width, g.Height = even(float64(srcH)*target), srcH&^1
		}
	} else {
		p, _ := strconv.Atoi(s.Resolution) // 2160, 1440, 1080 ...
		if target >= 1 {
			g.Height, g.Width = p, even(float64(p)*target)
		} else { // vertical: the preset number is the width (1080 -> 1080x1920)
			g.Width, g.Height = p, even(float64(p)/target)
		}
	}

	if s.AspectFit == "crop" && srcW > 0 && srcH > 0 {
		if srcAspect > target+0.001 {
			keep := int(math.Round(float64(srcH) * target))
			cut := srcW - keep
			g.CropL, g.CropR = cut/2, cut-cut/2
		} else if srcAspect < target-0.001 {
			keep := int(math.Round(float64(srcW) / target))
			cut := srcH - keep
			g.CropT, g.CropB = cut/2, cut-cut/2
		}
	}
	return g
}

// EncoderName maps the codec setting to the ffmpeg NVENC encoder.
func EncoderName(codec string) string {
	switch codec {
	case "h264":
		return "h264_nvenc"
	case "av1":
		return "av1_nvenc"
	}
	return "hevc_nvenc"
}

// ValidateGeometry rejects combinations NVENC cannot encode.
func ValidateGeometry(s Settings, g Geometry) error {
	maxW := 8192
	if s.Codec == "h264" {
		maxW = 4096
	}
	if g.Width > maxW || g.Height > maxW {
		return fmt.Errorf("%s supports up to %d pixels per side; pick a lower resolution or switch to HEVC/AV1", strings.ToUpper(s.Codec), maxW)
	}
	return nil
}

// AudioInput is one raw float32 48 kHz stereo stream fed to ffmpeg.
type AudioInput struct {
	Path  string // named pipe / fifo
	Title string // track title in the file
}

// CaptureArgs builds the full ffmpeg command for the capture pipeline.
//
// Video: gfxcapture (Windows.Graphics.Capture) produces D3D11 GPU frames
// which go straight into NVENC — the frames never touch the CPU.
// Timestamps for both video and audio are anchored to the same wall-clock
// instant t0 (microseconds since the Unix epoch), which keeps them in sync.
// videoSource overrides gfxcapture (used by tests).
func CaptureArgs(s Settings, src CaptureSource, audio []AudioInput, segDir string, t0us int64, videoSource string) []string {

	args := []string{"-hide_banner", "-nostats", "-loglevel", "warning", "-y"}
	for _, a := range audio {
		// No -thread_queue_size: since FFmpeg 8 it's an output-only option and
		// FFmpeg rejects it on inputs ("Error opening input files: Invalid argument").
		args = append(args, "-f", "f32le", "-ar", "48000", "-ch_layout", "stereo", "-i", a.Path)
	}

	var src0 string
	if videoSource != "" {
		src0 = videoSource
	} else {
		// No repeated frames from gfxcapture: it stamps repeats on a different
		// clock than real frames, and with a game delivering frames about as
		// fast as the repeat deadline, about half of all frames became repeats
		// (the 2.2.x stutter). The fps filter below fills real gaps instead,
		// using the true capture timestamps.
		src0 = GfxSource(s, src, CaptureRate(s.FPS), false)
	}
	// Keep the capture timestamps (exact moment Windows captured each frame)
	// and shift them once so the first frame lines up with the wall clock
	// relative to t0, which is what the audio is aligned to. Re-stamping
	// every frame with the time it reaches the filter adds jitter that the
	// fps filter turns into dropped/duplicated frames (visible stutter).
	// st(0)/ld(0) keep the offset computed on the first frame.
	vchain := fmt.Sprintf("%s,setpts='PTS+if(eq(N\\,0)\\,st(0\\,(RTCTIME-%d)/(TB*1000000)-PTS)\\,ld(0))',trim=start=0,fps=%d[v]", src0, t0us, s.FPS)
	graph := []string{vchain}

	maps := []string{"-map", "[v]"}
	tracks, agraph := AudioTrackLayout(s.AudioTracks, audio)
	graph = append(graph, agraph...)
	for _, t := range tracks {
		maps = append(maps, "-map", t.Label)
	}

	// Video and audio run in separate filter graphs (separate threads), so
	// while the capture waits for Windows to deliver a frame (a still screen
	// delivers none) audio keeps flowing.
	args = append(args, "-filter_complex", vchain)
	if len(graph) > 1 {
		args = append(args, "-filter_complex", strings.Join(graph[1:], ";"))
	}
	args = append(args, maps...)
	args = append(args, VideoEncoderArgs(s)...)
	if len(tracks) > 0 {
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", s.AudioKbps), "-ar", "48000")
		for i, t := range tracks {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "title="+t.Title)
		}
	}
	args = append(args,
		"-max_muxing_queue_size", "4096",
		"-f", "segment",
		"-segment_time", strconv.Itoa(SegmentSeconds),
		"-segment_format", "matroska",
		"-segment_list", joinPath(segDir, "list.csv"),
		"-segment_list_type", "csv",
		"-reset_timestamps", "1", // each chunk starts at 0 so they concatenate cleanly
		joinPath(segDir, "seg_%06d.mkv"),
	)
	return args
}

// CaptureRate is how often Windows is asked for frames for an output rate.
func CaptureRate(fps int) int {
	r := fps * 2
	if r > 500 {
		r = 500
	}
	return r
}

func VideoEncoderArgs(s Settings) []string {
	a := []string{"-c:v", EncoderName(s.Codec), "-preset", fmt.Sprintf("p%d", s.Preset), "-tune", "hq"}
	br := s.BitrateMbps
	switch s.RateControl {
	case "cbr":
		a = append(a, "-rc", "cbr", "-b:v", fmt.Sprintf("%dM", br), "-maxrate", fmt.Sprintf("%dM", br), "-bufsize", fmt.Sprintf("%dM", br*2))
	case "cq":
		a = append(a, "-rc", "vbr", "-cq", strconv.Itoa(s.CQ), "-b:v", "0", "-maxrate", fmt.Sprintf("%dM", br), "-bufsize", fmt.Sprintf("%dM", br*2))
	default:
		a = append(a, "-rc", "vbr", "-b:v", fmt.Sprintf("%dM", br), "-maxrate", fmt.Sprintf("%dM", br*3/2), "-bufsize", fmt.Sprintf("%dM", br*2))
	}
	gop := s.FPS * SegmentSeconds
	a = append(a, "-g", strconv.Itoa(gop), "-forced-idr", "1",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", SegmentSeconds))
	if s.Codec == "h264" {
		a = append(a, "-profile:v", "high")
	}
	return a
}

// ConcatArgs joins finished segments into the final file without re-encoding.
func ConcatArgs(listFile, out string, s Settings, faststart bool) []string {
	a := []string{"-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", listFile,
		"-map", "0", "-c", "copy", "-avoid_negative_ts", "make_zero"}
	if strings.HasSuffix(strings.ToLower(out), ".mp4") || strings.HasSuffix(strings.ToLower(out), ".mp4.part") {
		if s.Codec == "hevc" {
			a = append(a, "-tag:v", "hvc1")
		}
		if faststart {
			a = append(a, "-movflags", "+faststart")
		}
		a = append(a, "-f", "mp4")
	} else {
		a = append(a, "-f", "matroska")
	}
	return append(a, out)
}

func boolNum(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func joinPath(dir, name string) string {
	if strings.Contains(dir, "\\") {
		return strings.TrimRight(dir, "\\") + "\\" + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

// DescribeVideo is the one-line summary shown in the UI.
func DescribeVideo(s Settings, src CaptureSource) string {
	g := ComputeGeometry(s, src.Width, src.Height)
	res := "source size"
	if g.Width > 0 && g.Height > 0 {
		res = fmt.Sprintf("%d×%d", g.Width, g.Height)
	}
	rc := fmt.Sprintf("%d Mbps %s", s.BitrateMbps, strings.ToUpper(s.RateControl))
	if s.RateControl == "cq" {
		rc = fmt.Sprintf("CQ %d", s.CQ)
	}
	return fmt.Sprintf("%s, %s, %d fps, %s", strings.ToUpper(map[string]string{"h264": "H.264", "hevc": "HEVC", "av1": "AV1"}[s.Codec]), res, s.FPS, rc)
}

// GfxSource builds the gfxcapture source. dup=false turns off repeating the
// last frame, so only frames Windows really delivered come out (capture test).
func GfxSource(s Settings, src CaptureSource, rate int, dup bool) string {
	g := ComputeGeometry(s, src.Width, src.Height)
	opts := []string{}
	if src.Kind == "window" {
		opts = append(opts, fmt.Sprintf("hwnd=%d", src.Handle))
	} else {
		opts = append(opts, fmt.Sprintf("hmonitor=%d", src.Handle))
	}
	// Capture at twice the output rate. Windows only delivers frames on
	// screen refreshes, so capping capture at exactly the output rate
	// makes it dip below it (e.g. 240 Hz screen: 16.7 ms cap often waits
	// a refresh longer), which forces duplicated frames = stutter.
	opts = append(opts, fmt.Sprintf("max_framerate=%d", rate))
	opts = append(opts, "capture_cursor="+boolNum(s.ShowCursor))
	if g.Width != 0 || g.Height != 0 {
		opts = append(opts, fmt.Sprintf("width=%d", g.Width), fmt.Sprintf("height=%d", g.Height), "resize_mode="+g.ResizeMode, "scale_mode=bicubic")
	}
	if g.CropL > 0 {
		opts = append(opts, fmt.Sprintf("crop_left=%d", g.CropL))
	}
	if g.CropR > 0 {
		opts = append(opts, fmt.Sprintf("crop_right=%d", g.CropR))
	}
	if g.CropT > 0 {
		opts = append(opts, fmt.Sprintf("crop_top=%d", g.CropT))
	}
	if g.CropB > 0 {
		opts = append(opts, fmt.Sprintf("crop_bottom=%d", g.CropB))
	}
	if !dup {
		opts = append(opts, "dup_frames=0")
	}
	return "gfxcapture=" + strings.Join(opts, ":")
}

// OutTrack is one audio track of the output file: an ffmpeg map label and
// the title stored with it.
type OutTrack struct{ Label, Title string }

// AudioTrackLayout lays out the audio tracks for any number of inputs (game,
// Discord, mic): one mixed track, each input on its own, or both (the mix
// first, so players that only play track 1 still get everything).
func AudioTrackLayout(mode string, audio []AudioInput) ([]OutTrack, []string) {
	n := len(audio)
	if n == 0 {
		return nil, nil
	}
	if n == 1 {
		return []OutTrack{{"0:a", audio[0].Title}}, nil
	}
	var titles []string
	for _, a := range audio {
		titles = append(titles, a.Title)
	}
	mixTitle := MixTitle(titles)
	amix := fmt.Sprintf("amix=inputs=%d:duration=longest:dropout_transition=0:normalize=0[amix]", n)
	var graph []string
	var tracks []OutTrack
	switch mode {
	case "mixed":
		in := ""
		for i := range audio {
			in += fmt.Sprintf("[%d:a]", i)
		}
		graph = append(graph, in+amix)
		tracks = append(tracks, OutTrack{"[amix]", mixTitle})
	case "separate":
		for i, a := range audio {
			tracks = append(tracks, OutTrack{fmt.Sprintf("%d:a", i), a.Title})
		}
	default: // both
		in := ""
		var own []OutTrack
		for i, a := range audio {
			graph = append(graph, fmt.Sprintf("[%d:a]asplit=2[a%d][a%dm]", i, i, i))
			in += fmt.Sprintf("[a%dm]", i)
			own = append(own, OutTrack{fmt.Sprintf("[a%d]", i), a.Title})
		}
		graph = append(graph, in+amix)
		tracks = append(append(tracks, OutTrack{"[amix]", mixTitle}), own...)
	}
	return tracks, graph
}
