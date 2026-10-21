package streamcopy

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/zaentrum/chino-stream/internal/mkv"
)

// Files the copy must not trip over: damaged, oddly stored or oddly timed
// originals, each made here from an ffmpeg-made file.

// blockAt is a block of a Matroska file and where it is.
type blockAt struct {
	track    uint64
	ts       int64
	key      bool
	pos      int64 // its element
	cluster  int64
	frameLen int
	frames   [][]byte
}

// blocksOf lists every block of the Matroska file.
func blocksOf(t testing.TB, file string) (*mkv.File, []blockAt) {
	t.Helper()
	fh, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	f, err := mkv.Open(fh, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	rd := f.Blocks(fh, f.FirstCluster)
	var out []blockAt
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if len(b.Frames) > 0 {
			n = len(b.Frames[0])
		}
		var frames [][]byte
		for _, fr := range b.Frames {
			frames = append(frames, append([]byte(nil), fr...))
		}
		out = append(out, blockAt{track: b.Track, ts: b.Timestamp, key: b.Keyframe, pos: b.Pos, cluster: b.Cluster, frameLen: n, frames: frames})
	}
	return f, out
}

// gopFilm is a 23.976 fps H.264 film of dur seconds with a keyframe every 2
// s and no B-frames, and a stereo AAC track.
func gopFilm(t *testing.T, dur int) string {
	return makeFile(t, "film.mkv", append(lavfi(dur), "-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-bf", "0",
		"-c:a", "aac", "-ac", "2")...)
}

// A frame missing from an audio track (its SimpleBlock made a Void): every
// frame after it keeps its own time — the gap stays a gap — so the audio stays
// in step with the video.
func TestAudioGapKeepsItsTime(t *testing.T) {
	file := gopFilm(t, 20)
	_, blocks := blocksOf(t, file)
	var victim *blockAt
	for i := range blocks {
		if blocks[i].track == 2 && blocks[i].ts >= 8000 {
			victim = &blocks[i]
			break
		}
	}
	b, _ := os.ReadFile(file)
	if victim == nil || b[victim.pos] != 0xA3 {
		t.Fatal("no audio SimpleBlock at 8 s")
	}
	b[victim.pos] = 0xEC // Void
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	ix := index(t, file)
	_, aud, _ := produceAll(t, ix, []int{0})
	copied := packets(t, write(t, "audio.mp4", aud[0]), "a:0")
	orig := packets(t, file, "a:0")
	if len(copied) != len(orig) {
		t.Fatalf("%d packets copied, the original has %d", len(copied), len(orig))
	}
	ts := int64(ix.Audio[0].Timescale)
	for i := range orig {
		wantNs := orig[i].PTS*1e6 - ix.OriginNs
		gotNs := copied[i].PTS * 1e9 / ts
		if d := gotNs - wantNs; d > 1_500_000 || d < -1_500_000 {
			t.Fatalf("audio frame %d presented at %.1f ms, the original's at %.1f ms", i, float64(gotNs)/1e6, float64(wantNs)/1e6)
		}
	}
}

// Two video frames presented at one time: the segment that holds them fails
// as the file's (mkv.ErrFormat), every time, and the others are made.
func TestTwoFramesAtOneTimeAreTheFiles(t *testing.T) {
	file := gopFilm(t, 20)
	_, blocks := blocksOf(t, file)
	b, _ := os.ReadFile(file)
	var prev *blockAt
	patched := false
	for i := range blocks {
		bl := &blocks[i]
		if bl.track != 1 {
			continue
		}
		if bl.ts >= 13000 && !bl.key && prev != nil && prev.cluster == bl.cluster {
			// SimpleBlock: ID a3, its size, the track (one byte), then the
			// timestamp relative to the Cluster's: set to the frame before's.
			p := bl.pos + 1
			l := int64(1)
			for m := byte(0x80); b[p]&m == 0; m >>= 1 {
				l++
			}
			p += l + 1 // past the size and the track
			rel := int64(int16(binary.BigEndian.Uint16(b[p:])))
			binary.BigEndian.PutUint16(b[p:], uint16(prev.ts-(bl.ts-rel)))
			patched = true
			break
		}
		prev = bl
	}
	if !patched {
		t.Fatal("nothing patched")
	}
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	ix := index(t, file)
	fh, _ := os.Open(file)
	defer fh.Close()
	failed := 0
	for n := range ix.Spans() {
		for try := 0; try < 2; try++ {
			if _, err := ix.Produce(fh, n, []int{0}); err != nil {
				if !errors.Is(err, mkv.ErrFormat) {
					t.Errorf("segment %d: %v, not the file's", n, err)
				}
				failed++
			}
		}
	}
	if failed != 2 {
		t.Errorf("%d failures in two tries of every segment, want the one segment's two", failed)
	}
}

