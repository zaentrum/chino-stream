package play

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The stream copy of the live pipeline: an original whose video, audio and
// container the client takes, remuxed in Go with mp4ff, so ffmpeg never runs
// as a muxer for it (phase-1.md, D).
//
//   - The plan is the one the ffmpeg path makes (buildPassPlan): the video
//     keyframes, grouped into segments of at least passTargetSec, the duration
//     the header probe's. A progressive original's keyframes come from its
//     stss and stts, as keyframesViaMp4ff reads them, so its playlist is the
//     same; a fragmented original's from its trun flags.
//   - init.mp4 is built from the original's moov: the video track's and the
//     first audio track's sample entries as they are, an mvex, and the
//     original's edit lists in its own movie timescale.
//   - Segment n holds the video samples of plan segment n, keyframe to
//     keyframe, and the audio samples presented from that keyframe to the next
//     one's (each frame where its middle falls), read from the original by
//     byte range and written with their own timestamps — the decode times of
//     the original's tables, as -copyts kept them — in one fragment; the
//     init's edit lists take a player to the presentation.
//
// An original mp4ff cannot read (MPEG-TS, FLV) is not stream-copied on the
// live pipeline: it takes the transcode ladder (remux), as Matroska always
// has, so neither ffmpeg nor a keyframe probe is needed for it.

// isoBMFF says whether ffprobe's format name is an ISO base media file
// (MP4, MOV, M4V, 3GP): what mp4ff reads.
func isoBMFF(format string) bool {
	for _, f := range strings.Split(strings.ToLower(format), ",") {
		switch strings.TrimSpace(f) {
		case "mov", "mp4", "m4a", "3gp", "3g2", "mj2":
			return true
		}
	}
	return false
}

// copySample is one sample of an original as the remux reads it: its decode
// time and duration (the track's ticks), composition offset, size, position
// in the file and whether it is a sync sample.
type copySample struct {
	dts    int64
	offset int64
	dur    uint32
	size   uint32
	cto    int32
	sync   bool
}

// copyTrack is one track of the remux.
type copyTrack struct {
	id        uint32 // its ID in the init and the fragments: video 1, audio 2
	timescale uint32
	// shift takes a composition time to the presentation, as the original's
	// edit list does (presentation = dts + cto + shift), in the track's ticks.
	shift   int64
	samples []copySample
}

// presentation is sample i's presentation time in seconds.
func (t *copyTrack) presentation(i int) float64 {
	s := t.samples[i]
	return float64(s.dts+int64(s.cto)+t.shift) / float64(t.timescale)
}

// copyIndex is an original indexed for the remux: its video track and first
// audio track, its plan and its init.
type copyIndex struct {
	path  string
	mtime time.Time
	size  int64
	video *copyTrack
	audio *copyTrack // nil without audio
	plan  *passPlan
	// segStart is the video sample each plan segment starts with.
	segStart []int
	init     []byte
	err      error // why the original cannot be indexed (cached as well)
}

// copyIndexCacheSize is how many originals stay indexed: their sample tables
// take about 30 bytes a sample (a two-hour film, some 15 MB).
const copyIndexCacheSize = 4

// copyIndexes caches the indexes by path, size and mtime, the least recently
// used dropped first.
var copyIndexes = struct {
	mu   sync.Mutex
	m    map[string]*copyIndex
	used map[string]time.Time
	lock sync.Map // map[string]*sync.Mutex: one index build per path at a time
}{m: map[string]*copyIndex{}, used: map[string]time.Time{}}

