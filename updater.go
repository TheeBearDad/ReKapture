package main

// Automatic updates from GitHub Releases.
//
// Each release (tag "vX.Y.Z") carries ReKapture-Setup-X.Y.Z.exe,
// ReKapture-Portable-X.Y.Z.zip and SHA256SUMS.txt. ReKapture asks GitHub
// for the latest release, and if it's newer, downloads the right file,
// checks its SHA-256 against SHA256SUMS.txt, and installs it.

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	UpdateRepo     = "TheeBearDad/ReKapture"
	updateAPI      = "https://api.github.com"
	maxUpdateBytes = 400 << 20 // the installer is ~50 MB; refuse anything absurd
)

// Release is the part of GitHub's release JSON ReKapture uses.
type Release struct {
	Tag    string `json:"tag_name"`
	Name   string `json:"name"`
	Body   string `json:"body"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (r Release) Version() string { return strings.TrimPrefix(strings.TrimSpace(r.Tag), "v") }

func (r Release) asset(name string) (string, int64, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a.URL, a.Size, true
		}
	}
	return "", 0, false
}

// NewerVersion reports whether version a is newer than b ("2.10.0" > "2.9.3").
func NewerVersion(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var p [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	for i, s := range strings.SplitN(v, ".", 3) {
		p[i], _ = strconv.Atoi(s)
	}
	return p
}

// UpdateAssetName is the file a copy of ReKapture updates from.
func UpdateAssetName(version string, portable bool) string {
	if portable {
		return "ReKapture-Portable-" + version + ".zip"
	}
	return "ReKapture-Setup-" + version + ".exe"
}

// ParseSums reads a SHA256SUMS.txt ("<hex>  <file name>" per line).
func ParseSums(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && len(f[0]) == 64 {
			out[strings.ToLower(strings.TrimPrefix(strings.Join(f[1:], " "), "*"))] = strings.ToLower(f[0])
		}
	}
	return out
}

// UpdateState is what the UI shows on the About page.
type UpdateState struct {
	Phase   string  `json:"phase"` // idle|checking|current|available|downloading|ready|installing|error
	Current string  `json:"current"`
	Latest  string  `json:"latest"`
	Notes   string  `json:"notes"`
	Page    string  `json:"page"`
	Percent float64 `json:"percent"`
	Error   string  `json:"error"`
	Checked string  `json:"checked"` // when GitHub was last asked
}

// Updater checks for, downloads and verifies updates. Installing is up to
// the platform (run the installer, or swap the portable files).
type Updater struct {
	Current  string
	Portable bool
	Dir      string // where downloads go
	API      string // GitHub API base (tests point it elsewhere)
	Client   *http.Client
	OnChange func(UpdateState)

	mu     sync.Mutex
	st     UpdateState
	rel    *Release
	file   string // verified download, ready to install
	cancel context.CancelFunc
}

func (u *Updater) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.st
	s.Current = u.Current
	if s.Phase == "" {
		s.Phase = "idle"
	}
	return s
}

func (u *Updater) set(f func(*UpdateState)) {
	u.mu.Lock()
	f(&u.st)
	u.mu.Unlock()
	if u.OnChange != nil {
		u.OnChange(u.State())
	}
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (u *Updater) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ReKapture/"+u.Current)
	req.Header.Set("Accept", "application/vnd.github+json")
	return u.client().Do(req)
}

// Check asks GitHub for the latest release. It returns true when a newer
// version is available.
func (u *Updater) Check(ctx context.Context) (bool, error) {
	u.mu.Lock()
	busy := u.st.Phase == "downloading" || u.st.Phase == "installing"
	u.mu.Unlock()
	if busy {
		return false, nil
	}
	u.set(func(s *UpdateState) { s.Phase, s.Error = "checking", "" })
	api := u.API
	if api == "" {
		api = updateAPI
	}
	rel, err := u.fetchLatest(ctx, api)
	now := time.Now().Format("Jan 2, 3:04 PM")
	if err != nil {
		u.set(func(s *UpdateState) { s.Phase, s.Error, s.Checked = "error", err.Error(), now })
		return false, err
	}
	v := rel.Version()
	if !NewerVersion(v, u.Current) {
		u.set(func(s *UpdateState) { s.Phase, s.Latest, s.Checked = "current", v, now })
		return false, nil
	}
	if _, _, ok := rel.asset(UpdateAssetName(v, u.Portable)); !ok {
		// Release is still being uploaded, or doesn't have this kind of file.
		u.set(func(s *UpdateState) { s.Phase, s.Latest, s.Checked = "current", u.Current, now })
		return false, nil
	}
	u.mu.Lock()
	u.rel = rel
	u.mu.Unlock()
	u.set(func(s *UpdateState) {
		s.Phase, s.Latest, s.Notes, s.Page, s.Checked = "available", v, trimNotes(rel.Body), rel.URL, now
	})
	return true, nil
}

func (u *Updater) fetchLatest(ctx context.Context, api string) (*Release, error) {
	resp, err := u.get(ctx, api+"/repos/"+UpdateRepo+"/releases/latest")
	if err != nil {
		return nil, errors.New("couldn't reach GitHub. Check your internet connection")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 404:
		return nil, errors.New("no releases have been published yet")
	case resp.StatusCode == 403 || resp.StatusCode == 429:
		return nil, errors.New("GitHub is limiting requests right now. ReKapture will try again later")
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, errors.New("GitHub sent an unexpected answer")
	}
	if rel.Version() == "" {
		return nil, errors.New("the latest release has no version tag")
	}
	return &rel, nil
}

func trimNotes(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) > 1500 {
		s = s[:1500] + "…"
	}
	return s
}

// allowedDownload keeps downloads to this repository's release files.
func allowedDownload(raw, api string) bool {
	if api != "" && api != updateAPI && strings.HasPrefix(raw, "http://127.0.0.1:") {
		return true // tests
	}
	p, err := url.Parse(raw)
	return err == nil && p.Scheme == "https" && strings.EqualFold(p.Host, "github.com") &&
		strings.HasPrefix(strings.ToLower(p.Path), "/"+strings.ToLower(UpdateRepo)+"/releases/download/")
}

// Download fetches the update for this copy and verifies its SHA-256.
// On success the file is ready for Install.
func (u *Updater) Download() (string, error) {
	u.mu.Lock()
	rel := u.rel
	if rel == nil || u.st.Phase == "downloading" {
		u.mu.Unlock()
		return "", errors.New("no update to download")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	u.cancel = cancel
	u.mu.Unlock()
	defer cancel()
	fail := func(err error) (string, error) {
		u.set(func(s *UpdateState) { s.Phase, s.Error = "error", err.Error() })
		return "", err
	}
	v := rel.Version()
	name := UpdateAssetName(v, u.Portable)
	fileURL, size, _ := rel.asset(name)
	sumsURL, _, ok := rel.asset("SHA256SUMS.txt")
	if !ok {
		return fail(errors.New("this release has no checksum file, so it can't be verified. Download it from GitHub instead"))
	}
	if !allowedDownload(fileURL, u.API) || !allowedDownload(sumsURL, u.API) {
		return fail(errors.New("the update isn't on ReKapture's GitHub page, so it wasn't downloaded"))
	}
	if size > maxUpdateBytes {
		return fail(errors.New("the update file is unexpectedly large, so it wasn't downloaded"))
	}
	u.set(func(s *UpdateState) { s.Phase, s.Percent, s.Error = "downloading", 0, "" })

	resp, err := u.get(ctx, sumsURL)
	if err != nil {
		return fail(errors.New("couldn't download the update. Check your internet connection"))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	want := ParseSums(string(b))[strings.ToLower(name)]
	if resp.StatusCode != 200 || want == "" {
		return fail(errors.New("couldn't read the update's checksum, so it wasn't installed"))
	}

	if err := os.MkdirAll(u.Dir, 0o755); err != nil {
		return fail(err)
	}
	dest := filepath.Join(u.Dir, name)
	part := dest + ".part"
	resp, err = u.get(ctx, fileURL)
	if err != nil {
		return fail(errors.New("couldn't download the update. Check your internet connection"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fail(fmt.Errorf("couldn't download the update (GitHub answered %d)", resp.StatusCode))
	}
	if resp.ContentLength > 0 {
		size = resp.ContentLength
	}
	f, err := os.Create(part)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(part)
				return fail(fmt.Errorf("couldn't save the update: %v", err))
			}
			h.Write(buf[:n])
			done += int64(n)
			if done > maxUpdateBytes {
				f.Close()
				os.Remove(part)
				return fail(errors.New("the update file is unexpectedly large, so it wasn't downloaded"))
			}
			if size > 0 && time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				pct := float64(done) * 100 / float64(size)
				u.set(func(s *UpdateState) { s.Percent = pct })
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			if ctx.Err() == context.Canceled {
				u.set(func(s *UpdateState) { s.Phase, s.Percent = "available", 0 })
				return "", errors.New("download cancelled")
			}
			return fail(errors.New("the download was interrupted. Try again"))
		}
	}
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(part)
		return fail(errors.New("the downloaded update didn't match its checksum, so it wasn't installed. Try again"))
	}
	os.Remove(dest)
	if err := os.Rename(part, dest); err != nil {
		return fail(err)
	}
	u.mu.Lock()
	u.file = dest
	u.mu.Unlock()
	u.set(func(s *UpdateState) { s.Phase, s.Percent = "ready", 100 })
	return dest, nil
}

// CancelDownload stops a download in progress.
func (u *Updater) CancelDownload() {
	u.mu.Lock()
	c := u.cancel
	u.mu.Unlock()
	if c != nil {
		c()
	}
}

// CleanDownloads removes old update files (after a successful update).
func (u *Updater) CleanDownloads() {
	entries, _ := os.ReadDir(u.Dir)
	for _, e := range entries {
		os.Remove(filepath.Join(u.Dir, e.Name()))
	}
}

// ApplyPortableUpdate puts the files from a portable release zip into dir.
// Files that may be running (ReKapture.exe) are renamed to *.old first,
// since Windows allows renaming a running program but not overwriting it;
// the old copies are removed the next time ReKapture starts. The user's
// Data folder and portable.txt are never touched.
func ApplyPortableUpdate(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("the update file is damaged: %v", err)
	}
	defer zr.Close()
	// The zip may hold the files directly or inside one top folder.
	prefix := ""
	if len(zr.File) > 0 {
		first := strings.SplitN(strings.ReplaceAll(zr.File[0].Name, `\`, "/"), "/", 2)[0] + "/"
		all := true
		for _, f := range zr.File {
			if !strings.HasPrefix(strings.ReplaceAll(f.Name, `\`, "/"), first) {
				all = false
				break
			}
		}
		if all {
			prefix = first
		}
	}
	type item struct {
		f   *zip.File
		rel string
	}
	var items []item
	hasExe := false
	for _, f := range zr.File {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, `\`, "/"), prefix)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("the update file has unsafe paths, so it wasn't installed")
		}
		low := strings.ToLower(filepath.ToSlash(clean))
		if low == "portable.txt" || low == "data" || strings.HasPrefix(low, "data/") {
			continue
		}
		if low == "rekapture.exe" {
			hasExe = true
		}
		items = append(items, item{f, clean})
	}
	if !hasExe {
		return errors.New("the update file doesn't contain ReKapture.exe")
	}
	// Write everything to temporary names first, so a failure part-way
	// leaves the current version untouched.
	var staged []string
	cleanup := func() {
		for _, s := range staged {
			os.Remove(s)
		}
	}
	for _, it := range items {
		dst := filepath.Join(dir, it.rel) + ".new"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return err
		}
		rc, err := it.f.Open()
		if err != nil {
			cleanup()
			return err
		}
		w, err := os.Create(dst)
		if err != nil {
			rc.Close()
			cleanup()
			return fmt.Errorf("can't write to %s: %v", dir, err)
		}
		_, err = io.Copy(w, io.LimitReader(rc, maxUpdateBytes))
		rc.Close()
		w.Close()
		if err != nil {
			cleanup()
			return err
		}
		staged = append(staged, dst)
	}
	for _, it := range items {
		target := filepath.Join(dir, it.rel)
		if _, err := os.Stat(target); err == nil {
			os.Remove(target + ".old")
			if err := os.Rename(target, target+".old"); err != nil {
				return fmt.Errorf("couldn't replace %s: %v", it.rel, err)
			}
		}
		if err := os.Rename(target+".new", target); err != nil {
			return fmt.Errorf("couldn't replace %s: %v", it.rel, err)
		}
	}
	return nil
}

// RemoveOldFiles deletes *.old files left by a portable update.
func RemoveOldFiles(dir string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.EqualFold(d.Name(), "Data") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(p), ".old") {
			os.Remove(p)
		}
		return nil
	})
}
