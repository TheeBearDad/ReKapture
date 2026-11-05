//go:build linux

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeWebhookURL(t *testing.T) {
	good := map[string]string{
		"https://discord.com/api/webhooks/123/abc-DEF_9":               "https://discord.com/api/webhooks/123/abc-DEF_9",
		" https://discordapp.com/api/webhooks/123/abc/ ":               "https://discordapp.com/api/webhooks/123/abc",
		"https://ptb.discord.com/api/v10/webhooks/123/abc?thread_id=5": "https://ptb.discord.com/api/v10/webhooks/123/abc",
		"https://canary.discord.com/api/webhooks/1/x#frag":             "https://canary.discord.com/api/webhooks/1/x",
	}
	for in, want := range good {
		if got, err := NormalizeWebhookURL(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "discord.com/api/webhooks/1/x", "http://discord.com/api/webhooks/1/x",
		"https://evil.com/api/webhooks/1/x", "https://discord.com.evil.com/api/webhooks/1/x", "https://discord.com/channels/1/2"} {
		if _, err := NormalizeWebhookURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPlanShrink(t *testing.T) {
	mb := int64(1 << 20)
	small := ClipInfo{Duration: 10, VideoCodec: "h264", Height: 1440, FPS: 60, Audio: []AudioTrack{{0, "Game audio", "aac"}}}
	if p := PlanShrink(small, "a.mp4", 15*mb, 20*mb); p.Encode {
		t.Errorf("small H.264 MP4 should post as is: %+v", p)
	}
	if p := PlanShrink(small, "a.mkv", 15*mb, 20*mb); !p.Remux || p.Encode {
		t.Errorf("small H.264 MKV should just be repackaged: %+v", p)
	}
	multi := small
	multi.Audio = append(multi.Audio, AudioTrack{1, "Microphone", "aac"})
	if p := PlanShrink(multi, "a.mp4", 15*mb, 20*mb); !p.Remux {
		t.Errorf("extra audio tracks should be dropped by repackaging: %+v", p)
	}
	hevc := small
	hevc.VideoCodec = "hevc"
	if p := PlanShrink(hevc, "a.mp4", 15*mb, 20*mb); !p.Encode {
		t.Errorf("HEVC should be converted for inline playback: %+v", p)
	}
	big := ClipInfo{Duration: 120, VideoCodec: "h264", Height: 1440, FPS: 144, Audio: []AudioTrack{{0, "", "aac"}}}
	p := PlanShrink(big, "a.mp4", 700*mb, 20*mb)
	if !p.Encode || p.TooLong || p.Height > 720 || p.FPS > 60 || p.VideoKbps < 300 {
		t.Errorf("2 min clip into 20 MB: %+v", p)
	}
	if est := float64(p.VideoKbps+shareAudioKbps) * 1000 / 8 * big.Duration; est > float64(20*mb) {
		t.Errorf("planned size %.1f MB exceeds the limit", est/float64(mb))
	}
	long := big
	long.Duration = 1800
	if p := PlanShrink(long, "a.mp4", 9000*mb, 20*mb); !p.TooLong || p.MaxSecs < 150 || p.MaxSecs > 400 {
		t.Errorf("30 min into 20 MB should be refused with a sensible max: %+v", p)
	}
}

// fakeDiscord mimics the webhook API closely enough to test uploads.
type fakeDiscord struct {
	srv      *httptest.Server
	mu       sync.Mutex
	posts    int
	gotSize  int64
	gotName  string
	gotJSON  map[string]interface{}
	limit429 int // reply 429 this many times first
	maxBytes int64
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{maxBytes: 1 << 40}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Unknown Webhook","code":10015}`)
			return
		case r.Method == "GET":
			io.WriteString(w, `{"name":"Captain Hook","guild_id":"111","channel_id":"222"}`)
			return
		}
		f.mu.Lock()
		if f.limit429 > 0 {
			f.limit429--
			f.mu.Unlock()
			w.WriteHeader(429)
			io.WriteString(w, `{"message":"You are being rate limited.","retry_after":0.2,"global":false}`)
			return
		}
		f.mu.Unlock()
		if r.URL.Query().Get("wait") != "true" || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			w.WriteHeader(400)
			return
		}
		if r.ContentLength > f.maxBytes {
			w.WriteHeader(413)
			io.WriteString(w, `{"message":"Request entity too large","code":40005}`)
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(400)
			return
		}
		var size int64
		var name string
		var pj map[string]interface{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			switch p.FormName() {
			case "payload_json":
				json.NewDecoder(p).Decode(&pj)
			case "files[0]":
				name = p.FileName()
				size, _ = io.Copy(io.Discard, p)
			}
		}
		f.mu.Lock()
		f.posts++
		f.gotSize, f.gotName, f.gotJSON = size, name, pj
		f.mu.Unlock()
		io.WriteString(w, `{"id":"333","channel_id":"222"}`)
	}))
	t.Cleanup(f.srv.Close)
	allowTestWebhooks = true
	t.Cleanup(func() { allowTestWebhooks = false })
	return f
}

