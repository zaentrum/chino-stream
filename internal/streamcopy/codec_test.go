package streamcopy

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// bitWriter writes bits MSB first.
type bitWriter struct {
	b   []byte
	pos int
}

func (w *bitWriter) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos/8 >= len(w.b) {
			w.b = append(w.b, 0)
		}
		if v>>i&1 != 0 {
			w.b[w.pos/8] |= 0x80 >> (w.pos % 8)
		}
		w.pos++
	}
}

// eac3Frame is an E-AC-3 syncframe header of size bytes (padded with zeros):
// a stream type, 48 kHz, six blocks, acmod and lfeon; a dependent one with
// its chanmap.
func eac3Frame(strmtyp, acmod, lfeon int, chanmap int, size int) []byte {
	w := &bitWriter{}
	w.put(0x0B77, 16)
	w.put(strmtyp, 2)
	w.put(0, 3) // substreamid
	w.put(size/2-1, 11)
	w.put(0, 2) // fscod 48 kHz
	w.put(3, 2) // numblkscod: six blocks
	w.put(acmod, 3)
	w.put(lfeon, 1)
	w.put(16, 5) // bsid
	w.put(0, 5)  // dialnorm
	w.put(0, 1)  // compre
	if strmtyp == 1 {
		if chanmap >= 0 {
			w.put(1, 1)
			w.put(chanmap, 16)
		} else {
			w.put(0, 1)
		}
	}
	for len(w.b) < size {
		w.b = append(w.b, 0)
	}
	return w.b
}

// An E-AC-3 access unit of an independent 5.1 substream and a dependent one
// carrying the back surrounds (7.1): one independent substream with one
// dependent, its channel locations Lrs/Rrs; the channel count the track's.
func TestEAC3Config(t *testing.T) {
	main := eac3Frame(0, 7, 1, -1, 1536)
	dep := eac3Frame(1, 2, 0, 1<<(15-6), 512) // chanmap: the Lrs/Rrs pair
	c, err := eac3Config(append(main, dep...), 8)
	if err != nil {
		t.Fatal(err)
	}
	if c.codecs != "ec-3" || c.sampleRate != 48000 || c.frameSamples != 1536 || c.channels != 8 {
		t.Errorf("config %+v", c)
	}
	init, err := audioInit(c, 0, "eng")
	if err != nil || len(init) == 0 {
		t.Fatalf("init: %v", err)
	}
	if got := locBits(1 << (15 - 6)); got != 1<<1 {
		t.Errorf("chan_loc of Lrs/Rrs: %b", got)
	}
	if got := locBits(1 << (15 - 14)); got != 1<<8 {
		t.Errorf("chan_loc of LFE2: %b", got)
	}
	// A 5.1 one alone: its own channels.
	if c, err := eac3Config(main, 0); err != nil || c.channels != 6 {
		t.Errorf("5.1: %+v %v", c, err)
	}
	if _, err := eac3Config([]byte{0, 1, 2, 3, 4, 5, 6}, 0); err == nil {
		t.Error("a frame without a syncword is read")
	}
	if n := eac3Samples(main, 0); n != 1536 {
		t.Errorf("samples %d", n)
	}
}

// A frame rate's timescale: its frames and whole milliseconds both exact.
func TestVideoGrid(t *testing.T) {
	for _, tc := range []struct {
		ns    uint64
		ts    uint32
		frame int64
	}{
		{41708333, 24000, 1001}, {40000000, 24000, 960}, {33366666, 30000, 1001}, {16683333, 60000, 1001},
		{41666666, 24000, 1000}, {20000000, 24000, 480}, {0, 90000, 0}, {37000000, 90000, 0},
	} {
		ts, frame := videoGrid(tc.ns)
		if ts != tc.ts || frame != tc.frame || ts%1000 != 0 {
			t.Errorf("videoGrid(%d) = %d, %d; want %d, %d", tc.ns, ts, frame, tc.ts, tc.frame)
		}
	}
	// Millisecond timestamps of 23.976 fps frames go back on their grid.
	for k := int64(0); k < 5000; k++ {
		ms := (k*1001*1000 + 12000) / 24000 // rounded to the millisecond
		if got := snap(nsToTicks(ms*1e6, 24000), 0, 1001, 24); got != k*1001 {
			t.Fatalf("frame %d at %d ms snaps to %d", k, ms, got)
		}
	}
	if got := snap(500, 0, 1001, 24); got != 500 {
		t.Errorf("a time off the grid moved: %d", got)
	}
	if got := nsToTicks(-21_000_000, 48000); got != -1008 {
		t.Errorf("nsToTicks(-21 ms) = %d", got)
	}
}

// A video every decoder of its kind does not play is not copied: 10-bit
// H.264 (High 10).
func TestUnsupportedVideoIsNotCopied(t *testing.T) {
	file := makeFile(t, "film.mkv", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-profile:v", "high10")
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	_, err = IndexMatroska(f, st.Size(), Options{DurationNs: 2e9})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "H.264 profile 110") {
		t.Errorf("10-bit H.264: %v", err)
	}
}

// Audio that ends before the video: the segments after its end carry an
// audio fragment without samples, which a player reads past, and the track's
// frames are all there.
func TestAudioShorterThanTheVideo(t *testing.T) {
	file := makeFile(t, "film.mkv", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=20",
		"-f", "lavfi", "-i", "sine=sample_rate=48000:duration=4",
		"-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "ac3")
	ix := index(t, file)
	_, aud, segs := produceAll(t, ix, []int{0})
	empty := 0
	for _, s := range segs {
		if len(s.Audio[0]) < 200 {
			empty++
		}
	}
	if empty == 0 {
		t.Fatalf("no segment without audio among %d", len(segs))
	}
	af := write(t, "audio.mp4", aud[0])
	if want, got := hashes(t, file, "a:0", true), hashes(t, af, "a:0", true); len(got) != len(want) {
		t.Errorf("audio: %d packets, the original %d", len(got), len(want))
	}
}
