package mkv

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"runtime"
	"testing"
)

// Files written by hand, element by element: the shapes a muxer writes and
// the damage a file cut short or a broken muxer leaves, none of which may
// end in anything but an error.

// idBytes is an element ID's bytes.
func idBytes(id uint32) []byte {
	switch {
	case id >= 1<<24:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id >= 1<<16:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id >= 1<<8:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

// el is an element of a known size (written in 8 bytes) holding payload.
func el(id uint32, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	n := uint64(len(p))
	size := []byte{0x01, byte(n >> 48), byte(n >> 40), byte(n >> 32), byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(append(idBytes(id), size...), p...)
}

// unknownEl is an element whose size says unknown.
func unknownEl(id uint32, payload ...[]byte) []byte {
	return append(append(idBytes(id), 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF), bytes.Join(payload, nil)...)
}

func uintEl(id uint32, v uint64) []byte {
	return el(id, []byte{byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32), byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func strEl(id uint32, s string) []byte { return el(id, []byte(s)) }

// blockPayload is a block's payload: the track number (one byte), the
// timestamp relative to its Cluster's, the flags and the frames' data.
func blockPayload(track byte, rel int16, flags byte, data []byte) []byte {
	return append([]byte{0x80 | track, byte(uint16(rel) >> 8), byte(rel), flags}, data...)
}

func ebmlHeader() []byte { return el(idEBML, strEl(idDocType, "matroska")) }

// threeTracks are the Tracks of a video track (1), a header-stripped audio
// track (2) and a zlib-compressed subtitle track (3).
func threeTracks() []byte {
	video := el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo), strEl(idCodecID, "V_MPEG4/ISO/AVC"),
		el(idCodecPrivate, []byte{1, 0x64, 0, 0x1F, 0xFF, 0xE0}), el(idVideo, uintEl(idPixelWidth, 320), uintEl(idPixelHeight, 180)))
	audio := el(idTrackEntry, uintEl(idTrackNumber, 2), uintEl(idTrackType, TrackAudio), strEl(idCodecID, "A_AC3"),
		el(idAudio, uintEl(idChannels, 2)),
		el(idContentEncs, el(idContentEnc, uintEl(idContentEncScope, 1),
			el(idContentComp, uintEl(idContentCompAlgo, compHeaderStripping), el(idContentCompSet, []byte{0x0B, 0x77})))))
	subs := el(idTrackEntry, uintEl(idTrackNumber, 3), uintEl(idTrackType, TrackSubtitle), strEl(idCodecID, "S_TEXT/UTF8"),
		el(idContentEncs, el(idContentEnc, uintEl(idContentEncScope, 1), el(idContentComp, uintEl(idContentCompAlgo, compZlib)))))
	return el(idTracks, video, audio, subs)
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

// handMadeFile is a Matroska file with every kind of block the reader
// reads: SimpleBlocks unlaced and laced three ways, BlockGroups with a
// duration and with a reference, a Cluster of known size and one of unknown
// size, a SeekHead to the Info, the Tracks and the Cues — and, with pad > 0,
// a Void of that many bytes before the first Cluster.
func handMadeFile(t testing.TB, pad int) []byte {
	xiph := append([]byte{2, 10, 20}, bytes.Repeat([]byte{0xA1}, 10+20+5)...)          // 3 frames: 10, 20, 5
	ebml := append([]byte{2, 0x80 | 12, 0xC3}, bytes.Repeat([]byte{0xA2}, 12+16+3)...) // 12, then +4 (16), then 3
	fixed := append([]byte{3}, bytes.Repeat([]byte{0xA3}, 4*6)...)                     // 4 frames of 6
	cluster1 := el(idCluster, uintEl(idTimestamp, 0),
		el(idSimpleBlock, blockPayload(1, 0, 0x80, bytes.Repeat([]byte{0x65}, 300))),
		el(idSimpleBlock, blockPayload(2, 0, 0x80|1<<1, xiph)),
		el(idBlockGroup, el(idBlock, blockPayload(3, 10, 0, zlibbed(t, []byte("a subtitle line")))), uintEl(idBlockDuration, 900)),
		el(idSimpleBlock, blockPayload(1, 40, 0, bytes.Repeat([]byte{0x41}, 120))),
		el(idVoid, make([]byte, 7)))
	cluster2 := unknownEl(idCluster, uintEl(idTimestamp, 1000),
		el(idSimpleBlock, blockPayload(1, 0, 0x80, bytes.Repeat([]byte{0x65}, 200))),
		el(idSimpleBlock, blockPayload(2, 0, 0x80|3<<1, ebml)),
		el(idSimpleBlock, blockPayload(2, 30, 0x80|2<<1, fixed)),
		el(idBlockGroup, el(idBlock, blockPayload(1, 40, 0, bytes.Repeat([]byte{0x41}, 90))), uintEl(idReferenceBlock, 40)))
	info := el(idInfo, uintEl(idTSScale, 1_000_000), strEl(idMuxingApp, "hand"))
	tracks := threeTracks()
	var void []byte
	if pad > 0 {
		void = el(idVoid, make([]byte, pad))
	}
	seekHead := func(infoAt, tracksAt, cuesAt uint64) []byte {
		seek := func(id uint32, at uint64) []byte { return el(idSeek, el(idSeekID, idBytes(id)), uintEl(idSeekPos, at)) }
		return el(idSeekHead, seek(idInfo, infoAt), seek(idTracks, tracksAt), seek(idCues, cuesAt))
	}
	cuesOf := func(c1, c2 uint64) []byte {
		return el(idCues,
			el(idCuePoint, uintEl(idCueTime, 0), el(idCueTrackPositions, uintEl(idCueTrack, 1), uintEl(idCueClusterPos, c1))),
			el(idCuePoint, uintEl(idCueTime, 1000), el(idCueTrackPositions, uintEl(idCueTrack, 1), uintEl(idCueClusterPos, c2))))
	}
	// Every number is written in 8 bytes: no size depends on a position.
	infoAt := uint64(len(seekHead(0, 0, 0)))
	tracksAt := infoAt + uint64(len(info))
	c1At := tracksAt + uint64(len(tracks)) + uint64(len(void))
	cuesAt := c1At + uint64(len(cluster1))
	c2At := cuesAt + uint64(len(cuesOf(0, 0)))
	// The Cues before the Cluster of unknown size, which runs to the end.
	segment := bytes.Join([][]byte{seekHead(infoAt, tracksAt, cuesAt), info, tracks, void, cluster1, cuesOf(c1At, c2At), cluster2}, nil)
	return append(ebmlHeader(), unknownEl(idSegment, segment)...)
}

// readAll reads every block of data from its first Cluster, the frames of
// the tracks want wants (nil: all), with reads of chunk bytes, inflating
// them to maxInflated bytes at most (0: the default).
func readAll(data []byte, want func(uint64) bool, chunk int, maxInflated int64) ([]*Block, error) {
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	rd := f.Blocks(bytes.NewReader(data), f.FirstCluster)
	rd.Want = want
	rd.MaxInflated = maxInflated
	if chunk > 0 {
		rd.Chunk = chunk
	}
	var out []*Block
	for i := 0; i < 100000; i++ {
		b, err := rd.Next()
		if err != nil {
			return out, err
		}
		out = append(out, b)
	}
	return out, errors.New("no end to the blocks")
}

// The hand-made file reads as it was written: its tracks, its two cues, and
// its blocks with their frames unlaced and their encodings undone.
func TestHandMadeFile(t *testing.T) {
	data := handMadeFile(t, 0)
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Tracks) != 3 || len(f.Cues) != 2 || f.FirstCluster < 0 {
		t.Fatalf("%d tracks, %d cues, first cluster %d", len(f.Tracks), len(f.Cues), f.FirstCluster)
	}
	blocks, err := readAll(data, nil, 0, 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	var frames []int
	for _, b := range blocks {
		frames = append(frames, len(b.Frames))
	}
	if want := []int{1, 3, 1, 1, 1, 3, 4, 1}; len(frames) != len(want) {
		t.Fatalf("frames per block %v, want %v", frames, want)
	}
	if a := blocks[1].Frames; len(a[0]) != 2+10 || !bytes.HasPrefix(a[0], []byte{0x0B, 0x77}) || len(a[2]) != 2+5 {
		t.Errorf("Xiph-laced, header-stripped frames %d %d %d", len(a[0]), len(a[1]), len(a[2]))
	}
	if s := blocks[2]; string(s.Frames[0]) != "a subtitle line" || s.Duration != 900 || !s.Keyframe {
		t.Errorf("subtitle block %q, duration %d, keyframe %v", s.Frames[0], s.Duration, s.Keyframe)
	}
	if e := blocks[5].Frames; len(e[0]) != 2+12 || len(e[1]) != 2+16 || len(e[2]) != 2+3 {
		t.Errorf("EBML-laced frames %d %d %d", len(e[0]), len(e[1]), len(e[2]))
	}
	if g := blocks[7]; g.Keyframe || g.Timestamp != 1040 {
		t.Errorf("a BlockGroup with a reference: keyframe %v at %d", g.Keyframe, g.Timestamp)
	}
}

// A file cut short anywhere — inside a BlockGroup's Block of a track whose
// frames are not wanted too, as mkvmerge writes subtitles — ends its blocks
// with io.EOF or an error of its format, whatever is wanted and whatever the
// reads' size.
func TestTruncatedFiles(t *testing.T) {
	data := handMadeFile(t, 0)
	wants := map[string]func(uint64) bool{"all": nil, "none": func(uint64) bool { return false }, "video": func(t uint64) bool { return t == 1 }}
	for cut := 0; cut < len(data); cut++ {
		for name, want := range wants {
			for _, chunk := range []int{0, 16} {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("cut at %d, %s wanted, chunk %d: panic %v", cut, name, chunk, r)
						}
					}()
					_, err := readAll(data[:cut], want, chunk, 0)
					if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, ErrFormat) {
						t.Fatalf("cut at %d, %s wanted: %v", cut, name, err)
					}
				}()
			}
		}
	}
}