// loadCopyIndex is the index of the original at path, whose header probe says
// it lasts durationMs: cached, else built (and an original that cannot be
// indexed is remembered so, until it changes).
func loadCopyIndex(path string, durationMs int64) (*copyIndex, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if ix := cachedCopyIndex(path, st); ix != nil {
		return ix, ix.err
	}
	mu, _ := copyIndexes.lock.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if ix := cachedCopyIndex(path, st); ix != nil {
		return ix, ix.err
	}
	ix, err := buildCopyIndex(path, durationMs)
	if err != nil {
		// Remembered as the file is: a changed one is indexed again.
		ix = &copyIndex{path: path, mtime: st.ModTime(), size: st.Size(), err: fmt.Errorf("stream copy of %s: %w", path, err)}
	}
	copyIndexes.mu.Lock()
	defer copyIndexes.mu.Unlock()
	for len(copyIndexes.m) >= copyIndexCacheSize {
		oldest, at := "", time.Time{}
		for p, t := range copyIndexes.used {
			if oldest == "" || t.Before(at) {
				oldest, at = p, t
			}
		}
		delete(copyIndexes.m, oldest)
		delete(copyIndexes.used, oldest)
	}
	copyIndexes.m[path] = ix
	copyIndexes.used[path] = time.Now()
	return ix, ix.err
}

func cachedCopyIndex(path string, st os.FileInfo) *copyIndex {
	copyIndexes.mu.Lock()
	defer copyIndexes.mu.Unlock()
	ix := copyIndexes.m[path]
	if ix == nil || ix.size != st.Size() || !ix.mtime.Equal(st.ModTime()) {
		return nil
	}
	copyIndexes.used[path] = time.Now()
	return ix
}

// copyRemuxable says whether the original of src is stream-copied in Go on
// the live pipeline: an ISO media file mp4ff indexes, with a plan.
func copyRemuxable(src *source) bool {
	if src.fromPackage() || src.probe == nil || !isoBMFF(src.probe.Container) {
		return false
	}
	ix, err := loadCopyIndex(src.file, src.probe.DurationMs)
	return err == nil && len(ix.plan.Segments) > 0
}

// buildCopyIndex indexes the original at path.
func buildCopyIndex(path string, durationMs int64) (*copyIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	mf, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("mp4 decode: %w", err)
	}
	if mf.Moov == nil || mf.Moov.Mvhd == nil {
		return nil, errors.New("no moov")
	}
	var vt, at *mp4.TrakBox
	for _, t := range mf.Moov.Traks {
		if t.Mdia == nil || t.Mdia.Hdlr == nil || t.Mdia.Mdhd == nil || t.Mdia.Minf == nil || t.Mdia.Minf.Stbl == nil || t.Tkhd == nil {
			continue
		}
		switch t.Mdia.Hdlr.HandlerType {
		case "vide":
			if vt == nil {
				vt = t
			}
		case "soun":
			if at == nil {
				at = t
			}
		}
	}
	if vt == nil {
		return nil, errors.New("no video track")
	}
	ix := &copyIndex{path: path, mtime: st.ModTime(), size: st.Size()}
	if ix.video, err = copyTrackOf(mf, vt, 1); err != nil {
		return nil, fmt.Errorf("video: %w", err)
	}
	if at != nil {
		if ix.audio, err = copyTrackOf(mf, at, 2); err != nil {
			return nil, fmt.Errorf("audio: %w", err)
		}
	}
	var kfIdx []int
	var kf []float64
	ts := float64(ix.video.timescale)
	for i, s := range ix.video.samples {
		if s.sync {
			kfIdx = append(kfIdx, i)
			kf = append(kf, float64(s.dts)/ts)
		}
	}
	if len(kf) == 0 {
		return nil, errors.New("the video track has no sync sample")
	}
	segs, starts := groupKeyframes(kf, durationMs)
	if len(segs) == 0 {
		return nil, errors.New("no segment in the plan")
	}
	ix.plan = &passPlan{DurationMs: durationMs, Segments: segs}
	for _, s := range starts {
		ix.segStart = append(ix.segStart, kfIdx[s])
	}
	if ix.init, err = copyInit(mf.Moov, vt, at); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	return ix, nil
}

