package main

import (
	"os"
	"path/filepath"
)

// PortableMarker next to ReKapture.exe turns on portable mode: settings,
// logs and caches go in a "Data" folder beside the exe, so the whole folder
// can move to another PC or a USB drive.
const PortableMarker = "portable.txt"

// portableDataIn returns the Data folder to use when dir holds the portable
// marker, or "" for normal (AppData) mode. If the folder can't be written,
// for example because it was copied into Program Files, it returns "" so
// ReKapture falls back to AppData instead of failing.
func portableDataIn(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, PortableMarker)); err != nil {
		return ""
	}
	data := filepath.Join(dir, "Data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		return ""
	}
	probe := filepath.Join(data, ".write-test")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return ""
	}
	os.Remove(probe)
	return data
}
