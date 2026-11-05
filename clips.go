package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- clip list ----------

func clipKind(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	switch {
	case strings.HasSuffix(base, " Replay") || strings.Contains(base, " Replay ("):
		return "Replay"
	case strings.HasSuffix(base, " Recording") || strings.Contains(base, " Recording ("):
		return "Recording"
	}
	return "Video"
}

// ScanClips lists the newest videos in the clips folder (and one level of
// per-game folders), so the Recent clips list survives restarts.
func ScanClips(root string, limit int) []Clip {
	type item struct {
		path string
		fi   os.FileInfo
	}
	var items []item
	consider := func(p string, fi os.FileInfo) {
		ext := strings.ToLower(filepath.Ext(p))
		if fi.IsDir() || (ext != ".mp4" && ext != ".mkv") || fi.Size() == 0 {
			return
		}
		items = append(items, item{p, fi})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, en := range entries {
		p := filepath.Join(root, en.Name())
		if strings.HasPrefix(en.Name(), ".") {
			continue // .rekapture-buffer and other hidden folders
		}
		fi, err := en.Info()
		if err != nil {
			continue
		}
		if en.IsDir() {
			sub, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, s := range sub {
				if sfi, err := s.Info(); err == nil {
					consider(filepath.Join(p, s.Name()), sfi)
				}
			}
			continue
		}
		consider(p, fi)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].fi.ModTime().After(items[j].fi.ModTime()) })
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]Clip, 0, len(items))
	now := time.Now()
	for _, it := range items {
		mt := it.fi.ModTime()
		when := mt.Format("15:04")
		if y, m, d := mt.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
			when = mt.Format("Jan 2, 15:04")
		}
		out = append(out, Clip{Path: it.path, Name: filepath.Base(it.path), Kind: clipKind(it.fi.Name()),
			SizeMB: float64(it.fi.Size()) / (1 << 20), When: when})
	}
	return out
}

var (
	copyrightRe = regexp.MustCompile(`(?i)\s*(\(c\)|©|®|™|\(r\)|\(tm\)|copyright\b).*$`)
	versionRe   = regexp.MustCompile(`(?i)\s+(v\d+(\.\d+)+|build\s+\d+[\w.]*)$`)
)

// CleanTitle turns a window title into a tidy game name for folders and
// file names: "Cyberpunk 2077 (C) 2020 by CD Projekt RED" → "Cyberpunk 2077".
func CleanTitle(t string) string {
	c := strings.TrimSpace(copyrightRe.ReplaceAllString(t, ""))
	c = strings.TrimSpace(versionRe.ReplaceAllString(c, ""))
	c = strings.TrimRight(c, " -–—|:")
	if c == "" {
		return strings.TrimSpace(t)
	}
	return c
}

// ---------- media server ----------

// MediaServer streams clips, thumbnails and preview copies to the UI.
// It listens on 127.0.0.1 only and every URL carries a random token, so
// other programs and websites can't read your files through it.
type MediaServer struct {
	ln       net.Listener
	token    string
	cacheDir string
	ffmpeg   string
	command  func(name string, args ...string) *exec.Cmd
	roots    func() []string
	hasNVENC func() bool

	UI []byte // the app's HTML page, served at Base()+"/"

	mu       sync.Mutex
	inflight map[string]chan struct{}
	sem      chan struct{}
}

func NewMediaServer(ffmpeg, cacheDir string, command func(string, ...string) *exec.Cmd, roots func() []string, hasNVENC func() bool) (*MediaServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	m := &MediaServer{ln: ln, token: hex.EncodeToString(b), cacheDir: cacheDir, ffmpeg: ffmpeg, command: command,
		roots: roots, hasNVENC: hasNVENC, inflight: map[string]chan struct{}{}, sem: make(chan struct{}, 2)}
	os.MkdirAll(cacheDir, 0o755)
	go m.pruneCache(14 * 24 * time.Hour)
	mux := http.NewServeMux()
	// Serving the page from here keeps the UI and the clips on one origin, so
	// the browser engine treats clip loads as same-site (no private-network blocks).
	mux.HandleFunc("/"+m.token+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+m.token+"/" || m.UI == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(m.UI)
	})
	mux.HandleFunc("/"+m.token+"/file", m.handle("file"))
	mux.HandleFunc("/"+m.token+"/thumb", m.handle("thumb"))
	mux.HandleFunc("/"+m.token+"/preview", m.handle("preview"))
	mux.HandleFunc("/"+m.token+"/strip", m.handle("strip"))
	go http.Serve(ln, mux)
	return m, nil
}