// The reviewed case itself: a subtitle's BlockGroup cut inside its Block's
// frame, the subtitle track not read.
func TestTruncatedBlockGroup(t *testing.T) {
	simple := el(idSimpleBlock, blockPayload(1, 0, 0x80, bytes.Repeat([]byte{0xAB}, 100)))
	group := el(idBlockGroup, el(idBlock, blockPayload(2, 10, 0, bytes.Repeat([]byte{'x'}, 2000))), uintEl(idBlockDuration, 500))
	tracks := el(idTracks,
		el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo), strEl(idCodecID, "V_MPEG4/ISO/AVC")),
		el(idTrackEntry, uintEl(idTrackNumber, 2), uintEl(idTrackType, TrackSubtitle), strEl(idCodecID, "S_TEXT/UTF8")))
	file := append(ebmlHeader(), el(idSegment, el(idInfo, uintEl(idTSScale, 1_000_000)), tracks,
		el(idCluster, uintEl(idTimestamp, 0), simple, group))...)
	cut := bytes.Index(file, bytes.Repeat([]byte{'x'}, 16)) + 500
	for _, want := range []func(uint64) bool{func(uint64) bool { return false }, func(t uint64) bool { return t == 1 }} {
		blocks, err := readAll(file[:cut], want, 0, 0)
		if !errors.Is(err, io.EOF) || len(blocks) != 1 {
			t.Errorf("%d blocks, then %v; want the video's, then io.EOF", len(blocks), err)
		}
	}
}

