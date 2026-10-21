package mkv

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests in this file read byte ranges of real originals: the head of a
// file (its EBML header, SeekHead, Info, Tracks and first Clusters) and its
// Cues, each a part file named by its offset in a folder holding the file's
// size, as MKV_SAMPLES/<name>/{size,<offset>.part}. Media of a library is
// never committed: without MKV_SAMPLES the tests are skipped.

// errHole is a read of bytes no part holds.
var errHole = errors.New("outside the sampled byte ranges")

// sample is a file of which only some byte ranges are at hand.
type sample struct {
	name  string
	size  int64
	parts []samplePart
}

type samplePart struct {
	at   int64
	data []byte
}

// ReadAt reads from the parts; a read that touches a hole reads up to it and
// fails with errHole.
func (s *sample) ReadAt(b []byte, off int64) (int, error) {
	n := 0
	for n < len(b) {
		at := off + int64(n)
		if at >= s.size {
			return n, io.EOF
		}
		var p *samplePart
		for i := range s.parts {
			if at >= s.parts[i].at && at < s.parts[i].at+int64(len(s.parts[i].data)) {
				p = &s.parts[i]
				break
			}
		}
		if p == nil {
			return n, errHole
		}
		n += copy(b[n:], p.data[at-p.at:])
	}
	return n, nil
}

// samples are the sampled files under MKV_SAMPLES; the test is skipped
// without any.
func samples(t *testing.T) []*sample {
	t.Helper()
	root := os.Getenv("MKV_SAMPLES")
	if root == "" {
		t.Skip("MKV_SAMPLES is not set: no byte ranges of real files at hand")
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []*sample
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		b, err := os.ReadFile(filepath.Join(dir, "size"))
		if err != nil {
			continue
		}
		s := &sample{name: d.Name()}
		if s.size, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil {
			t.Fatal(err)
		}
		parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
		for _, p := range parts {
			at, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(p), ".part"), 10, 64)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			s.parts = append(s.parts, samplePart{at: at, data: data})
		}
		sort.Slice(s.parts, func(i, j int) bool { return s.parts[i].at < s.parts[j].at })
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Skip("no samples under MKV_SAMPLES")
	}
	return out
}

// headEnd is where the first part of s ends.
func (s *sample) headEnd() int64 { return s.parts[0].at + int64(len(s.parts[0].data)) }

// On each real file: Open reads its header, tracks and Cues from its head
// and from where its SeekHead says the Cues are — nothing else of the file
// is touched (a read of anything else would hit a hole) — in milliseconds;
// the video's cue points are in order, each names a Cluster, and every one
// whose Cluster the sample holds is that Cluster's video keyframe at that
// time.
func TestRealCues(t *testing.T) {
	for _, s := range samples(t) {
		t.Run(s.name, func(t *testing.T) {
			t0 := time.Now()
			f, err := Open(s, s.size)
			took := time.Since(t0)
			if err != nil {
				t.Fatal(err)
			}
			var video *Track
			for i := range f.Tracks {
				if f.Tracks[i].Type == TrackVideo {
					video = &f.Tracks[i]
					break
				}
			}
			if video == nil || len(f.Cues) == 0 || f.CuesPos < s.headEnd() {
				t.Fatalf("video %v, %d cues at %d", video, len(f.Cues), f.CuesPos)
			}
			var times []uint64
			checked := 0
			prev := int64(-1)
			for _, c := range f.Cues {
				for _, p := range c.Positions {
					if p.Track != video.Number {
						continue
					}
					times = append(times, c.Time)
					at := f.SegmentData + int64(p.Cluster)
					if at < prev || at >= s.size {
						t.Fatalf("cue at %d: its Cluster at %d (the one before at %d)", c.Time, at, prev)
					}
					prev = at
					if at+1<<20 > s.headEnd() {
						continue // not in the sample
					}
					rd := f.Blocks(s, at)
					found := false
					for {
						b, err := rd.Next()
						if err != nil || b.Cluster != at {
							break
						}
						if b.Track == video.Number && b.Keyframe && b.Timestamp == int64(c.Time) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("cue at %d: no keyframe at that time in its Cluster at %d", c.Time, at)
					}
					checked++
				}
			}
			if !sort.SliceIsSorted(times, func(i, j int) bool { return times[i] < times[j] }) || len(times) < 10 {
				t.Errorf("%d video cue times, sorted %v", len(times), sort.SliceIsSorted(times, func(i, j int) bool { return times[i] < times[j] }))
			}
			if checked == 0 {
				t.Error("no cue's Cluster in the sampled head")
			}
			t.Logf("%s: %d video cues (%d checked in the head), read in %v", f.MuxingApp, len(times), checked, took.Round(time.Microsecond))
		})
	}
}
