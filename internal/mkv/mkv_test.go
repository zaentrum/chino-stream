package mkv

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadVintAndID(t *testing.T) {
	for _, tc := range []struct {
		in      []byte
		v       uint64
		n       int
		allOnes bool
	}{
		{[]byte{0x81}, 1, 1, false},
		{[]byte{0xFF}, 127, 1, true},
		{[]byte{0x40, 0x02}, 2, 2, false},
		{[]byte{0x43, 0x20}, 800, 2, false},
		{[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, 1<<56 - 1, 8, true},
		{[]byte{0x10, 0x00, 0x01, 0x00}, 256, 4, false},
	} {
		v, n, ones, err := readVint(tc.in)
		if err != nil || v != tc.v || n != tc.n || ones != tc.allOnes {
			t.Errorf("readVint(% x) = %d, %d, %v, %v; want %d, %d, %v", tc.in, v, n, ones, err, tc.v, tc.n, tc.allOnes)
		}
	}
	if _, _, _, err := readVint([]byte{0x00, 1}); err == nil {
		t.Error("a VINT of more than 8 bytes is read")
	}
	if _, n, _, _ := readVint([]byte{0x40}); n != 0 {
		t.Error("a VINT cut short is read")
	}
	for in, want := range map[string]uint32{"\x1a\x45\xdf\xa3": idEBML, "\xa3": idSimpleBlock, "\x2a\xd7\xb1": idTSScale, "\x1f\x43\xb6\x75": idCluster} {
		id, n, err := readID([]byte(in))
		if err != nil || id != want || n != len(in) {
			t.Errorf("readID(% x) = %x, %d, %v", in, id, n, err)
		}
	}
	if s, _, _ := readSize([]byte{0xFF}); s != unknownSize {
		t.Errorf("an unknown size reads %d", s)
	}
	if v, _ := intOf([]byte{0xFF, 0x38}); v != -200 {
		t.Errorf("intOf = %d", v)
	}
}

// The lacing examples of the Matroska specification (RFC 9559, 10.3): three
// frames of 800, 500 and 1000 bytes.
func TestUnlace(t *testing.T) {
	frames := func(sizes ...int) []byte {
		var b []byte
		for i, s := range sizes {
			b = append(b, bytes.Repeat([]byte{byte(i + 1)}, s)...)
		}
		return b
	}
	payload := frames(800, 500, 1000)
	for _, tc := range []struct {
		name   string
		lacing byte
		head   []byte
		sizes  []int
		data   []byte
	}{
		{"xiph", 1, []byte{0x02, 0xFF, 0xFF, 0xFF, 0x23, 0xFF, 0xF5}, []int{800, 500, 1000}, payload},
		{"ebml", 3, []byte{0x02, 0x43, 0x20, 0x5E, 0xD3}, []int{800, 500, 1000}, payload},
		{"fixed", 2, []byte{0x02}, []int{800, 800, 800}, frames(800, 800, 800)},
		{"none", 0, nil, []int{2300}, payload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unlace(append(append([]byte(nil), tc.head...), tc.data...), tc.lacing)
			if err != nil {
				t.Fatal(err)
			}
			var sizes []int
			for _, f := range got {
				sizes = append(sizes, len(f))
			}
			if !reflect.DeepEqual(sizes, tc.sizes) || !bytes.Equal(bytes.Join(got, nil), tc.data) {
				t.Errorf("sizes %v, want %v", sizes, tc.sizes)
			}
		})
	}
	if _, err := unlace([]byte{0x02, 0xFF, 0xFF}, 1); err == nil {
		t.Error("Xiph lacing that runs out is read")
	}
	if _, err := unlace(append([]byte{0x01}, make([]byte, 5)...), 2); err == nil {
		t.Error("fixed lacing of an odd split is read")
	}
}

