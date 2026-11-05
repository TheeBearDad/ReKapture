package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Segment is one finished chunk of the rolling buffer.
type Segment struct {
	Index int
	File  string // absolute path
	Start float64
	End   float64
}

// ParseSegmentList reads ffmpeg's CSV segment list ("name,start,end" per
// line). Only finished segments appear in it.
func ParseSegmentList(dir string, data string) []Segment {
	var out []Segment
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// File names never contain commas (we choose them), so a plain split is safe.
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		name := strings.Trim(parts[0], "\"")
		st, err1 := strconv.ParseFloat(parts[1], 64)
		en, err2 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		idx := -1
		base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
		if i := strings.LastIndex(base, "_"); i >= 0 {
			if n, err := strconv.Atoi(base[i+1:]); err == nil {
				idx = n
			}
		}
		path := name
		if !filepath.IsAbs(path) && !strings.Contains(path, "\\") {
			path = filepath.Join(dir, name)
		}
		out = append(out, Segment{Index: idx, File: path, Start: st, End: en})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// Buffer tracks finished segments on disk and deletes the ones that fall
// out of the replay window, unless a recording or a save still needs them.
type Buffer struct {
	mu          sync.Mutex
	dir         string
	segs        []Segment
	deleted     map[int]bool
	locks       map[int]int
	keepSeconds float64
	protectFrom float64 // segments ending after this time are kept (recording); <0 = none
	remove      func(string) error
}

func NewBuffer(dir string, keepSeconds float64) *Buffer {
	return &Buffer{dir: dir, deleted: map[int]bool{}, locks: map[int]int{}, keepSeconds: keepSeconds, protectFrom: -1, remove: os.Remove}
}

func (b *Buffer) Dir() string { return b.dir }

func (b *Buffer) SetKeep(sec float64) {
	b.mu.Lock()
	b.keepSeconds = sec
	b.mu.Unlock()
}

func (b *Buffer) SetProtect(from float64) {
	b.mu.Lock()
	b.protectFrom = from
	b.mu.Unlock()
}

// Refresh re-reads the segment list and prunes old segments.
func (b *Buffer) Refresh() {
	data, err := os.ReadFile(filepath.Join(b.dir, "list.csv"))
	if err != nil {
		return
	}
	segs := ParseSegmentList(b.dir, string(data))
	b.mu.Lock()
	defer b.mu.Unlock()
	b.segs = b.segs[:0]
	for _, s := range segs {
		if !b.deleted[s.Index] {
			b.segs = append(b.segs, s)
		}
	}
	b.pruneLocked()
}

func (b *Buffer) pruneLocked() {
	if len(b.segs) == 0 {
		return
	}
	latest := b.segs[len(b.segs)-1].End
	// Keep two extra segments of margin so a save in progress never races a delete.
	cutoff := latest - b.keepSeconds - 2*SegmentSeconds
	kept := b.segs[:0]
	for _, s := range b.segs {
		protected := b.protectFrom >= 0 && s.End > b.protectFrom
		if s.End < cutoff && !protected && b.locks[s.Index] == 0 {
			if err := b.remove(s.File); err == nil || os.IsNotExist(err) {
				b.deleted[s.Index] = true
				continue
			}
		}
		kept = append(kept, s)
	}
	b.segs = kept
}

// Latest returns the end time of the newest finished segment.
func (b *Buffer) Latest() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.segs) == 0 {
		return -1
	}
	return b.segs[len(b.segs)-1].End
}

// BufferedSeconds is how much replay is currently available.
func (b *Buffer) BufferedSeconds() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.segs) == 0 {
		return 0
	}
	d := b.segs[len(b.segs)-1].End - b.segs[0].Start
	if d < 0 {
		return 0
	}
	return d
}

// SelectRange locks and returns segments overlapping [from, to].
// Pass to < 0 for "up to the newest segment".
func (b *Buffer) SelectRange(from, to float64) []Segment {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Segment
	for _, s := range b.segs {
		if s.End <= from {
			continue
		}
		if to >= 0 && s.Start >= to {
			continue
		}
		out = append(out, s)
		b.locks[s.Index]++
	}
	return out
}

// SelectLast locks and returns the newest segments covering `seconds`,
// ending at or before `until` (newest if until < 0).
func (b *Buffer) SelectLast(seconds, until float64) []Segment {
	b.mu.Lock()
	defer b.mu.Unlock()
	end := -1
	for i := len(b.segs) - 1; i >= 0; i-- {
		if until < 0 || b.segs[i].Start < until {
			end = i
			break
		}
	}
	if end < 0 {
		return nil
	}
	limit := b.segs[end].End - seconds
	start := end
	for start > 0 && b.segs[start].Start > limit+0.05 {
		start--
	}
	out := append([]Segment(nil), b.segs[start:end+1]...)
	for _, s := range out {
		b.locks[s.Index]++
	}
	return out
}

func (b *Buffer) Unlock(segs []Segment) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range segs {
		if b.locks[s.Index] > 0 {
			b.locks[s.Index]--
		}
	}
}

// WriteConcatList writes an ffconcat file for the given segments.
func WriteConcatList(path string, segs []Segment) error {
	var sb strings.Builder
	sb.WriteString("ffconcat version 1.0\n")
	for _, s := range segs {
		p := strings.ReplaceAll(filepath.ToSlash(s.File), "'", "'\\''")
		sb.WriteString("file '" + p + "'\n")
		// Exact chunk length from the segment list; without it the joiner uses
		// the longest stream (audio can overhang by part of a frame) and small
		// gaps accumulate at every join.
		if d := s.End - s.Start; d > 0 {
			sb.WriteString(fmt.Sprintf("duration %.6f\n", d))
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