// A SeekHead pointing at an Info, Tracks or SeekHead of unknown size (one
// no muxer writes there) is an error of the file's format.
func TestSeekHeadToAnElementOfUnknownSize(t *testing.T) {
	cluster := el(idCluster, uintEl(idTimestamp, 0), el(idSimpleBlock, blockPayload(1, 0, 0x80, []byte{1, 2, 3})))
	info := el(idInfo, uintEl(idTSScale, 1_000_000))
	tracks := el(idTracks, el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo)))
	for _, target := range []uint32{idTracks, idInfo, idSeekHead} {
		sh := func(at uint64) []byte {
			return el(idSeekHead, el(idSeek, el(idSeekID, idBytes(target)), uintEl(idSeekPos, at)))
		}
		head := tracks
		if target == idTracks {
			head = nil // its Tracks only where the SeekHead points
		}
		at := uint64(len(sh(0)) + len(info) + len(head) + len(cluster))
		var unknown []byte
		switch target {
		case idTracks:
			unknown = unknownEl(idTracks, el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo)))
		case idInfo:
			unknown = unknownEl(idInfo, uintEl(idTSScale, 1_000_000))
		case idSeekHead:
			unknown = unknownEl(idSeekHead, el(idSeek, el(idSeekID, idBytes(idCues)), uintEl(idSeekPos, 0)))
		}
		data := append(ebmlHeader(), el(idSegment, sh(at), info, head, cluster, unknown)...)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("0x%x: panic in Open: %v", target, r)
				}
			}()
			if _, err := Open(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrFormat) {
				t.Errorf("0x%x of unknown size: %v", target, err)
			}
		}()
	}
}