// Header stripping puts the bytes back that the muxer took off every frame.
func TestHeaderStripping(t *testing.T) {
	tr := Track{Number: 2, Encodings: []ContentEncoding{{Scope: 1, Type: 0, CompAlgo: compHeaderStripping, CompSettings: []byte{0x0B, 0x77}}}}
	got, err := tr.decode([]byte{1, 2, 3})
	if err != nil || !bytes.Equal(got, []byte{0x0B, 0x77, 1, 2, 3}) || !tr.Decodable() {
		t.Errorf("decode = % x, %v", got, err)
	}
	enc := Track{Encodings: []ContentEncoding{{Scope: 1, Type: 1}}}
	if enc.Decodable() {
		t.Error("an encrypted track is decodable")
	}
}

// makeMKV makes a Matroska file with a real ffmpeg (skipping the test without
// one): args after the inputs, the output last.
func makeMKV(t *testing.T, args ...string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on $PATH")
	}
	out := filepath.Join(t.TempDir(), "film.mkv")
	cmd := exec.Command(ffmpeg, append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...), "-y", out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(b), "Unknown encoder") {
			t.Skip(string(b))
		}
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	return out
}

// packetHashes are the MD5s of the packets of a stream of a file, in file
// order, as ffmpeg's own demuxer reads them.
func packetHashes(t *testing.T, file, stream string) []string {
	t.Helper()
	ffmpeg, _ := exec.LookPath("ffmpeg")
	out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", file, "-map", "0:"+stream,
		"-c", "copy", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("framemd5: %v", err)
	}
	var hashes []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		// stream, dts, pts, duration, size, hash[, side data…]
		f := strings.Split(l, ",")
		if len(f) < 6 {
			t.Fatalf("framemd5 line %q", l)
		}
		hashes = append(hashes, strings.TrimSpace(f[5]))
	}
	return hashes
}

func openFile(t *testing.T, path string) (*File, *os.File) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fh.Close() })
	st, _ := fh.Stat()
	f, err := Open(fh, st.Size())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f, fh
}

// A file ffmpeg wrote: its tracks and index as ffmpeg described them, and its
// blocks, read from the first Cluster on, hold exactly the packets ffmpeg's
// own demuxer reads, track by track in file order; every keyframe of the
// video is a cue point, and every cue names a Cluster holding that keyframe.
func TestReadFFmpegFile(t *testing.T) {
	file := makeMKV(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24000/1001:duration=12",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12",
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration=12",
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "3", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-c:a:0", "eac3", "-ac:a:0", "6", "-c:a:1", "aac", "-ac:a:1", "2",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=ger", "-cluster_time_limit", "1000")
	f, fh := openFile(t, file)
	if f.DocType != "matroska" || f.TimestampScale != 1_000_000 || f.Duration < 11000 || len(f.Tracks) != 3 || f.FirstCluster < 0 {
		t.Fatalf("file %+v", f)
	}
	v, e, a := f.Tracks[0], f.Tracks[1], f.Tracks[2]
	if v.Type != TrackVideo || v.CodecID != "V_MPEG4/ISO/AVC" || len(v.CodecPrivate) == 0 || v.Video == nil || v.Video.PixelWidth != 320 ||
		v.DefaultDuration != 41708333 {
		t.Errorf("video %+v %+v", v, v.Video)
	}
	if e.Type != TrackAudio || e.CodecID != "A_EAC3" || e.Audio == nil || e.Audio.Channels != 6 || e.Audio.SamplingFrequency != 48000 || e.Language != "eng" {
		t.Errorf("E-AC-3 %+v %+v", e, e.Audio)
	}
	if a.CodecID != "A_AAC" || len(a.CodecPrivate) < 2 || a.Language != "ger" {
		t.Errorf("AAC %+v", a)
	}
	if len(f.Cues) == 0 || f.CuesPos < 0 {
		t.Fatalf("no cues")
	}

	got := map[uint64][]string{}
	var keyframes []int64
	rd := f.Blocks(fh, f.FirstCluster)
	rd.Chunk = 64 << 10 // blocks across many reads
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range b.Frames {
			s := md5.Sum(fr)
			got[b.Track] = append(got[b.Track], hex.EncodeToString(s[:]))
		}
		if b.Track == v.Number && b.Keyframe {
			keyframes = append(keyframes, b.Timestamp)
		}
	}
	for i, stream := range []string{"v:0", "a:0", "a:1"} {
		want := packetHashes(t, file, stream)
		if n := f.Tracks[i].Number; !reflect.DeepEqual(got[n], want) {
			t.Errorf("%s: %d frames read, ffmpeg reads %d (equal: %v)", stream, len(got[n]), len(want), reflect.DeepEqual(got[n], want))
		}
	}
	var cued []int64
	for _, c := range f.Cues {
		for _, p := range c.Positions {
			if p.Track != v.Number {
				continue
			}
			cued = append(cued, int64(c.Time))
			// The cue's Cluster holds its keyframe.
			crd := f.Blocks(fh, f.SegmentData+int64(p.Cluster))
			found := false
			for {
				b, err := crd.Next()
				if err != nil || b.Cluster != f.SegmentData+int64(p.Cluster) {
					break
				}
				if b.Track == v.Number && b.Keyframe && b.Timestamp == int64(c.Time) {
					found = true
					if p.Relative > 0 && b.Pos != f.SegmentData+int64(p.Cluster)+int64(p.Relative)+clusterHeaderLen(t, fh, f.SegmentData+int64(p.Cluster)) {
						t.Errorf("cue %d: relative position %d, the block is at %d", c.Time, p.Relative, b.Pos)
					}
					break
				}
			}
			if !found {
				t.Errorf("cue at %d: its Cluster at %d does not hold the keyframe", c.Time, p.Cluster)
			}
		}
	}
	if !reflect.DeepEqual(cued, keyframes) {
		t.Errorf("cues %v, keyframes %v", cued, keyframes)
	}
}

