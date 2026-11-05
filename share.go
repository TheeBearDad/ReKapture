package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- webhooks ----------

// Webhook is a Discord channel the user can post clips to.
type Webhook struct {
	Name      string `json:"name"`      // what the user calls it, e.g. "#clips (Friends)"
	URL       string `json:"url"`       // secret: anyone with it can post to the channel
	GuildID   string `json:"guildId"`   // filled in by CheckWebhook
	ChannelID string `json:"channelId"` // filled in by CheckWebhook
}

var webhookURLRe = regexp.MustCompile(`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/api(?:/v\d+)?/webhooks/(\d+)/([\w-]+)/?$`)

// allowTestWebhooks lets tests point webhooks at a local fake server.
var allowTestWebhooks = false

// NormalizeWebhookURL checks that a pasted link is a Discord webhook.
func NormalizeWebhookURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if allowTestWebhooks && strings.HasPrefix(u, "http://127.0.0.1:") {
		return u, nil
	}
	if !webhookURLRe.MatchString(u) {
		return "", errors.New("that isn't a Discord webhook link. It should start with https://discord.com/api/webhooks/")
	}
	return u, nil
}

// redact hides the secret part of a webhook URL for logs.
func redact(u string) string {
	if i := strings.LastIndex(u, "/"); i > 0 {
		return u[:i] + "/…"
	}
	return "…"
}

var shareClient = &http.Client{Timeout: 0} // uploads can take a while; requests use contexts

