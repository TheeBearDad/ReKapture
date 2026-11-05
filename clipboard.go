package main

import (
	"encoding/binary"
	u16 "unicode/utf16"
)

// DropFilesData builds a CF_HDROP payload (DROPFILES header + wide path list)
// for one file, the same thing Explorer puts on the clipboard when you copy
// a file. Discord attaches it when you press Ctrl+V.
func DropFilesData(path string) []byte {
	u := u16.Encode([]rune(path))
	b := make([]byte, 20+(len(u)+2)*2)
	binary.LittleEndian.PutUint32(b[0:], 20) // pFiles: offset of the file list
	// pt (8 bytes) and fNC (4 bytes) stay zero
	binary.LittleEndian.PutUint32(b[16:], 1) // fWide: UTF-16 paths
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[20+i*2:], c)
	}
	// two trailing NUL characters end the list (already zero)
	return b
}
