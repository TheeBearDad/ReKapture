package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortableData(t *testing.T) {
	dir := t.TempDir()
	if got := portableDataIn(dir); got != "" {
		t.Errorf("no marker should mean AppData mode, got %q", got)
	}
	os.WriteFile(filepath.Join(dir, PortableMarker), []byte("x"), 0o644)
	if got := portableDataIn(dir); got != filepath.Join(dir, "Data") {
		t.Errorf("marker should give the Data folder, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "Data", ".write-test")); err == nil {
		t.Error("write probe left behind")
	}
	// A Data folder that can't be created must also fall back.
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, PortableMarker), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(bad, "Data"), []byte("not a folder"), 0o644)
	if got := portableDataIn(bad); got != "" {
		t.Errorf("unwritable Data must fall back to AppData, got %q", got)
	}
	if os.Geteuid() != 0 { // root can write anywhere
		ro := t.TempDir()
		os.WriteFile(filepath.Join(ro, PortableMarker), []byte("x"), 0o644)
		os.Chmod(ro, 0o555)
		defer os.Chmod(ro, 0o755)
		if got := portableDataIn(ro); got != "" {
			t.Errorf("read-only folder must fall back to AppData, got %q", got)
		}
	}
}
