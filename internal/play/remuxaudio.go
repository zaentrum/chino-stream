package play

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/zaentrum/live-worker/cmaf"
)

// A package's audio on the live pipeline (phase-1.md, B for a package). A
// package's stereo rendition is AAC-LC stereo already and every AAC frame is
// a sync sample, so audio/{n}/{k}.m4s is the rendition's own frames presented
// in [6k, 6k+6) on the grid — the frame nearest each boundary starting a
// segment (its middle at or after it), as a video segment's first frame is the
// one nearest its start — written as a fragment with their own timestamps;
// and audio/{n}/init.mp4 is the rendition's own init, its edit list (the
// encoder's priming) included. The URIs and the playlist stay as they were;
// the bytes are the package's audio, not a second encode of it.

// errNoAudioFrames is a segment the rendition holds no frame of (its audio
// ended before the segment does).
var errNoAudioFrames = errors.New("the rendition holds no audio frame of the segment")

// errPastEnd is a segment past the grid's end.
var errPastEnd = errors.New("segment past end")

// renditionTrack is what a rendition's init says of its track.
type renditionTrack struct {
	trackID   uint32
	timescale uint32
	// shift takes the composition time to the presentation, as the init's
	// edit list does (presentation = cts + shift).
	shift int64
	trex  *mp4.TrexBox
}

// readRenditionTrack reads the first track of a rendition's init.
func readRenditionTrack(init []byte) (renditionTrack, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil {
		return renditionTrack{}, err
	}
	if f.Moov == nil || len(f.Moov.Traks) == 0 || f.Moov.Mvhd == nil {
		return renditionTrack{}, errors.New("the init has no track")
	}
	trak := f.Moov.Traks[0]
	if trak.Tkhd == nil || trak.Mdia == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Mdhd.Timescale == 0 {
		return renditionTrack{}, errors.New("the init's track has no header or timescale")
	}
	t := renditionTrack{trackID: trak.Tkhd.TrackID, timescale: trak.Mdia.Mdhd.Timescale,
		shift: editShift(trak, f.Moov.Mvhd.Timescale), trex: trexOf(f.Moov, trak.Tkhd.TrackID)}
	if t.trex == nil || t.trex.TrackID != t.trackID {
		t.trex = &mp4.TrexBox{TrackID: t.trackID}
	}
	return t, nil
}

// packageAudioDir is the folder of the stereo rendition n of the package src.
func packageAudioDir(src *source, n int) string { return src.pkg.path(src.mf.Renditions.Audio[n].Dir) }

// readPackaged is a package file's bytes, through packagedCache.
func readPackaged(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return readPackagedBytes(path, st.ModTime())
}

// packageAudioInit is the init of the stereo rendition n of the package src:
// the rendition's own.
func packageAudioInit(src *source, n int) ([]byte, error) {
	dir := packageAudioDir(src, n)
	pl, err := readRendition(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		return nil, err
	}
	init, err := readPackaged(filepath.Join(dir, pl.init))
	if err != nil {
		return nil, err
	}
	if _, err := readRenditionTrack(init); err != nil {
		return nil, fmt.Errorf("rendition %s: %w", dir, err)
	}
	return init, nil
}

// audioFrame is one AAC frame of a rendition, on its own timeline.
type audioFrame struct {
	dts, pts int64
	dur      int64
	data     []byte
}

// packageAudioSegment is segment k of the grid of the stereo rendition n of
// the package src: the rendition's frames whose middle is presented in
// [start, end) (from the first for segment 0, to the last for the last one),
// each frame lasting until the next one's decode time, so consecutive
// segments tile the rendition's timeline exactly.
func packageAudioSegment(src *source, n, k int) ([]byte, error) {
	start, end, last, ok := liveSpan(src.probe.DurationMs, k)
	if !ok {
		return nil, errPastEnd
	}
	dir := packageAudioDir(src, n)
	pl, err := readRendition(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		return nil, err
	}
	if len(pl.segments) == 0 {
		return nil, fmt.Errorf("rendition %s lists no segments", dir)
	}
	init, err := readPackaged(filepath.Join(dir, pl.init))
	if err != nil {
		return nil, err
	}
	tr, err := readRenditionTrack(init)
	if err != nil {
		return nil, fmt.Errorf("rendition %s: %w", dir, err)
	}
	ts := float64(tr.timescale)
	from, to := int64(math.Round(start*ts)), int64(math.Round(end*ts))
	// The rendition's files around the segment, by their EXTINF starts, widened
	// while the frames read do not reach past the segment on either side.
	lo, hi := 0, len(pl.segments)-1
	for i, s := range pl.segments {
		if s.start <= start-2 {
			lo = i
		}
		if s.start < end+2 {
			hi = i
		}
	}
	if k == 0 {
		lo = 0
	}
	if last {
		hi = len(pl.segments) - 1
	}
	mid := func(f audioFrame) int64 { return 2*(f.pts+tr.shift) + f.dur } // twice the middle's presentation
	var frames []audioFrame
	for {
		frames, err = readAudioFrames(dir, pl, lo, hi, tr.trex)
		if err != nil {
			return nil, err
		}
		switch {
		case lo > 0 && (len(frames) == 0 || mid(frames[0]) >= 2*from):
			lo--
			continue
		case hi < len(pl.segments)-1 && (len(frames) == 0 || mid(frames[len(frames)-1]) < 2*to):
			hi++
			continue
		}
		break
	}
	first, stop := -1, len(frames)
	for i, f := range frames {
		if k > 0 && mid(f) < 2*from {
			continue
		}
		if !last && mid(f) >= 2*to {
			stop = i
			break
		}
		if first < 0 {
			first = i
		}
	}
	if first < 0 || first >= stop {
		return nil, errNoAudioFrames
	}
	frag, err := mp4.CreateFragment(uint32(k+1), tr.trackID)
	if err != nil {
		return nil, err
	}
	for i := first; i < stop; i++ {
		f := frames[i]
		dur := f.dur
		if i+1 < len(frames) && frames[i+1].dts > f.dts {
			dur = frames[i+1].dts - f.dts
		}
		frag.AddFullSample(mp4.FullSample{Sample: mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: uint32(dur), Size: uint32(len(f.data)),
			CompositionTimeOffset: int32(f.pts - f.dts)}, DecodeTime: uint64(f.dts), Data: f.data})
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

// readAudioFrames reads the frames of the rendition's segments lo … hi, in
// decode order.
func readAudioFrames(dir string, pl *rendition, lo, hi int, trex *mp4.TrexBox) ([]audioFrame, error) {
	var out []audioFrame
	for _, s := range pl.segments[lo : hi+1] {
		b, err := readPackaged(filepath.Join(dir, s.uri))
		if err != nil {
			return nil, err
		}
		frags, _, err := cmaf.ReadFragments(b, trex)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.uri, err)
		}
		for _, fr := range frags {
			for _, smp := range fr.Samples {
				out = append(out, audioFrame{dts: smp.DTS, pts: smp.PTS, dur: smp.Dur, data: smp.Data})
			}
		}
	}
	return out, nil
}