// clusterHeaderLen is the length of the header of the Cluster at pos: a
// CueRelativePosition counts from the end of it.
func clusterHeaderLen(t *testing.T, r io.ReaderAt, pos int64) int64 {
	b := make([]byte, 12)
	if _, err := r.ReadAt(b, pos); err != nil {
		t.Fatal(err)
	}
	h, ok, err := readHeader(b)
	if !ok || err != nil {
		t.Fatalf("cluster header at %d: %v", pos, err)
	}
	return int64(h.hlen)
}

// A file written live (no seeking back: no Cues, Clusters of unknown size) is
// read all the same, and Open says it has no index.
func TestReadLiveFile(t *testing.T) {
	file := makeMKV(t, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=6",
		"-f", "lavfi", "-i", "sine=sample_rate=48000:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-c:a", "aac", "-live", "1", "-f", "matroska")
	f, fh := openFile(t, file)
	if f.Cues != nil || f.CuesPos >= 0 {
		t.Errorf("a live file has cues: %d", len(f.Cues))
	}
	n := map[uint64]int{}
	rd := f.Blocks(fh, f.FirstCluster)
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n[b.Track] += len(b.Frames)
	}
	for i, stream := range []string{"v:0", "a:0"} {
		if want := len(packetHashes(t, file, stream)); n[f.Tracks[i].Number] != want {
			t.Errorf("%s: %d frames, ffmpeg reads %d", stream, n[f.Tracks[i].Number], want)
		}
	}
}

// Damage is an error, never a panic: a file cut at every length, and bytes
// flipped through its head.
func TestDamagedFiles(t *testing.T) {
	file := makeMKV(t, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "25")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	walk := func(data []byte) {
		f, err := Open(bytes.NewReader(data), int64(len(data)))
		if err != nil || f.FirstCluster < 0 {
			return
		}
		rd := f.Blocks(bytes.NewReader(data), f.FirstCluster)
		for i := 0; i < 10000; i++ {
			if _, err := rd.Next(); err != nil {
				return
			}
		}
		t.Fatalf("no end to the blocks of %d bytes", len(data))
	}
	for n := 0; n < len(b); n += 1 + n/7 {
		walk(b[:n])
	}
	for i := 0; i < min(len(b), 4096); i += 3 {
		c := append([]byte(nil), b...)
		c[i] ^= 0xA5
		walk(c)
	}
}