// A Matroska file whose AC-3 track is stored with header stripping (each
// frame's syncword taken off, as mkvmerge stores it): every copied frame is
// whole again, the packets ffmpeg reads of the file.
func TestHeaderStrippedAC3(t *testing.T) {
	src := makeFile(t, "src.mkv", append(lavfi(14)[:8], "-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-bf", "0",
		"-c:a", "ac3", "-ac", "2", "-b:a", "192k")...)
	f, blocks := blocksOf(t, src)
	vt, at := f.Tracks[0], f.Tracks[1]
	if vt.CodecID != "V_MPEG4/ISO/AVC" || at.CodecID != "A_AC3" {
		t.Fatalf("tracks %s %s", vt.CodecID, at.CodecID)
	}
	file := write(t, "stripped.mkv", strippedFile(t, f, blocks, 2, []byte{0x0B, 0x77}))
	ix := index(t, file)
	if !ix.Audio[0].Copyable() {
		t.Fatalf("the AC-3 track is not copied: %s", ix.Audio[0].Why)
	}
	_, aud, segs := produceAll(t, ix, []int{0})
	mf, err := mp4.DecodeFile(bytes.NewReader(append(append([]byte(nil), ix.Audio[0].Init...), segs[0].Audio[0]...)))
	if err != nil {
		t.Fatal(err)
	}
	fs, err := mf.Segments[0].Fragments[0].GetFullSamples(nil)
	if err != nil || len(fs) == 0 || !bytes.HasPrefix(fs[0].Data, []byte{0x0B, 0x77}) {
		t.Fatalf("the first copied AC-3 frame does not start with its syncword (%v)", err)
	}
	if want, got := hashes(t, file, "a:0", true), hashes(t, write(t, "audio.mp4", aud[0]), "a:0", true); !reflect.DeepEqual(got, want) {
		t.Errorf("%d packets copied, ffmpeg reads %d of the file (equal: %v)", len(got), len(want), reflect.DeepEqual(got, want))
	}
}