// copyTrackOf reads the samples of trak — its sample tables and, in a
// fragmented original, its fragments — as the remux's track id.
func copyTrackOf(mf *mp4.File, trak *mp4.TrakBox, id uint32) (*copyTrack, error) {
	if err := trak.CheckDataIsSelfContained(); err != nil {
		return nil, err
	}
	t := &copyTrack{id: id, timescale: trak.Mdia.Mdhd.Timescale}
	if t.timescale == 0 {
		return nil, errors.New("zero timescale")
	}
	t.shift = editShift(trak, mf.Moov.Mvhd.Timescale)
	samples, err := tableSamples(trak.Mdia.Minf.Stbl)
	if err != nil {
		return nil, err
	}
	if mf.IsFragmented() {
		var next int64
		if n := len(samples); n > 0 {
			next = samples[n-1].dts + int64(samples[n-1].dur)
		}
		samples, err = fragmentSamples(mf, trak.Tkhd.TrackID, trexOf(mf.Moov, trak.Tkhd.TrackID), next, samples)
		if err != nil {
			return nil, err
		}
	}
	if len(samples) == 0 {
		return nil, errors.New("no samples")
	}
	t.samples = samples
	return t, nil
}

// editShift is what trak's edit list amounts to: presentation = composition
// time + shift, in the track's ticks (an empty edit delays the presentation,
// the first edit's media time is where it starts), with movieTimescale the
// mvhd's.
func editShift(trak *mp4.TrakBox, movieTimescale uint32) int64 {
	if trak.Edts == nil {
		return 0
	}
	var shift int64
	ts := int64(trak.Mdia.Mdhd.Timescale)
	for _, elst := range trak.Edts.Elst {
		for _, e := range elst.Entries {
			if e.MediaTime < 0 {
				if movieTimescale > 0 {
					shift += int64(e.SegmentDuration) * ts / int64(movieTimescale)
				}
				continue
			}
			return shift - e.MediaTime
		}
	}
	return shift
}

// tableSamples are the samples of a progressive track's sample tables (none
// for a fragmented one's empty tables).
func tableSamples(stbl *mp4.StblBox) ([]copySample, error) {
	if stbl.Stsz == nil || stbl.Stts == nil || stbl.Stsc == nil {
		return nil, nil
	}
	n := stbl.Stsz.GetNrSamples()
	if n == 0 {
		return nil, nil
	}
	durs, err := stbl.Stts.SampleDurations(n)
	if err != nil {
		return nil, fmt.Errorf("stts: %w", err)
	}
	var ctos []int32
	if stbl.Ctts != nil {
		if ctos, err = stbl.Ctts.CompositionTimeOffsets(n); err != nil {
			return nil, fmt.Errorf("ctts: %w", err)
		}
	}
	var syncs []bool
	if stbl.Stss != nil {
		if syncs, err = stbl.Stss.SampleIsSync(n); err != nil {
			return nil, fmt.Errorf("stss: %w", err)
		}
	}
	var chunks []uint64
	switch {
	case stbl.Co64 != nil:
		chunks = stbl.Co64.ChunkOffset
	case stbl.Stco != nil:
		for _, o := range stbl.Stco.ChunkOffset {
			chunks = append(chunks, uint64(o))
		}
	default:
		return nil, errors.New("no chunk offsets")
	}
	out := make([]copySample, n)
	var dts int64
	for i := range out {
		out[i].dts = dts
		out[i].dur = durs[i]
		dts += int64(durs[i])
		out[i].size = stbl.Stsz.GetSampleSize(i + 1)
		if ctos != nil {
			out[i].cto = ctos[i]
		}
		out[i].sync = syncs == nil || syncs[i]
	}
	// Chunk by chunk: each stsc entry gives its first chunk's number and how
	// many samples its chunks hold, up to the next entry's first chunk.
	entries := stbl.Stsc.Entries
	i := 0
	for e, ent := range entries {
		last := uint32(len(chunks))
		if e+1 < len(entries) {
			last = entries[e+1].FirstChunk - 1
		}
		for c := ent.FirstChunk; c <= last && i < len(out); c++ {
			if c == 0 || int(c) > len(chunks) {
				return nil, fmt.Errorf("stsc names chunk %d of %d", c, len(chunks))
			}
			off := int64(chunks[c-1])
			for k := uint32(0); k < ent.SamplesPerChunk && i < len(out); k++ {
				out[i].offset = off
				off += int64(out[i].size)
				i++
			}
		}
	}
	if i != len(out) {
		return nil, fmt.Errorf("the chunks hold %d of %d samples", i, len(out))
	}
	return out, nil
}

