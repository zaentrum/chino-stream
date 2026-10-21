package streamcopy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/mkv"
)

// Byte ranges of real originals (MKV_SAMPLES/<name>/{size,<offset>.part}: a
// file's head and its Cues; see the mkv package's sample_test.go). Library
// media is never committed: without MKV_SAMPLES these tests are skipped.

var errHole = errors.New("outside the sampled byte ranges")

type sample struct {
	name, dir string
	size      int64
	parts     []samplePart
}

type samplePart struct {
	at   int64
	data []byte
}

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

func samples(t *testing.T) []*sample {
	t.Helper()
	root := os.Getenv("MKV_SAMPLES")
	if root == "" {
		t.Skip("MKV_SAMPLES is not set: no byte ranges of real files at hand")
	}
	dirs, _ := os.ReadDir(root)
	var out []*sample
	for _, d := range dirs {
		dir := filepath.Join(root, d.Name())
		b, err := os.ReadFile(filepath.Join(dir, "size"))
		if !d.IsDir() || err != nil {
			continue
		}
		s := &sample{name: d.Name(), dir: dir}
		s.size, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
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

// On each real file: the index is read from its head and its Cues alone
// (nothing else of the file is touched, and it takes milliseconds); its
// keyframes are the video's cues; segment 0, made from the head, holds the
// head's video and copied audio packets as ffmpeg's own demuxer reads them,
// decodes without an error, and tiles its decode timeline.
func TestRealFiles(t *testing.T) {
	ffmpeg := tool(t, "ffmpeg")
	for _, s := range samples(t) {
		t.Run(s.name, func(t *testing.T) {
			f, err := mkv.Open(s, s.size)
			if err != nil {
				t.Fatal(err)
			}
			t0 := time.Now()
			ix, err := IndexMatroska(s, s.size, Options{DurationNs: int64(f.Duration * float64(f.TimestampScale)), Path: s.name})
			took := time.Since(t0)
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			if err := ix.Check(); err != nil {
				t.Fatalf("check: %v", err)
			}
			var copied []int
			var codecs []string
			for i, a := range ix.Audio {
				if a.Copyable() {
					copied = append(copied, i)
					codecs = append(codecs, a.Codec)
				} else {
					codecs = append(codecs, "("+a.Why+")")
				}
			}
			spans := ix.Spans()
			t.Logf("%s in %v: video %s %dx%d ts %d frame %d shift %d dv %d.%d, %d keyframes (%.1f s apart), %d segments (first %.3f s), audio %v",
				f.MuxingApp, took.Round(time.Microsecond), ix.Video.Codec, ix.Video.Width, ix.Video.Height, ix.Video.Timescale, ix.Video.FrameTicks,
				ix.Video.Shift, ix.Video.DVProfile, ix.Video.DVCompatibility, len(ix.Keyframes),
				float64(ix.DurationNs)/1e9/float64(len(ix.Keyframes)), len(spans), spans[0].Dur, codecs)

			// A Dolby Vision stream: its record only in the header for a client
			// that plays it.
			if ix.Video.DVProfile > 0 && (!bytes.Contains(ix.Video.InitDV, []byte("dvvC")) && !bytes.Contains(ix.Video.InitDV, []byte("dvcC")) ||
				bytes.Contains(ix.Video.Init, []byte("dvvC")) || bytes.Contains(ix.Video.Init, []byte("dvcC"))) {
				t.Errorf("Dolby Vision %d: the record in the wrong header", ix.Video.DVProfile)
			}
			out, err := ix.Produce(s, 0, copied)
			if errors.Is(err, errHole) {
				// A segment larger than the sampled head (a remux's first one runs
				// to tens of MB): the index alone is checked.
				t.Logf("segment 0 reaches past the sampled head: not made")
				return
			}
			if err != nil {
				t.Fatalf("segment 0: %v", err)
			}
			t.Logf("segment 0: %d bytes read, video fragment %d bytes", out.Read, len(out.Video))
			// The head as a file of its own: what ffmpeg reads of it.
			head := filepath.Join(t.TempDir(), "head.mkv")
			if err := os.WriteFile(head, s.parts[0].data, 0o644); err != nil {
				t.Fatal(err)
			}
			vf := write(t, "video.mp4", append(append([]byte(nil), ix.Video.Init...), out.Video...))
			got := hashes(t, vf, "v:0", true)
			want := hashes(t, head, "v:0", true)
			if len(want) < len(got) || !reflect.DeepEqual(got, want[:len(got)]) {
				t.Errorf("video: %d packets, the head's first are not them (%d)", len(got), len(want))
			}
			checkTiling(t, vf, "v:0")
			var stderr bytes.Buffer
			cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-v", "error", "-i", vf, "-f", "null", "-")
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil || stderr.Len() > 0 {
				t.Errorf("decoding segment 0: %v %s", err, stderr.String())
			}
			// The audio: a prefix of the head's packets of the track, in step
			// with the video.
			n := 0
			for i := range ix.Audio {
				if !ix.Audio[i].Copyable() {
					continue
				}
				af := write(t, "audio"+strconv.Itoa(i)+".mp4", append(append([]byte(nil), ix.Audio[i].Init...), out.Audio[i]...))
				got := hashes(t, af, "a:0", true)
				want := hashes(t, head, "a:"+strconv.Itoa(i), true)
				if len(got) == 0 || len(want) < len(got) || !reflect.DeepEqual(got, want[:len(got)]) {
					t.Errorf("audio %d: %d packets, not the head's first (%d)", i, len(got), len(want))
				}
				checkTiling(t, af, "a:0")
				n++
			}
			if n == 0 {
				t.Logf("no audio track copied")
			}
		})
	}
}

// BenchmarkIndexRealFiles is the index of each real file made from its head
// and Cues (MKV_SAMPLES), read from memory: the work besides the reads.
func BenchmarkIndexRealFiles(b *testing.B) {
	root := os.Getenv("MKV_SAMPLES")
	if root == "" {
		b.Skip("MKV_SAMPLES is not set")
	}
	dirs, _ := os.ReadDir(root)
	for _, d := range dirs {
		dir := filepath.Join(root, d.Name())
		sizeB, err := os.ReadFile(filepath.Join(dir, "size"))
		if err != nil {
			continue
		}
		s := &sample{name: d.Name()}
		s.size, _ = strconv.ParseInt(strings.TrimSpace(string(sizeB)), 10, 64)
		parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
		for _, p := range parts {
			at, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(p), ".part"), 10, 64)
			data, _ := os.ReadFile(p)
			s.parts = append(s.parts, samplePart{at: at, data: data})
		}
		sort.Slice(s.parts, func(i, j int) bool { return s.parts[i].at < s.parts[j].at })
		b.Run(d.Name(), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f, err := mkv.Open(s, s.size)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := IndexMatroska(s, s.size, Options{DurationNs: int64(f.Duration * float64(f.TimestampScale))}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