// EBML lacing whose size differences add up past what an int holds: an
// error, not a slice out of range.
func TestEBMLLacingOverflow(t *testing.T) {
	const n = 256
	a := int64(72340172838076673) // (2^64-1)/255
	first := int64(1)<<56 - 2
	sizes := make([]int64, n-1)
	sizes[0] = first
	sizes[1] = a + (a - first)
	for i := 2; i < n-2; i++ {
		sizes[i] = a
	}
	sizes[n-2] = a + 11
	bias := int64(1)<<55 - 1
	hdr := []byte{byte(n - 1)}
	put := func(v uint64) {
		hdr = append(hdr, 0x01, byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	put(uint64(first))
	for i := 1; i < n-1; i++ {
		put(uint64(sizes[i] - sizes[i-1] + bias))
	}
	if _, err := unlace(append(hdr, make([]byte, 64)...), 3); !errors.Is(err, ErrFormat) {
		t.Errorf("unlace: %v", err)
	}
	// Xiph sizes larger than the block.
	if _, err := unlace(append([]byte{1}, append(bytes.Repeat([]byte{255}, 40), 3, 1, 2, 3)...), 1); !errors.Is(err, ErrFormat) {
		t.Errorf("Xiph: %v", err)
	}
}

// sparseReader is a file of size bytes: data, then zeros; it fails the test
// if one read asks for more than limit bytes.
type sparseReader struct {
	t     *testing.T
	data  []byte
	size  int64
	limit int
}

func (s *sparseReader) ReadAt(b []byte, off int64) (int, error) {
	if len(b) > s.limit {
		s.t.Fatalf("a read of %d bytes at %d", len(b), off)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	n := 0
	if off < int64(len(s.data)) {
		n = copy(b, s.data[off:])
	}
	for i := n; i < len(b) && off+int64(i) < s.size; i++ {
		b[i] = 0
		n++
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// A block said to be larger than any frame (1 GiB, in a file that large) is
// an error before anything is read for it.
func TestOversizedBlock(t *testing.T) {
	tracks := el(idTracks, el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo)))
	huge := append(idBytes(idSimpleBlock), 0x01, 0, 0, 0, 0x40, 0, 0, 0) // 1 GiB
	huge = append(huge, blockPayload(1, 0, 0x80, nil)...)
	data := append(ebmlHeader(), unknownEl(idSegment, el(idInfo, uintEl(idTSScale, 1_000_000)), tracks,
		unknownEl(idCluster, uintEl(idTimestamp, 0)), huge)...)
	r := &sparseReader{t: t, data: data, size: 2 << 30, limit: 32 << 20}
	f, err := Open(r, r.size)
	if err != nil {
		t.Fatal(err)
	}
	rd := f.Blocks(r, f.FirstCluster)
	if _, err := rd.Next(); !errors.Is(err, ErrFormat) {
		t.Errorf("a block of 1 GiB: %v", err)
	}
}

// errStorage is a read the storage failed.
var errStorage = errors.New("input/output error")

// failingReader fails every read from at on.
type failingReader struct {
	data []byte
	at   int64
}

func (f *failingReader) ReadAt(b []byte, off int64) (int, error) {
	if off+int64(len(b)) > f.at {
		return 0, errStorage
	}
	return copy(b, f.data[off:]), nil
}

// A read of the Cues that fails is the storage's, not a file without an
// index: Open says so, and does not come back without Cues.
func TestCuesThatCannotBeRead(t *testing.T) {
	data := handMadeFile(t, headRead) // the Cues past Open's first read
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(f.Cues) != 2 || f.CuesPos <= int64(headRead) {
		t.Fatalf("%v: %d cues at %d", err, len(f.Cues), f.CuesPos)
	}
	r := &failingReader{data: data, at: f.CuesPos}
	if _, err := Open(r, int64(len(data))); !errors.Is(err, errStorage) {
		t.Errorf("Open with the Cues unreadable: %v, want the read's error", err)
	}
}

// zeros is a zlib stream of n zero bytes.
func zeros(t testing.TB, n int) []byte {
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

// zlibBombFile is a file of a video track (1) and a zlib-compressed
// subtitle track (3) whose blocks (one per given count) hold frames laced
// frames each, each a zlib stream of size zeros: a block that inflates to
// frames × size bytes.
func zlibBombFile(t testing.TB, frames, size, blocks int) []byte {
	tracks := el(idTracks,
		el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackType, TrackVideo), strEl(idCodecID, "V_MPEG4/ISO/AVC")),
		el(idTrackEntry, uintEl(idTrackNumber, 3), uintEl(idTrackType, TrackSubtitle), strEl(idCodecID, "S_TEXT/UTF8"),
			el(idContentEncs, el(idContentEnc, uintEl(idContentEncScope, 1), el(idContentComp, uintEl(idContentCompAlgo, compZlib))))))
	frame := zeros(t, size)
	parts := [][]byte{uintEl(idTimestamp, 0), el(idSimpleBlock, blockPayload(1, 0, 0x80, []byte{1, 2, 3}))}
	for i := 0; i < blocks; i++ {
		laced := append([]byte{byte(frames - 1)}, bytes.Repeat(frame, frames)...) // fixed-size lacing
		flags := byte(0)
		if frames > 1 {
			flags = 2 << 1
		} else {
			laced = frame
		}
		parts = append(parts, el(idSimpleBlock, blockPayload(3, int16(i), 0x80|flags, laced)))
	}
	return append(ebmlHeader(), el(idSegment, el(idInfo, uintEl(idTSScale, 1_000_000)), tracks, el(idCluster, parts...))...)
}

// allocated is how many bytes fn allocates.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A frame that inflates to 128 MiB from 127 KB, and a block of 256 frames
// that inflate to 1 MiB each: errors of the file's format, read no further
// than a block's bound — not a frame cut short, not 256 MiB in memory.
func TestZlibBombs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		frames, size int
	}{{"one frame of 128 MiB", 1, 128 << 20}, {"256 frames of 1 MiB", 256, 1 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			data := zlibBombFile(t, tc.frames, tc.size, 1)
			var blocks []*Block
			var err error
			n := allocated(func() { blocks, err = readAll(data, nil, 0, 0) })
			if !errors.Is(err, ErrFormat) || len(blocks) != 1 {
				t.Errorf("%d blocks, then %v; want the video's, then the file's error", len(blocks), err)
			}
			// What a block bounded at MaxBlockInflated allocates (the growing
			// buffers, the race detector's share) is a few times the bound:
			// one inflating its 256 MiB allocates twice that and more.
			if n > 6*MaxBlockInflated {
				t.Errorf("%d MiB allocated reading a block of %d KB", n>>20, len(data)>>10)
			}
			t.Logf("%d KB, %d MiB allocated: %v", len(data)>>10, n>>20, err)
		})
	}
	// A block the subtitle track's frames are not wanted of is not inflated.
	data := zlibBombFile(t, 256, 1<<20, 1)
	var err error
	if n := allocated(func() { _, err = readAll(data, func(t uint64) bool { return t == 1 }, 0, 0) }); n > 8<<20 || !errors.Is(err, io.EOF) {
		t.Errorf("not wanted: %d MiB allocated, %v", n>>20, err)
	}
}