// Base is the URL prefix the UI appends "/file?p=…" etc. to.
func (m *MediaServer) Base() string {
	return fmt.Sprintf("http://%s/%s", m.ln.Addr().String(), m.token)
}

func (m *MediaServer) Close() { m.ln.Close() }

// allowed reports whether p is a video inside one of the clip folders.
func (m *MediaServer) allowed(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ext != ".mp4" && ext != ".mkv" {
		return "", false
	}
	for _, r := range m.roots() {
		ra, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(ra, abs)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return abs, true
		}
	}
	return "", false
}

func (m *MediaServer) handle(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := m.allowed(r.URL.Query().Get("p"))
		if !ok {
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		fi, err := os.Stat(p)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		serve := p
		ctype := "video/mp4"
		if strings.EqualFold(filepath.Ext(p), ".mkv") {
			ctype = "video/x-matroska"
		}
		switch kind {
		case "thumb":
			serve, err = m.derived(p, fi, ".jpg", m.makeThumb)
			ctype = "image/jpeg"
		case "preview":
			serve, err = m.derived(p, fi, ".mp4", m.makePreview)
			ctype = "video/mp4"
		case "strip":
			serve, err = m.derived(p, fi, ".strip.jpg", m.makeStrip)
			ctype = "image/jpeg"
		}
		if err != nil {
			log.Printf("media %s %s: %v", kind, filepath.Base(p), err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f, err := os.Open(serve)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		sfi, _ := f.Stat()
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, "", sfi.ModTime(), f) // handles Range requests (seeking)
	}
}

// derived returns a cached file made from the clip, generating it once
// (concurrent requests for the same clip wait for the first one).
func (m *MediaServer) derived(src string, fi os.FileInfo, ext string, gen func(src, out string) error) (string, error) {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d", strings.ToLower(src), fi.Size(), fi.ModTime().UnixNano())))
	out := filepath.Join(m.cacheDir, hex.EncodeToString(h[:10])+ext)
	for {
		if _, err := os.Stat(out); err == nil {
			return out, nil
		}
		m.mu.Lock()
		if ch, busy := m.inflight[out]; busy {
			m.mu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		m.inflight[out] = ch
		m.mu.Unlock()

		m.sem <- struct{}{}
		tmp := out + ".tmp" + ext
		err := gen(src, tmp)
		<-m.sem
		if err == nil {
			err = os.Rename(tmp, out)
		}
		os.Remove(tmp)
		m.mu.Lock()
		delete(m.inflight, out)
		close(ch)
		m.mu.Unlock()
		if err != nil {
			return "", err
		}
		return out, nil
	}
}

func (m *MediaServer) run(args ...string) error {
	out, err := m.command(m.ffmpeg, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, lastLine(string(out)))
	}
	return nil
}

func (m *MediaServer) makeThumb(src, out string) error {
	for _, ss := range []string{"2", "0"} {
		err := m.run("-hide_banner", "-loglevel", "error", "-y", "-ss", ss, "-i", src,
			"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "4", "-f", "image2", out)
		if fi, e := os.Stat(out); err == nil && e == nil && fi.Size() > 0 {
			return nil
		}
	}
	return fmt.Errorf("couldn't read a frame from the clip")
}

// makePreview makes a small H.264 copy for clips the built-in player can't
// decode (HEVC without the Windows HEVC extension, MKV files). Uses NVENC
// when available; very long recordings are previewed up to 20 minutes.
func (m *MediaServer) makePreview(src, out string) error {
	encs := [][]string{{"-c:v", "libopenh264", "-b:v", "3M"}}
	if m.hasNVENC == nil || m.hasNVENC() {
		encs = append([][]string{{"-c:v", "h264_nvenc", "-preset", "p3", "-b:v", "4M"}}, encs...)
	}
	var err error
	for _, enc := range encs {
		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src, "-t", "1200",
			"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale='min(1280,iw)':-2,format=yuv420p"}
		args = append(args, enc...)
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", out)
		if err = m.run(args...); err == nil {
			return nil
		}
	}
	return err
}

// StripFrames is how many pictures the editor's timeline filmstrip has.
const StripFrames = 24

func (m *MediaServer) makeStrip(src, out string) error {
	info, err := ProbeClip(m.ffmpeg, src, m.command)
	if err != nil {
		return err
	}
	return m.run(FilmstripArgs(src, out, info.Duration, StripFrames)...)
}

func (m *MediaServer) pruneCache(maxAge time.Duration) {
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > maxAge {
			os.Remove(filepath.Join(m.cacheDir, e.Name()))
		}
	}
}