// strippedFile writes the blocks of f anew, a Cluster per video keyframe, the
// frames of track stripped of prefix and the track's header stripping said
// in its ContentEncodings, with Cues.
func strippedFile(t *testing.T, f *mkv.File, blocks []blockAt, track uint64, prefix []byte) []byte {
	t.Helper()
	type cluster struct {
		ts, kf int64
		video  bool
		blocks []blockAt
	}
	var cls []cluster
	vt := f.Tracks[0]
	for _, b := range blocks {
		// A new Cluster at each video keyframe after the first video block.
		if len(cls) == 0 || (b.track == vt.Number && b.key && cls[len(cls)-1].video) {
			cls = append(cls, cluster{ts: b.ts, kf: -1})
		}
		c := &cls[len(cls)-1]
		if b.track == vt.Number {
			c.video = true
			if b.key && c.kf < 0 {
				c.kf = b.ts
			}
		}
		c.blocks = append(c.blocks, b)
	}
	var data [][]byte
	for _, c := range cls {
		parts := [][]byte{ebmlUint(0xE7, uint64(c.ts))}
		for _, b := range c.blocks {
			frame := b.frames[0]
			if b.track == track {
				if !bytes.HasPrefix(frame, prefix) {
					t.Fatalf("a frame of track %d without % x", track, prefix)
				}
				frame = frame[len(prefix):]
			}
			flags := byte(0)
			if b.key {
				flags = 0x80
			}
			rel := b.ts - c.ts
			parts = append(parts, ebml(0xA3, append([]byte{0x80 | byte(b.track), byte(uint16(rel) >> 8), byte(rel), flags}, frame...)))
		}
		data = append(data, ebml(0x1F43B675, parts...))
	}
	at := f.Track(track)
	info := ebml(0x1549A966, ebmlUint(0x2AD7B1, uint64(f.TimestampScale)), ebmlFloat(0x4489, f.Duration))
	video := ebml(0xAE, ebmlUint(0xD7, vt.Number), ebmlUint(0x73C5, 1), ebmlUint(0x83, 1), ebml(0x86, []byte(vt.CodecID)),
		ebml(0x63A2, vt.CodecPrivate), ebmlUint(0x23E383, vt.DefaultDuration),
		ebml(0xE0, ebmlUint(0xB0, vt.Video.PixelWidth), ebmlUint(0xBA, vt.Video.PixelHeight)))
	audio := ebml(0xAE, ebmlUint(0xD7, at.Number), ebmlUint(0x73C5, 2), ebmlUint(0x83, 2), ebml(0x86, []byte(at.CodecID)),
		ebml(0xE1, ebmlFloat(0xB5, at.Audio.SamplingFrequency), ebmlUint(0x9F, at.Audio.Channels)),
		ebml(0x6D80, ebml(0x6240, ebmlUint(0x5031, 0), ebmlUint(0x5032, 1), ebmlUint(0x5033, 0),
			ebml(0x5034, ebmlUint(0x4254, 3), ebml(0x4255, prefix)))))
	tracks := ebml(0x1654AE6B, video, audio)
	cues := func(first int64) []byte {
		var points [][]byte
		pos := first
		for i, c := range cls {
			points = append(points, ebml(0xBB, ebmlUint(0xB3, uint64(c.kf)), ebml(0xB7, ebmlUint(0xF7, vt.Number), ebmlUint(0xF1, uint64(pos)))))
			pos += int64(len(data[i]))
		}
		return ebml(0x1C53BB6B, points...)
	}
	head := int64(len(info) + len(tracks))
	first := head + int64(len(cues(0)))
	segment := bytes.Join(append([][]byte{info, tracks, cues(first)}, data...), nil)
	return append(ebml(0x1A45DFA3, ebml(0x4282, []byte("matroska"))), ebml(0x18538067, segment)...)
}

// ebml is an element of a known size (written in 8 bytes).
func ebml(id uint32, payload ...[]byte) []byte {
	var idb []byte
	for v := id; v > 0; v >>= 8 {
		idb = append([]byte{byte(v)}, idb...)
	}
	p := bytes.Join(payload, nil)
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(p)))
	size[0] = 0x01
	return append(append(idb, size...), p...)
}

func ebmlUint(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return ebml(id, b)
}

func ebmlFloat(id uint32, f float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(f))
	return ebml(id, b)
}