// trexOf is the trex of trackID.
func trexOf(moov *mp4.MoovBox, trackID uint32) *mp4.TrexBox {
	if moov.Mvex == nil {
		return nil
	}
	for _, t := range moov.Mvex.Trexs {
		if t.TrackID == trackID {
			return t
		}
	}
	return moov.Mvex.Trex
}

// fragmentSamples appends to out the samples of trackID in the fragments of a
// fragmented original, from the decode time next on where a fragment has no
// tfdt; a sample is a sync sample when its flags do not say non-sync.
func fragmentSamples(mf *mp4.File, trackID uint32, trex *mp4.TrexBox, next int64, out []copySample) ([]copySample, error) {
	for _, seg := range mf.Segments {
		for _, frag := range seg.Fragments {
			if frag.Moof == nil {
				continue
			}
			for _, traf := range frag.Moof.Trafs {
				if traf.Tfhd == nil || traf.Tfhd.TrackID != trackID {
					continue
				}
				dts := next
				if traf.Tfdt != nil {
					dts = int64(traf.Tfdt.BaseMediaDecodeTime())
				}
				base := frag.Moof.StartPos
				if traf.Tfhd.HasBaseDataOffset() {
					base = traf.Tfhd.BaseDataOffset
				}
				pos := int64(base)
				for _, trun := range traf.Truns {
					trun.AddSampleDefaultValues(traf.Tfhd, trex)
					if trun.HasDataOffset() {
						pos = int64(base) + int64(trun.DataOffset)
					}
					for _, s := range trun.Samples {
						out = append(out, copySample{dts: dts, offset: pos, dur: s.Dur, size: s.Size,
							cto: s.CompositionTimeOffset, sync: s.Flags&mp4.NonSyncSampleFlags == 0})
						dts += int64(s.Dur)
						pos += int64(s.Size)
					}
				}
				next = dts
			}
		}
	}
	return out, nil
}