// CheckWebhook asks Discord about a webhook, so we can confirm it works
// before saving it and build "Open in Discord" links later.
func CheckWebhook(ctx context.Context, raw string) (Webhook, string, error) {
	u, err := NormalizeWebhookURL(raw)
	if err != nil {
		return Webhook{}, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := shareClient.Do(req)
	if err != nil {
		return Webhook{}, "", fmt.Errorf("couldn't reach Discord: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 404 {
		return Webhook{}, "", errors.New("Discord says this webhook doesn't exist. It may have been deleted; make a new one in the channel's settings")
	}
	if resp.StatusCode != 200 {
		return Webhook{}, "", fmt.Errorf("Discord answered %s", resp.Status)
	}
	var info struct {
		Name      string `json:"name"`
		GuildID   string `json:"guild_id"`
		ChannelID string `json:"channel_id"`
	}
	json.NewDecoder(resp.Body).Decode(&info)
	return Webhook{URL: u, GuildID: info.GuildID, ChannelID: info.ChannelID}, info.Name, nil
}

// ---------- fitting a clip into Discord's limit ----------

// ShrinkPlan describes how a clip will be prepared for Discord.
type ShrinkPlan struct {
	Encode    bool    `json:"encode"`    // re-encode (shrink and/or convert)
	Remux     bool    `json:"remux"`     // repackage only (no quality loss)
	Height    int     `json:"height"`    // 0 = keep
	FPS       int     `json:"fps"`       // 0 = keep
	VideoKbps int     `json:"videoKbps"` // target video bitrate
	Duration  float64 `json:"duration"`
	Summary   string  `json:"summary"` // shown in the share dialog
	TooLong   bool    `json:"tooLong"` // can't fit at watchable quality
	MaxSecs   int     `json:"maxSecs"` // roughly how long a clip fits
}

const shareAudioKbps = 128

// PlanShrink decides whether a clip needs re-encoding to post it, and how.
// Discord only plays H.264 MP4 inline everywhere, so other formats are
// converted even when they're small enough.
func PlanShrink(info ClipInfo, path string, size int64, limitBytes int64) ShrinkPlan {
	p := ShrinkPlan{Duration: info.Duration}
	playable := info.VideoCodec == "h264" && strings.EqualFold(filepath.Ext(path), ".mp4")
	budget := float64(limitBytes) * 0.92 // headroom for the container and upload overhead
	p.MaxSecs = int(budget * 8 / 1000 / (600 + shareAudioKbps))
	if size <= int64(float64(limitBytes)*0.97) && playable && len(info.Audio) <= 1 {
		p.Summary = fmt.Sprintf("Posts as is (%.1f MB).", float64(size)/(1<<20))
		return p
	}
	if size <= int64(float64(limitBytes)*0.97) && info.VideoCodec == "h264" {
		// Fits already and the video is H.264: just repackage as MP4 with the mixed track.
		p.Remux = true
		p.Summary = fmt.Sprintf("Repackages as MP4 so it plays in Discord, no quality loss (about %.1f MB).", float64(size)/(1<<20))
		return p
	}
	p.Encode = true
	if info.Duration <= 0 {
		info.Duration = 1
	}
	vk := int(budget*8/1000/info.Duration) - shareAudioKbps
	srcH, srcFPS := info.Height, int(math.Round(info.FPS))
	if srcFPS <= 0 {
		srcFPS = 60
	}
	switch {
	case size <= int64(float64(limitBytes)*0.97):
		// Small enough already; only the format changes. Keep quality high.
		vk = int(float64(size)*8/1000/info.Duration*1.0) + 500
		p.Height = srcH
	case vk >= 9000:
		p.Height = 1440
	case vk >= 4500:
		p.Height = 1080
	case vk >= 2200:
		p.Height = 720
	case vk >= 1100:
		p.Height = 540
	default:
		p.Height = 480
	}
	if srcH > 0 && p.Height > srcH {
		p.Height = srcH
	}
	p.FPS = srcFPS
	if vk < 2500 && srcFPS > 60 {
		p.FPS = 60
	}
	if vk < 1200 && srcFPS > 30 {
		p.FPS = 30
	}
	if vk < 300 {
		p.TooLong = true
		p.Summary = fmt.Sprintf("This clip is too long to fit %d MB and still look okay. Trim it to about %s in the editor first.",
			limitBytes>>20, humanSeconds(p.MaxSecs))
		return p
	}
	p.VideoKbps = vk
	if size <= int64(float64(limitBytes)*0.97) {
		p.Summary = "Converts to MP4 so it plays right in Discord."
	} else {
		p.Summary = fmt.Sprintf("Shrinks to fit %d MB: %dp, %d fps.", limitBytes>>20, p.Height, p.FPS)
	}
	return p
}

// ShrinkArgs builds the ffmpeg command for a plan. Only the first audio
// track is kept (it's the mixed one), because Discord plays only one.
func ShrinkArgs(src, out string, p ShrinkPlan, kbps int, nvenc bool) []string {
	a := []string{"-hide_banner", "-nostats", "-loglevel", "error", "-y", "-progress", "pipe:1"}
	if nvenc {
		a = append(a, "-hwaccel", "auto")
	}
	a = append(a, "-i", src, "-map", "0:v:0", "-map", "0:a:0?")
	vf := []string{}
	if p.Height > 0 {
		vf = append(vf, fmt.Sprintf("scale=-2:%d:flags=lanczos", p.Height))
	}
	if p.FPS > 0 {
		vf = append(vf, fmt.Sprintf("fps=%d", p.FPS))
	}
	vf = append(vf, "format=yuv420p")
	a = append(a, "-vf", strings.Join(vf, ","))
	k := strconv.Itoa(kbps) + "k"
	if nvenc {
		a = append(a, "-c:v", "h264_nvenc", "-preset", "p6", "-tune", "hq", "-multipass", "fullres",
			"-rc", "cbr", "-b:v", k, "-maxrate", k, "-bufsize", k, "-profile:v", "high")
	} else {
		a = append(a, "-c:v", "libopenh264", "-b:v", k, "-maxrate", k)
	}
	a = append(a, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", shareAudioKbps), "-ac", "2",
		"-movflags", "+faststart", "-f", "mp4", out)
	return a
}

// ---------- posting ----------

type ShareRequest struct {
	Path    string  `json:"path"`
	Webhook Webhook `json:"webhook"`
	Message string  `json:"message"`
}

type ShareResult struct {
	MessageURL string  `json:"messageUrl"`
	SizeMB     float64 `json:"sizeMB"`
	messageID  string
	channelID  string
	guildID    string
}

// Sharer prepares and posts one clip at a time.
type Sharer struct {
	FFmpeg  string
	Command func(string, ...string) *exec.Cmd
	NVENC   func() bool
	TempDir string     // working files for posts (deleted right after)
	ClipDir string     // prepared copies for the clipboard (kept a day)
	LimitMB func() int // server upload limit for webhook posts

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *Sharer) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

func (s *Sharer) Cancel() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *Sharer) limitBytes() int64 {
	mb := 20
	if s.LimitMB != nil {
		mb = s.LimitMB()
	}
	return int64(mb) << 20
}

// Plan tells the UI what will happen before the user clicks Post.
func (s *Sharer) Plan(path string, limitMB int) (ShrinkPlan, error) {
	info, err := ProbeClip(s.FFmpeg, path, s.Command)
	if err != nil {
		return ShrinkPlan{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ShrinkPlan{}, errors.New("the clip was moved or deleted")
	}
	return PlanShrink(info, path, fi.Size(), s.limitFor(limitMB)), nil
}

func (s *Sharer) limitFor(mb int) int64 {
	if mb <= 0 {
		return s.limitBytes()
	}
	return int64(mb) << 20
}

func (s *Sharer) begin() (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil, nil, errors.New("another clip is still being shared")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return ctx, func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
		cancel()
	}, nil
}

// shareName is the file name people see in Discord.
func shareName(src string) string {
	return sanitizeName(strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))) + ".mp4"
}