func (f *fakeDiscord) hook(name string) string { return f.srv.URL + "/api/webhooks/1/" + name }

func makeBigClip(t *testing.T, path string, secs, kbps int) {
	t.Helper()
	d := itoa(secs)
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=60:d=" + d, "-f", "lavfi", "-i", "sine=f=440:d=" + d,
		"-f", "lavfi", "-i", "sine=f=660:d=" + d,
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libopenh264", "-b:v", itoa(kbps) + "k", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path}
	if out, err := exec.Command(testFFmpeg(), args...).CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, out)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func testSharer(dir string, mb int) *Sharer {
	return &Sharer{FFmpeg: testFFmpeg(), Command: exec.Command, NVENC: func() bool { return false },
		TempDir: filepath.Join(dir, "tmp"), LimitMB: func() int { return mb }}
}

func TestShareShrinksAndPosts(t *testing.T) {
	fd := newFakeDiscord(t)
	fd.limit429 = 1
	dir := t.TempDir()
	clip := filepath.Join(dir, "Cyberpunk 2077 2026-11-04 06-18-24 Replay.mp4")
	makeBigClip(t, clip, 12, 6000)
	fi, _ := os.Stat(clip)
	if fi.Size() < 4<<20 {
		t.Fatalf("test clip too small (%d bytes)", fi.Size())
	}

	w, name, err := CheckWebhook(t.Context(), fd.hook("tok"))
	if err != nil || name != "Captain Hook" || w.GuildID != "111" {
		t.Fatalf("check: %+v %q %v", w, name, err)
	}
	sh := testSharer(dir, 2) // 2 MB limit forces a shrink
	plan, err := sh.Plan(clip, 0)
	if err != nil || !plan.Encode || plan.TooLong {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	var stages []string
	res, err := sh.Share(ShareRequest{Path: clip, Webhook: w, Message: "**Cyberpunk 2077** @everyone"}, func(s string, p float64) {
		if len(stages) == 0 || stages[len(stages)-1] != s {
			stages = append(stages, s)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if fd.posts != 1 || fd.gotSize == 0 || fd.gotSize > 2<<20 {
		t.Errorf("posted %d times, size %d (limit %d)", fd.posts, fd.gotSize, 2<<20)
	}
	if fd.gotName != "Cyberpunk 2077 2026-11-04 06-18-24 Replay.mp4" {
		t.Errorf("filename %q", fd.gotName)
	}
	if fd.gotJSON["content"] != "**Cyberpunk 2077** @everyone" {
		t.Errorf("content %v", fd.gotJSON["content"])
	}
	am, _ := fd.gotJSON["allowed_mentions"].(map[string]interface{})
	if am == nil || len(am["parse"].([]interface{})) != 0 {
		t.Errorf("mentions must be disabled: %v", fd.gotJSON["allowed_mentions"])
	}
	if res.MessageURL != "https://discord.com/channels/111/222/333" {
		t.Errorf("link %q", res.MessageURL)
	}
	if strings.Join(stages, ",") != "prepare,upload" {
		t.Errorf("stages %v", stages)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "tmp", "*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestShareAsIsAndErrors(t *testing.T) {
	fd := newFakeDiscord(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "small.mp4")
	makeClip(t, clip) // H.264 MP4, 1 audio track, tiny
	sh := testSharer(dir, 20)
	if p, _ := sh.Plan(clip, 0); p.Encode || p.Remux {
		t.Errorf("small clip should post as is: %+v", p)
	}
	fi, _ := os.Stat(clip)
	if _, err := sh.Share(ShareRequest{Path: clip, Webhook: Webhook{URL: fd.hook("tok")}}, func(string, float64) {}); err != nil {
		t.Fatal(err)
	}
	if fd.gotSize != fi.Size() || fd.gotName != "small.mp4" {
		t.Errorf("as-is upload changed the file: %d vs %d, %q", fd.gotSize, fi.Size(), fd.gotName)
	}
	if _, ok := fd.gotJSON["content"]; ok {
		t.Errorf("empty message should send no content")
	}

	if _, err := sh.Share(ShareRequest{Path: clip, Webhook: Webhook{URL: fd.hook("gone")}}, func(string, float64) {}); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("deleted webhook: %v", err)
	}
	if _, _, err := CheckWebhook(t.Context(), fd.hook("gone")); err == nil {
		t.Errorf("check of deleted webhook should fail")
	}
	fd.maxBytes = 100
	if _, err := sh.Share(ShareRequest{Path: clip, Webhook: Webhook{URL: fd.hook("tok")}}, func(string, float64) {}); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Errorf("413: %v", err)
	}
	if _, err := sh.Share(ShareRequest{Path: clip, Webhook: Webhook{URL: "https://example.com/x"}}, func(string, float64) {}); err == nil {
		t.Errorf("non-Discord URL must be refused")
	}
}

func TestShareCancel(t *testing.T) {
	fd := newFakeDiscord(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "long.mp4")
	makeBigClip(t, clip, 40, 4000)
	sh := testSharer(dir, 5)
	errc := make(chan error, 1)
	go func() {
		_, err := sh.Share(ShareRequest{Path: clip, Webhook: Webhook{URL: fd.hook("tok")}}, func(string, float64) {})
		errc <- err
	}()
	time.Sleep(700 * time.Millisecond)
	if !sh.Busy() {
		t.Fatal("should be busy")
	}
	sh.Cancel()
	if err := <-errc; err != ErrCanceled {
		t.Errorf("want canceled, got %v", err)
	}
	if fd.posts != 0 {
		t.Errorf("nothing should be posted after cancel")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "tmp", "*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestCopyReady(t *testing.T) {
	dir := t.TempDir()
	sh := testSharer(dir, 20)
	sh.ClipDir = filepath.Join(dir, "clipboard")

	// Already fits and plays: the original goes on the clipboard, nothing is made.
	small := filepath.Join(dir, "small.mp4")
	makeClip(t, small)
	out, _, err := sh.CopyReady(small, 20, func(float64) {})
	if err != nil || out != small {
		t.Fatalf("as-is copy: %q %v", out, err)
	}
	if left, _ := filepath.Glob(filepath.Join(sh.ClipDir, "*")); len(left) != 0 {
		t.Errorf("nothing should be prepared for an as-is copy: %v", left)
	}

	// Multi-track MKV that fits: repackaged losslessly, 1 audio track, clean name.
	mkv := filepath.Join(dir, "Game 2026-11-04 06-18-24 Replay.mkv")
	makeLayeredClip(t, mkv, 4)
	out, _, err = sh.CopyReady(mkv, 20, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "Game 2026-11-04 06-18-24 Replay.mp4" || !strings.HasPrefix(out, sh.ClipDir) {
		t.Errorf("prepared copy path %q", out)
	}
	info, _ := ProbeClip(testFFmpeg(), out, exec.Command)
	if len(info.Audio) != 1 || info.VideoCodec != "h264" {
		t.Errorf("remuxed copy: %+v", info)
	}

	// Too big for the chosen size: shrunk to fit.
	big := filepath.Join(dir, "big.mp4")
	makeBigClip(t, big, 12, 6000)
	out, mb, err := sh.CopyReady(big, 2, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st == nil || st.Size() > 2<<20 || mb <= 0 {
		t.Errorf("shrunk copy too big or missing: %v %.2f MB", out, mb)
	}

	// Old copies get cleaned up.
	old := filepath.Join(sh.ClipDir, "1")
	os.MkdirAll(old, 0o755)
	os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	sh.CleanClipDir(24 * time.Hour)
	if fileExists(old) {
		t.Errorf("old clipboard copy not cleaned")
	}
}

func TestDropFiles(t *testing.T) {
	b := DropFilesData(`C:\Users\Tyler\Videos\ReKapture\clip é.mp4`)
	if len(b) < 20 || b[0] != 20 || b[16] != 1 {
		t.Fatalf("bad DROPFILES header: % x", b[:20])
	}
	if b[len(b)-1] != 0 || b[len(b)-2] != 0 || b[len(b)-3] != 0 || b[len(b)-4] != 0 {
		t.Errorf("list must end with a double NUL")
	}
}