// Blocks below a block's bound inflate to a Reader's at most, all together.
func TestAReadersInflationIsBounded(t *testing.T) {
	data := zlibBombFile(t, 40, 256<<10, 4) // 10 MiB a block
	blocks, err := readAll(data, nil, 0, 25<<20)
	if !errors.Is(err, ErrFormat) || len(blocks) != 3 {
		t.Errorf("%d blocks, then %v; want the video's and two of 10 MiB, then the file's error", len(blocks), err)
	}
	if blocks, err := readAll(data, nil, 0, 0); !errors.Is(err, io.EOF) || len(blocks) != 5 {
		t.Errorf("with the default bound: %d blocks, %v", len(blocks), err)
	}
}

// The fuzz targets: whatever the bytes, Open and the block reader end in a
// File and blocks, or an error.

func FuzzReader(f *testing.F) {
	data := handMadeFile(f, 0)
	f.Add(data)
	f.Add(data[:len(data)/2])
	f.Add(data[:len(data)-7])
	// A block of zlib frames that inflate to 4 MiB, past the bound the fuzz
	// reads with.
	f.Add(zlibBombFile(f, 64, 64<<10, 1))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, want := range []func(uint64) bool{nil, func(uint64) bool { return false }, func(t uint64) bool { return t == 1 }} {
			for _, chunk := range []int{0, 7} {
				_, _ = readAll(data, want, chunk, 1<<20)
			}
		}
	})
}

func FuzzUnlace(f *testing.F) {
	f.Add([]byte{2, 10, 20, 1, 2, 3}, byte(1))
	f.Add([]byte{2, 0x8C, 0xC3, 1, 2, 3}, byte(3))
	f.Add([]byte{3, 1, 2, 3, 4, 5, 6, 7, 8}, byte(2))
	f.Fuzz(func(t *testing.T, data []byte, lacing byte) {
		frames, err := unlace(data, lacing&3)
		if err != nil {
			return
		}
		n := 0
		for _, fr := range frames {
			n += len(fr)
		}
		if n > len(data) {
			t.Fatalf("frames of %d bytes from %d", n, len(data))
		}
	})
}