// prepare makes a Discord-ready copy of the clip inside dir (or returns the
// original when it already fits and plays). made reports whether out is a
// new file the caller owns.
func (s *Sharer) prepare(ctx context.Context, src string, limit int64, dir string, progress func(float64)) (out string, made bool, err error) {
	info, err := ProbeClip(s.FFmpeg, src, s.Command)
	if err != nil {
		return "", false, err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", false, errors.New("the clip was moved or deleted")
	}
	plan := PlanShrink(info, src, fi.Size(), limit)
	if plan.TooLong {
		return "", false, errors.New(plan.Summary)
	}
	if !plan.Encode && !plan.Remux {
		progress(1)
		return src, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	out = filepath.Join(dir, shareName(src))
	fail := func(e error) (string, bool, error) { os.Remove(out); return "", false, e }
	if plan.Remux {
		if err := runFFmpegProgress(ctx, s.Command, s.FFmpeg, RemuxArgs(src, out), plan.Duration, progress); err != nil {
			return fail(err)
		}
		if st, e := os.Stat(out); e == nil && st.Size() <= limit {
			progress(1)
			return out, true, nil
		}
		plan.Remux, plan.Encode = false, true               // still too big after dropping tracks: shrink it
		plan = PlanShrink(info, src, limit+limit/50, limit) // pretend it is over the limit to get a shrink plan
		if plan.TooLong {
			return fail(errors.New(plan.Summary))
		}
	}
	kbps := plan.VideoKbps
	nv := s.NVENC == nil || s.NVENC()
	for attempt := 0; attempt < 3; attempt++ {
		err = runFFmpegProgress(ctx, s.Command, s.FFmpeg, ShrinkArgs(src, out, plan, kbps, nv), plan.Duration, progress)
		if err != nil && nv && !errors.Is(err, ErrCanceled) {
			nv = false // NVENC unavailable: retry on the CPU encoder
			attempt--
			continue
		}
		if err != nil {
			return fail(err)
		}
		st, e := os.Stat(out)
		if e == nil && st.Size() <= limit {
			progress(1)
			return out, true, nil
		}
		kbps = kbps * 85 / 100 // came out a little big: aim lower and try again
	}
	return fail(fmt.Errorf("couldn't get the clip under %d MB. Trim it shorter in the editor and try again", limit>>20))
}

// Share prepares the clip (if needed) and posts it. progress gets a stage
// ("prepare" or "upload") and 0..1.
func (s *Sharer) Share(req ShareRequest, progress func(stage string, p float64)) (ShareResult, error) {
	ctx, done, err := s.begin()
	if err != nil {
		return ShareResult{}, err
	}
	defer done()
	hook, err := NormalizeWebhookURL(req.Webhook.URL)
	if err != nil {
		return ShareResult{}, err
	}
	dir := filepath.Join(s.TempDir, fmt.Sprintf("post-%d", time.Now().UnixNano()))
	defer os.RemoveAll(dir)
	upload, _, err := s.prepare(ctx, req.Path, s.limitBytes(), dir, func(p float64) { progress("prepare", p) })
	if err != nil {
		return ShareResult{}, err
	}
	name := filepath.Base(upload)
	res, err := postToWebhook(ctx, hook, upload, name, req.Message, func(p float64) { progress("upload", p) })
	if err != nil {
		return ShareResult{}, err
	}
	if st, e := os.Stat(upload); e == nil {
		res.SizeMB = float64(st.Size()) / (1 << 20)
	}
	res.MessageURL = messageLink(res, req.Webhook)
	return res, nil
}

// CopyReady prepares a clip to paste into Discord yourself and returns the
// file to put on the clipboard. Prepared copies live in ClipDir (each in its
// own folder so the file keeps a clean name) and are cleaned up after a day.
func (s *Sharer) CopyReady(src string, limitMB int, progress func(float64)) (string, float64, error) {
	ctx, done, err := s.begin()
	if err != nil {
		return "", 0, err
	}
	defer done()
	s.CleanClipDir(24 * time.Hour)
	dir := filepath.Join(s.ClipDir, fmt.Sprintf("%d", time.Now().UnixNano()))
	out, made, err := s.prepare(ctx, src, s.limitFor(limitMB), dir, progress)
	if err != nil || !made {
		os.RemoveAll(dir)
	}
	if err != nil {
		return "", 0, err
	}
	var mb float64
	if st, e := os.Stat(out); e == nil {
		mb = float64(st.Size()) / (1 << 20)
	}
	return out, mb, nil
}

// CleanClipDir removes prepared clipboard copies older than maxAge.
func (s *Sharer) CleanClipDir(maxAge time.Duration) {
	if s.ClipDir == "" {
		return
	}
	entries, _ := os.ReadDir(s.ClipDir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > maxAge {
			os.RemoveAll(filepath.Join(s.ClipDir, e.Name()))
		}
	}
}

// RemuxArgs repackages an H.264 clip as MP4 with just the first (mixed)
// audio track: no re-encoding, no quality loss.
func RemuxArgs(src, out string) []string {
	return []string{"-hide_banner", "-nostats", "-loglevel", "error", "-y", "-progress", "pipe:1",
		"-i", src, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", out}
}

type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	cb    func(float64)
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.total > 0 && (time.Since(p.last) > 100*time.Millisecond || err == io.EOF) {
		p.last = time.Now()
		p.cb(math.Min(1, float64(p.n)/float64(p.total)))
	}
	return n, err
}

func postToWebhook(ctx context.Context, hook, file, name, message string, progress func(float64)) (ShareResult, error) {
	payload := map[string]interface{}{
		"attachments":      []map[string]interface{}{{"id": 0, "filename": name}},
		"allowed_mentions": map[string]interface{}{"parse": []string{}}, // never ping @everyone by accident
	}
	if strings.TrimSpace(message) != "" {
		if len([]rune(message)) > 2000 {
			message = string([]rune(message)[:2000])
		}
		payload["content"] = message
	}
	pj, _ := json.Marshal(payload)

	for attempt := 0; ; attempt++ {
		f, err := os.Open(file)
		if err != nil {
			return ShareResult{}, err
		}
		st, _ := f.Stat()
		// Build the multipart body as a stream so large files aren't held in memory.
		var head, tail bytes.Buffer
		mw := multipart.NewWriter(&head)
		pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="payload_json"`}, "Content-Type": {"application/json"}})
		pw.Write(pj)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[0]"; filename="%s"`, strings.ReplaceAll(name, `"`, "")))
		h.Set("Content-Type", "video/mp4")
		mw.CreatePart(h)
		boundary := mw.Boundary()
		fmt.Fprintf(&tail, "\r\n--%s--\r\n", boundary)
		total := int64(head.Len()) + st.Size() + int64(tail.Len())
		body := &progressReader{r: io.MultiReader(&head, f, &tail), total: total, cb: progress}

		u := hook + "?wait=true"
		req, _ := http.NewRequestWithContext(ctx, "POST", u, body)
		req.ContentLength = total
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
		req.Header.Set("User-Agent", "ReKapture (Windows clip recorder)")
		resp, err := shareClient.Do(req)
		f.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ShareResult{}, ErrCanceled
			}
			return ShareResult{}, fmt.Errorf("upload failed: %v", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 429 && attempt < 3:
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			json.Unmarshal(data, &rl)
			wait := time.Duration(math.Max(1, math.Min(30, rl.RetryAfter)) * float64(time.Second))
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ShareResult{}, ErrCanceled
			}
		case resp.StatusCode == 413 || strings.Contains(string(data), "40005"):
			return ShareResult{}, errors.New("Discord says the file is too big for this server. Set the upload limit on the Sharing page to 20 MB and try again")
		case resp.StatusCode == 401 || resp.StatusCode == 404:
			return ShareResult{}, errors.New("this Discord webhook no longer exists. Remove it on the Sharing page and add a new one")
		case resp.StatusCode == 403:
			return ShareResult{}, errors.New("Discord didn't allow the post (the webhook may lack permission to send files in that channel)")
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			var e struct {
				Message string `json:"message"`
			}
			json.Unmarshal(data, &e)
			if e.Message == "" {
				e.Message = resp.Status
			}
			return ShareResult{}, fmt.Errorf("Discord refused the upload: %s", e.Message)
		}
		var msg struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
			GuildID   string `json:"guild_id"`
		}
		json.Unmarshal(data, &msg)
		return ShareResult{messageID: msg.ID, channelID: msg.ChannelID, guildID: msg.GuildID}, nil
	}
}

// messageLink builds an "Open in Discord" link from whatever IDs we have.
func messageLink(r ShareResult, w Webhook) string {
	guild, channel := r.guildID, r.channelID
	if guild == "" {
		guild = w.GuildID
	}
	if channel == "" {
		channel = w.ChannelID
	}
	switch {
	case guild != "" && channel != "" && r.messageID != "":
		return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guild, channel, r.messageID)
	case guild != "" && channel != "":
		return fmt.Sprintf("https://discord.com/channels/%s/%s", guild, channel)
	}
	return ""
}