// copyInit is the init of the remux: the original's movie header, its video
// track (1) and first audio track (2) with their headers, edit lists and
// sample entries as they are, empty sample tables, and an mvex.
func copyInit(moov *mp4.MoovBox, video, audio *mp4.TrakBox) ([]byte, error) {
	init := mp4.NewMP4Init()
	init.AddChild(mp4.NewFtyp("iso6", 0, []string{"iso6", "isom", "mp41"}))
	m := mp4.NewMoovBox()
	init.AddChild(m)
	mvhd := *moov.Mvhd
	mvhd.Duration = 0
	mvhd.NextTrackID = 3
	m.AddChild(&mvhd)
	mvex := mp4.NewMvexBox()
	for i, t := range []*mp4.TrakBox{video, audio} {
		if t == nil {
			continue
		}
		id := uint32(i + 1)
		m.AddChild(copyTrak(t, id))
		mvex.AddChild(mp4.CreateTrex(id))
	}
	m.AddChild(mvex)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copyTrak is trak for a fragmented file, as track id: its header, edit list,
// media header, handler and sample entries, and empty sample tables.
func copyTrak(t *mp4.TrakBox, id uint32) *mp4.TrakBox {
	trak := &mp4.TrakBox{}
	tkhd := *t.Tkhd
	tkhd.TrackID, tkhd.Duration = id, 0
	trak.AddChild(&tkhd)
	if t.Edts != nil {
		trak.AddChild(t.Edts)
	}
	mdia := &mp4.MdiaBox{}
	mdhd := *t.Mdia.Mdhd
	mdhd.Duration = 0
	mdia.AddChild(&mdhd)
	mdia.AddChild(t.Mdia.Hdlr)
	if t.Mdia.Elng != nil {
		mdia.AddChild(t.Mdia.Elng)
	}
	minf := mp4.NewMinfBox()
	switch {
	case t.Mdia.Minf.Vmhd != nil:
		minf.AddChild(t.Mdia.Minf.Vmhd)
	case t.Mdia.Minf.Smhd != nil:
		minf.AddChild(t.Mdia.Minf.Smhd)
	}
	dinf := &mp4.DinfBox{}
	dinf.AddChild(mp4.CreateDref())
	minf.AddChild(dinf)
	stbl := mp4.NewStblBox()
	stbl.AddChild(t.Mdia.Minf.Stbl.Stsd)
	stbl.AddChild(&mp4.SttsBox{})
	stbl.AddChild(&mp4.StscBox{})
	stbl.AddChild(&mp4.StszBox{})
	stbl.AddChild(&mp4.StcoBox{})
	minf.AddChild(stbl)
	mdia.AddChild(minf)
	trak.AddChild(mdia)
	return trak
}

// audioFrom is the first audio sample of plan segment n: the first one whose
// middle is presented at or after the presentation of the segment's first
// video sample (none before segment 0, all after the last).
func (ix *copyIndex) audioFrom(n int) int {
	a := ix.audio
	switch {
	case a == nil:
		return 0
	case n <= 0:
		return 0
	case n >= len(ix.segStart):
		return len(a.samples)
	}
	b := ix.video.presentation(ix.segStart[n])
	half := func(i int) float64 { return float64(a.samples[i].dur) / 2 / float64(a.timescale) }
	return sort.Search(len(a.samples), func(i int) bool { return a.presentation(i)+half(i) >= b })
}

// segment is plan segment n of the remux: one fragment with the video samples
// of the segment and the audio presented with them.
func (ix *copyIndex) segment(n int) ([]byte, error) {
	if n < 0 || n >= len(ix.segStart) {
		return nil, errPastEnd
	}
	v := ix.video
	v0, v1 := ix.segStart[n], len(v.samples)
	if n+1 < len(ix.segStart) {
		v1 = ix.segStart[n+1]
	}
	f, err := os.Open(ix.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != ix.size || !st.ModTime().Equal(ix.mtime) {
		return nil, errors.New("the original changed since it was indexed")
	}
	type part struct {
		t       *copyTrack
		samples []copySample
	}
	parts := []part{{v, v.samples[v0:v1]}}
	if a := ix.audio; a != nil {
		if a0, a1 := ix.audioFrom(n), ix.audioFrom(n+1); a1 > a0 {
			parts = append(parts, part{a, a.samples[a0:a1]})
		}
	}
	var ids []uint32
	for _, p := range parts {
		ids = append(ids, p.t.id)
	}
	frag, err := mp4.CreateMultiTrackFragment(uint32(n+1), ids)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		data, err := readSampleData(f, p.samples)
		if err != nil {
			return nil, err
		}
		for i, s := range p.samples {
			flags := mp4.SyncSampleFlags
			if !s.sync {
				flags = mp4.SetNonSyncSampleFlags(0)
			}
			full := mp4.FullSample{Sample: mp4.Sample{Flags: flags, Dur: s.dur, Size: s.size, CompositionTimeOffset: s.cto},
				DecodeTime: uint64(s.dts), Data: data[i]}
			if err := frag.AddFullSampleToTrack(full, p.t.id); err != nil {
				return nil, err
			}
		}
	}
	var buf bytes.Buffer
	if err := remuxStyp().Encode(&buf); err != nil {
		return nil, err
	}
	if err := frag.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// remuxStyp is the styp of a remuxed segment.
func remuxStyp() *mp4.StypBox { return mp4.NewStyp("msdh", 0, []string{"msdh", "msix"}) }

// readSampleData reads the samples' bytes from f by byte range: one read per
// run of samples that follow each other in the file (at most 8 MiB).
func readSampleData(f io.ReaderAt, samples []copySample) ([][]byte, error) {
	out := make([][]byte, len(samples))
	for i := 0; i < len(samples); {
		j, end := i+1, samples[i].offset+int64(samples[i].size)
		for j < len(samples) && samples[j].offset == end && end-samples[i].offset < 8<<20 {
			end += int64(samples[j].size)
			j++
		}
		buf := make([]byte, end-samples[i].offset)
		if _, err := f.ReadAt(buf, samples[i].offset); err != nil {
			return nil, fmt.Errorf("read %d bytes at %d: %w", len(buf), samples[i].offset, err)
		}
		pos := 0
		for k := i; k < j; k++ {
			out[k] = buf[pos : pos+int(samples[k].size)]
			pos += int(samples[k].size)
		}
		i = j
	}
	return out, nil
}
