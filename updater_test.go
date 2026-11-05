package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.5.0", "2.4.0", true}, {"v2.10.0", "2.9.3", true}, {"2.4.0", "2.4.0", false},
		{"2.4.0", "2.5.0", false}, {"3", "2.99.99", true}, {"2.5.0-beta", "2.4.9", true},
	} {
		if got := NewerVersion(c.a, c.b); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	m := ParseSums(h + "  ReKapture-Setup-2.5.0.exe\n" + strings.Repeat("cd", 32) + " *ReKapture-Portable-2.5.0.zip\r\nbad line\n")
	if m["rekapture-setup-2.5.0.exe"] != h || m["rekapture-portable-2.5.0.zip"] != strings.Repeat("cd", 32) || len(m) != 2 {
		t.Errorf("%v", m)
	}
}

// fakeGitHub serves a latest release with a setup file and checksums.
func fakeGitHub(t *testing.T, version string, payload []byte, sumOverride string) *httptest.Server {
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])
	if sumOverride != "" {
		hexsum = sumOverride
	}
	name := "ReKapture-Setup-" + version + ".exe"
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+UpdateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "ReKapture/") {
			http.Error(w, "no UA", 403)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v%s","body":"- Faster\r\n- Better","html_url":"https://github.com/%s/releases/tag/v%s",
			"assets":[{"name":%q,"browser_download_url":"%s/dl/setup","size":%d},
			{"name":"SHA256SUMS.txt","browser_download_url":"%s/dl/sums","size":100}]}`,
			version, UpdateRepo, version, name, srv.URL, len(payload), srv.URL)
	})
	mux.HandleFunc("/dl/setup", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%s  %s\n", hexsum, name) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdaterCheckAndDownload(t *testing.T) {
	payload := []byte(strings.Repeat("installer bytes ", 50000))
	srv := fakeGitHub(t, "2.5.0", payload, "")
	var phases []string
	u := &Updater{Current: "2.4.0", Dir: t.TempDir(), API: srv.URL, OnChange: func(s UpdateState) {
		if len(phases) == 0 || phases[len(phases)-1] != s.Phase {
			phases = append(phases, s.Phase)
		}
	}}
	ok, err := u.Check(context.Background())
	if err != nil || !ok {
		t.Fatalf("check: %v %v", ok, err)
	}
	st := u.State()
	if st.Latest != "2.5.0" || st.Phase != "available" || st.Notes != "- Faster\n- Better" {
		t.Errorf("state %+v", st)
	}
	path, err := u.Download()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(payload) || filepath.Base(path) != "ReKapture-Setup-2.5.0.exe" {
		t.Errorf("downloaded %s (%d bytes)", path, len(b))
	}
	if got := strings.Join(phases, ","); got != "checking,available,downloading,ready" {
		t.Errorf("phases %s", got)
	}

	// Already up to date.
	u2 := &Updater{Current: "2.5.0", Dir: t.TempDir(), API: srv.URL}
	if ok, err := u2.Check(context.Background()); ok || err != nil || u2.State().Phase != "current" {
		t.Errorf("up to date: %v %v %+v", ok, err, u2.State())
	}
}

func TestUpdaterRejectsBadChecksum(t *testing.T) {
	srv := fakeGitHub(t, "2.5.0", []byte("real file"), strings.Repeat("0", 64))
	u := &Updater{Current: "2.4.0", Dir: t.TempDir(), API: srv.URL}
	if ok, _ := u.Check(context.Background()); !ok {
		t.Fatal("no update found")
	}
	if _, err := u.Download(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("tampered file must be rejected, got %v", err)
	}
	if entries, _ := os.ReadDir(u.Dir); len(entries) != 0 {
		t.Errorf("rejected file left behind: %v", entries)
	}
}

func TestUpdaterNoReleasesYet(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	u := &Updater{Current: "2.4.0", Dir: t.TempDir(), API: srv.URL}
	if _, err := u.Check(context.Background()); err == nil || u.State().Phase != "error" {
		t.Errorf("want a clear error, got %v %+v", err, u.State())
	}
}

func TestAllowedDownload(t *testing.T) {
	good := "https://github.com/TheeBearDad/ReKapture/releases/download/v2.5.0/ReKapture-Setup-2.5.0.exe"
	if !allowedDownload(good, "") || !allowedDownload(strings.ToLower(good), "") {
		t.Error("the real release URL must be allowed")
	}
	for _, bad := range []string{
		"http://github.com/TheeBearDad/ReKapture/releases/download/v2.5.0/x.exe",
		"https://github.com/someoneelse/ReKapture/releases/download/v2.5.0/x.exe",
		"https://evil.example/TheeBearDad/ReKapture/releases/download/x.exe",
		"http://127.0.0.1:1234/dl/setup", // only allowed in tests with a fake API
	} {
		if allowedDownload(bad, "") {
			t.Errorf("%s should be refused", bad)
		}
	}
}

func TestApplyPortableUpdate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ReKapture.exe"), []byte("old exe"), 0o644)
	os.WriteFile(filepath.Join(dir, "portable.txt"), []byte("mine"), 0o644)
	os.MkdirAll(filepath.Join(dir, "Data"), 0o755)
	os.WriteFile(filepath.Join(dir, "Data", "settings.json"), []byte(`{"mine":true}`), 0o644)

	zp := filepath.Join(t.TempDir(), "p.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	add := func(name, body string) { w, _ := zw.Create(name); w.Write([]byte(body)) }
	add("ReKapture Portable/ReKapture.exe", "new exe")
	add("ReKapture Portable/ffmpeg/ffmpeg.exe", "new ffmpeg")
	add("ReKapture Portable/portable.txt", "theirs")
	add("ReKapture Portable/Data/settings.json", "theirs")
	zw.Close()
	f.Close()

	if err := ApplyPortableUpdate(zp, dir); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if read("ReKapture.exe") != "new exe" || read("ffmpeg/ffmpeg.exe") != "new ffmpeg" {
		t.Error("files not updated")
	}
	if read("ReKapture.exe.old") != "old exe" {
		t.Error("the running exe should be renamed aside, not overwritten")
	}
	if read("portable.txt") != "mine" || read("Data/settings.json") != `{"mine":true}` {
		t.Error("user's settings were touched")
	}
	RemoveOldFiles(dir)
	if _, err := os.Stat(filepath.Join(dir, "ReKapture.exe.old")); err == nil {
		t.Error("old files not cleaned up")
	}

	// A zip with escaping paths is refused and changes nothing.
	bad := filepath.Join(t.TempDir(), "bad.zip")
	f, _ = os.Create(bad)
	zw = zip.NewWriter(f)
	w, _ := zw.Create("ReKapture.exe")
	w.Write([]byte("x"))
	w, _ = zw.Create("../../evil.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if err := ApplyPortableUpdate(bad, dir); err == nil || read("ReKapture.exe") != "new exe" {
		t.Errorf("unsafe zip: err=%v", err)
	}
}