// An MP4 whose HEVC is in a Dolby Vision sample entry (dvh1, with its
// record): every other client's header is the base layer's hvc1 entry, its
// CODECS an HEVC one; a client that plays Dolby Vision gets the dvh1 entry
// with the record and the Dolby Vision CODECS.
func TestDolbyVisionSampleEntryOfAnMP4(t *testing.T) {
	src := makeFile(t, "hevc.mp4", append(lavfi(8)[:4], "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "keyint=48:min-keyint=48:log-level=error", "-tag:v", "hvc1", "-an")...)
	for _, tc := range []struct {
		profile, compat int
		box, codecDV    string
	}{{8, 1, "dvvC", "dvh1.08.06"}, {5, 0, "dvcC", "dvh1.05.06"}} {
		file := write(t, "dv.mp4", dolbyVisionEntry(t, src, "dvh1", tc.box, dvRecordOf(tc.profile, 6, tc.compat)))
		ix := index(t, file)
		if !strings.HasPrefix(ix.Video.Codec, "hvc1.") || ix.Video.CodecDV != tc.codecDV || ix.Video.DVProfile != tc.profile {
			t.Errorf("profile %d: CODECS %q, for Dolby Vision %q (profile %d)", tc.profile, ix.Video.Codec, ix.Video.CodecDV, ix.Video.DVProfile)
		}
		plain, withDV := string(ix.Video.Init), string(ix.Video.InitDV)
		if !strings.Contains(plain, "hvc1") || strings.Contains(plain, "dvh1") || strings.Contains(plain, tc.box) {
			t.Errorf("profile %d: the header for another client is not the base layer's hvc1 entry", tc.profile)
		}
		if !strings.Contains(withDV, "dvh1") || !strings.Contains(withDV, tc.box) {
			t.Errorf("profile %d: the Dolby Vision header has no dvh1 entry with its %s", tc.profile, tc.box)
		}
		// The base layer's header plays: segment 0 decodes with it.
		video, _, _ := produceAll(t, ix, nil)
		checkTiling(t, write(t, "video.mp4", video), "v:0")
	}
	// A Dolby Vision entry without its record: not copied.
	file := write(t, "dv.mp4", dolbyVisionEntry(t, src, "dvh1", "", nil))
	fh, _ := os.Open(file)
	defer fh.Close()
	st, _ := fh.Stat()
	if _, err := IndexMP4(fh, st.Size(), Options{DurationNs: 8e9, Path: file}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a dvh1 entry without its record: %v", err)
	}
}

// dolbyVisionEntry is the MP4 src (its moov after its mdat) with its video's
// sample entry of type entry and, unless box is "", the Dolby Vision record
// rec in a box of that type.
func dolbyVisionEntry(t *testing.T, src, entry, box string, rec []byte) []byte {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range mf.Children {
		if c.Type() == "moov" && i < len(mf.Children)-1 {
			t.Fatal("the moov is before the mdat: its offsets would move")
		}
	}
	vse := mf.Moov.Traks[0].Mdia.Minf.Stbl.Stsd.Children[0].(*mp4.VisualSampleEntryBox)
	vse.SetType(entry)
	if box != "" {
		vse.AddChild(mp4.CreateUnknownBox(box, uint64(8+len(rec)), rec))
	}
	var out bytes.Buffer
	if err := mf.Encode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// An MP4 with keyframes 30 s apart: not copied (its sample tables are the
// keyframes it has: there is nothing to scan for).
func TestAnMP4WithKeyframesFarApart(t *testing.T) {
	file := makeFile(t, "sparse.mp4", append(lavfi(70)[:4], "-c:v", "libx264", "-preset", "ultrafast", "-g", "720", "-keyint_min", "720",
		"-sc_threshold", "0", "-an")...)
	fh, _ := os.Open(file)
	defer fh.Close()
	st, _ := fh.Stat()
	start, dur := probeStart(t, file)
	ix, err := IndexMP4(fh, st.Size(), Options{OriginNs: start, DurationNs: dur, Path: file})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Check(); !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNoIndex) {
		t.Errorf("check: %v, want not copied", err)
	}
}

// Sessions of an index read back from its JSON (no spans made yet) ask for
// its spans at once (go test -race).
func TestSpansAtOnce(t *testing.T) {
	ix := index(t, gopFilm(t, 20))
	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	var fresh Index
	if err := json.Unmarshal(b, &fresh); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fresh.Bitrates()
			_ = fresh.Spans()
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(fresh.Spans(), ix.Spans()) {
		t.Error("the spans of the index read back are not the index's")
	}
}

// syntheticMP4 is an MP4 index of secs seconds of 24 fps video (20 kB a
// frame, a keyframe every 2 s) and audio tracks of 400-byte AAC frames.
func syntheticMP4(secs, audioTracks int) *Index {
	v := &mp4Track{timescale: 24000}
	for i := 0; i < secs*24; i++ {
		v.samples = append(v.samples, mp4Sample{dts: int64(i) * 1000, dur: 1000, size: 20000, sync: i%48 == 0})
	}
	src := &mp4Source{video: v}
	for k := 0; k < audioTracks; k++ {
		a := &mp4Track{timescale: 48000}
		for i := 0; i < secs*48000/1024; i++ {
			a.samples = append(a.samples, mp4Sample{dts: int64(i) * 1024, dur: 1024, size: 400, sync: true})
			src.audioBytes += 400
		}
		src.audio = append(src.audio, a)
	}
	ix := &Index{Kind: KindMP4, DurationNs: int64(secs) * 1e9, Video: VideoTrack{Timescale: 24000}, mp4: src}
	for i, s := range v.samples {
		if s.sync {
			ix.Keyframes = append(ix.Keyframes, Keyframe{PTS: s.dts, TS: int64(i), Pos: int64(i)})
		}
	}
	return ix
}

// An MP4's bit rates: the video's bytes in each segment and the audio's share
// of it, the peak and the average.
func TestMP4Bitrates(t *testing.T) {
	ix := syntheticMP4(600, 2)
	peak, avg := ix.Bitrates()
	// 24 frames of 20 kB a second, and 2 × 48000/1024 frames of 400 bytes.
	want := 24*20000*8 + int(2*400*8*48000.0/1024)
	if d := avg - want; d > want/100 || d < -want/100 || peak < avg || peak > avg+avg/50 {
		t.Errorf("peak %d, average %d; want about %d", peak, avg, want)
	}
}

// BenchmarkMP4Bitrates is the cost of a 2 h film's bit rates (once per
// master): linear in its samples.
func BenchmarkMP4Bitrates(b *testing.B) {
	ix := syntheticMP4(7200, 2)
	for i := 0; i < b.N; i++ {
		ix.spansMu.Lock()
		ix.spans = nil
		ix.spansMu.Unlock()
		_, _ = ix.Bitrates()
	}
}

// Audio that ends before the video, in Matroska and in MP4: every segment
// has a fragment of every copied track — the ones after the audio's end one
// of no samples whose decode time is the segment's start — so no segment of
// the track is missing, and the track's frames are all there.
func TestAudioShorterThanTheVideoHasEverySegment(t *testing.T) {
	for _, ext := range []string{"mkv", "mp4"} {
		t.Run(ext, func(t *testing.T) {
			file := makeFile(t, "film."+ext, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=30",
				"-f", "lavfi", "-i", "sine=sample_rate=48000:duration=12",
				"-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac")
			ix := index(t, file)
			_, aud, segs := produceAll(t, ix, []int{0})
			empty := 0
			for n, s := range segs {
				frag, ok := s.Audio[0]
				if !ok || len(frag) == 0 {
					t.Fatalf("segment %d has no audio fragment", n)
				}
				mf, err := mp4.DecodeFile(bytes.NewReader(append(append([]byte(nil), ix.Audio[0].Init...), frag...)))
				if err != nil {
					t.Fatalf("segment %d: %v", n, err)
				}
				fr := mf.Segments[0].Fragments[0]
				if fr.Moof.Traf.Tfdt == nil {
					t.Fatalf("segment %d: no decode time", n)
				}
				if len(fr.Moof.Traf.Truns) > 0 {
					continue
				}
				empty++
				a := ix.Audio[0]
				want := ix.segmentStart(n, a.Timescale) + a.Base
				if ix.Kind == KindMP4 {
					want = ix.segmentStart(n, a.Timescale) + ix.mp4.audio[0].mediaTime
				}
				if got := int64(fr.Moof.Traf.Tfdt.BaseMediaDecodeTime()); got != want {
					t.Errorf("segment %d: an empty fragment at %d, the segment starts at %d", n, got, want)
				}
			}
			if empty < 2 {
				t.Fatalf("%d segments without audio among %d", empty, len(segs))
			}
			if want, got := hashes(t, file, "a:0", true), hashes(t, write(t, "audio.mp4", aud[0]), "a:0", true); !reflect.DeepEqual(got, want) {
				t.Errorf("audio: %d packets, the original %d", len(got), len(want))
			}
		})
	}
}

// The head of a file is read as far as the index needs: the first keyframe
// and each audio track's first frame, not the first seconds — for a file of
// large frames megabytes, not the 16 MiB a read of 48 of them came to.
func TestTheHeadIsReadAsFarAsNeeded(t *testing.T) {
	file := makeFile(t, "film.mkv", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=4",
		"-f", "lavfi", "-i", "sine=sample_rate=48000:duration=4", "-map", "0", "-map", "1",
		"-vf", "noise=alls=80:allf=t", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "1", "-g", "96",
		"-c:a", "ac3")
	fh, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	start, dur := probeStart(t, file)
	r := &countingReader{r: fh}
	ix, err := IndexMatroska(r, st.Size(), Options{OriginNs: start, DurationNs: dur, Path: file})
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Audio[0].Copyable() {
		t.Fatalf("the AC-3 track: %s", ix.Audio[0].Why)
	}
	if r.n > 4<<20 || st.Size() < 24<<20 {
		t.Errorf("indexing read %d bytes of a file of %d", r.n, st.Size())
	}
	t.Logf("indexing read %d bytes of %d", r.n, st.Size())
}

// zeroStream is a zlib stream of n zeros.
func zeroStream(t testing.TB, n int) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	chunk := make([]byte, 1<<20)
	for n > 0 {
		k := min(n, len(chunk))
		if _, err := w.Write(chunk[:k]); err != nil {
			t.Fatal(err)
		}
		n -= k
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlibbed(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zlibFilm writes the blocks of the Matroska file f (an H.264 track 1 and
// an AAC track 2) anew, a Cluster per video keyframe and Cues, with a
// zlib-compressed subtitle track 3 besides — the audio's frames
// zlib-compressed too with zlibAudio — and in Cluster bombAt a block of
// track bomb: frames laced frames that each inflate to size zeros.
func zlibFilm(t testing.TB, f *mkv.File, blocks []blockAt, zlibAudio bool, bomb uint64, bombAt, frames, size int) []byte {
	t.Helper()
	type cluster struct {
		ts, kf int64
		video  bool
		blocks []blockAt
	}
	var cls []cluster
	vt, at := f.Tracks[0], f.Track(2)
	for _, b := range blocks {
		if len(cls) == 0 || (b.track == vt.Number && b.key && cls[len(cls)-1].video) {
			cls = append(cls, cluster{ts: b.ts, kf: -1})
		}
		c := &cls[len(cls)-1]
		if b.track == vt.Number {
			c.video = true
			if b.key && c.kf < 0 {
				c.kf = b.ts
			}
		}
		c.blocks = append(c.blocks, b)
	}
	simple := func(track uint64, rel int64, flags byte, data []byte) []byte {
		return ebml(0xA3, append([]byte{0x80 | byte(track), byte(uint16(rel) >> 8), byte(rel), flags}, data...))
	}
	var data [][]byte
	for i, c := range cls {
		parts := [][]byte{ebmlUint(0xE7, uint64(c.ts))}
		if i == bombAt {
			// First in its Cluster: where a read of the head meets it.
			z := zeroStream(t, size)
			laced, flags := z, byte(0x80)
			if frames > 1 {
				laced, flags = append([]byte{byte(frames - 1)}, bytes.Repeat(z, frames)...), 0x80|2<<1 // fixed lacing
			}
			parts = append(parts, simple(bomb, 0, flags, laced))
		}
		for _, b := range c.blocks {
			frame, flags := b.frames[0], byte(0)
			if b.key {
				flags = 0x80
			}
			if b.track == at.Number && zlibAudio {
				frame = zlibbed(t, frame)
			}
			parts = append(parts, simple(b.track, b.ts-c.ts, flags, frame))
		}
		data = append(data, ebml(0x1F43B675, parts...))
	}
	zlibEnc := ebml(0x6D80, ebml(0x6240, ebmlUint(0x5031, 0), ebmlUint(0x5032, 1), ebmlUint(0x5033, 0), ebml(0x5034, ebmlUint(0x4254, 0))))
	info := ebml(0x1549A966, ebmlUint(0x2AD7B1, uint64(f.TimestampScale)), ebmlFloat(0x4489, f.Duration))
	video := ebml(0xAE, ebmlUint(0xD7, vt.Number), ebmlUint(0x73C5, 1), ebmlUint(0x83, 1), ebml(0x86, []byte(vt.CodecID)),
		ebml(0x63A2, vt.CodecPrivate), ebmlUint(0x23E383, vt.DefaultDuration),
		ebml(0xE0, ebmlUint(0xB0, vt.Video.PixelWidth), ebmlUint(0xBA, vt.Video.PixelHeight)))
	audioParts := [][]byte{ebmlUint(0xD7, at.Number), ebmlUint(0x73C5, 2), ebmlUint(0x83, 2), ebml(0x86, []byte(at.CodecID)),
		ebml(0x63A2, at.CodecPrivate), ebml(0xE1, ebmlFloat(0xB5, at.Audio.SamplingFrequency), ebmlUint(0x9F, at.Audio.Channels))}
	if zlibAudio {
		audioParts = append(audioParts, zlibEnc)
	}
	subs := ebml(0xAE, ebmlUint(0xD7, 3), ebmlUint(0x73C5, 3), ebmlUint(0x83, 17), ebml(0x86, []byte("S_TEXT/UTF8")), zlibEnc)
	tracks := ebml(0x1654AE6B, video, ebml(0xAE, audioParts...), subs)
	cues := func(first int64) []byte {
		var points [][]byte
		pos := first
		for i, c := range cls {
			points = append(points, ebml(0xBB, ebmlUint(0xB3, uint64(c.kf)), ebml(0xB7, ebmlUint(0xF7, vt.Number), ebmlUint(0xF1, uint64(pos)))))
			pos += int64(len(data[i]))
		}
		return ebml(0x1C53BB6B, points...)
	}
	first := int64(len(info)+len(tracks)) + int64(len(cues(0)))
	segment := bytes.Join(append([][]byte{info, tracks, cues(first)}, data...), nil)
	return append(ebml(0x1A45DFA3, ebml(0x4282, []byte("matroska"))), ebml(0x18538067, segment)...)
}

// bytesAllocated is how many bytes fn allocates.
func bytesAllocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A subtitle block in the head that inflates to 256 MiB (laced frames of
// zeros, as a file made to exhaust memory has): the index reads only the
// copied audio's first frame and the video's block headers, and inflates
// none of it; nor does a segment, which reads the subtitles no more.
func TestAZlibBombNotCopiedIsNotInflated(t *testing.T) {
	src := gopFilm(t, 20)
	f, blocks := blocksOf(t, src)
	file := write(t, "bomb.mkv", zlibFilm(t, f, blocks, false, 3, 0, 256, 1<<20))
	var ix *Index
	if n := bytesAllocated(func() { ix = index(t, file) }); n > 32<<20 {
		t.Errorf("indexing allocated %d MiB", n>>20)
	}
	fh, _ := os.Open(file)
	defer fh.Close()
	var err error
	if n := bytesAllocated(func() { _, err = ix.Produce(fh, 0, []int{0}) }); err != nil || n > 64<<20 {
		t.Errorf("segment 0: %v, %d MiB allocated", err, n>>20)
	}
}

// A copied audio track stored zlib-compressed: its frames copied inflated,
// ffmpeg's packets; a block of it that inflates to 256 MiB fails its segment
// as the file's, bounded, and the other segments are made.
func TestAZlibBombInACopiedTrackFailsItsSegment(t *testing.T) {
	src := gopFilm(t, 20)
	f, blocks := blocksOf(t, src)
	file := write(t, "bomb.mkv", zlibFilm(t, f, blocks, true, 2, 3, 256, 1<<20))
	ix := index(t, file)
	if !ix.Audio[0].Copyable() {
		t.Fatalf("the audio is not copied: %s", ix.Audio[0].Why)
	}
	fh, _ := os.Open(file)
	defer fh.Close()
	failed := 0
	for n := range ix.Spans() {
		var err error
		allocated := bytesAllocated(func() { _, err = ix.Produce(fh, n, []int{0}) })
		switch {
		case errors.Is(err, mkv.ErrFormat):
			failed++
			// A few times the block's bound (the growing buffers, the race
			// detector's share), not the bomb's 256 MiB and more.
			if allocated > 6*mkv.MaxBlockInflated {
				t.Errorf("segment %d: %d MiB allocated before it failed", n, allocated>>20)
			}
		case err != nil:
			t.Errorf("segment %d: %v", n, err)
		}
	}
	if failed != 1 {
		t.Errorf("%d segments failed, want the bomb's", failed)
	}
	// Without the bomb: every frame copied inflated, as ffmpeg reads them.
	clean := write(t, "zlib.mkv", zlibFilm(t, f, blocks, true, 3, -1, 0, 0))
	ixc := index(t, clean)
	_, aud, _ := produceAll(t, ixc, []int{0})
	if want, got := hashes(t, clean, "a:0", true), hashes(t, write(t, "audio.mp4", aud[0]), "a:0", true); !reflect.DeepEqual(got, want) {
		t.Errorf("zlib-compressed audio: %d packets copied, ffmpeg reads %d (equal: %v)", len(got), len(want), reflect.DeepEqual(got, want))
	}
}
