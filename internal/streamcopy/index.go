// Package streamcopy serves an original's streams as they are: its video
// and the audio tracks a client decodes, repackaged into CMAF without an
// encoder. An original is indexed once — its keyframes, from the container's
// own index (a Matroska file's Cues, an MP4's sample tables), and the CMAF
// header of each track — and a segment of it is then one read of the byte
// range its keyframes bound, written as a fragment per track.
//
// Timestamps are the original's, on the timeline every on-the-fly rendition
// shares: presentation zero is the container's start time (an ffprobe's
// format start_time), as ffmpeg's and the live worker's output has it, so a
// copied video plays in step with audio converted elsewhere.
package streamcopy

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// IndexVersion is the version of what an Index holds; a persisted index of
// another version is built anew.
const IndexVersion = 1

// TargetSegment is the least duration of a segment, in seconds: a segment
// runs from a keyframe to the first keyframe at least this much later.
const TargetSegment = 6.0

// maxKeyframeGap is the longest stretch without a keyframe an index may have
// (seconds): an index sparser than that does not say where segments can
// start, and the file is scanned for its keyframes instead.
const maxKeyframeGap = 20.0

// Kinds of original.
const (
	KindMatroska = "matroska"
	KindMP4      = "mp4"
)

// How an index's keyframes were found.
const (
	FromCues = "cues" // a Matroska file's index
	FromScan = "scan" // its Clusters read through
	FromMoov = "moov" // an MP4's sample tables
)

// ErrNoIndex is an original whose container has no index the copy can go by:
// a Matroska file without Cues for its video (or Cues too sparse). Its
// keyframes can be found by reading it through (ScanMatroska).
var ErrNoIndex = errors.New("the file has no usable index")

// ErrUnsupported wraps the reasons an original's video is not copied.
var ErrUnsupported = errUnsupported

// Index is an original indexed for the copy.
type Index struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // unix nanoseconds
	Source  string `json:"source"`
	// OriginNs is the presentation's zero on the file's clock, DurationNs
	// how long it lasts from there (the probe's).
	OriginNs   int64        `json:"originNs"`
	DurationNs int64        `json:"durationNs"`
	Video      VideoTrack   `json:"video"`
	Audio      []AudioTrack `json:"audio"`
	Keyframes  []Keyframe   `json:"keyframes"`

	// A Matroska file's Segment: where its payload starts and ends, its
	// timestamp scale, and the content encodings of the tracks copied.
	SegmentData int64                 `json:"segmentData,omitempty"`
	SegmentEnd  int64                 `json:"segmentEnd,omitempty"`
	TSScale     int64                 `json:"tsScale,omitempty"`
	FirstData   int64                 `json:"firstData,omitempty"` // the first Cluster
	Encodings   map[uint64][]Encoding `json:"encodings,omitempty"`

	spans []Span
	mp4   *mp4Source // an MP4's sample tables, in memory only
}

// Encoding is a Matroska track's content encoding (mkv.ContentEncoding).
type Encoding struct {
	Order        uint64 `json:"order,omitempty"`
	Scope        uint64 `json:"scope"`
	Type         uint64 `json:"type,omitempty"`
	CompAlgo     uint64 `json:"algo,omitempty"`
	CompSettings []byte `json:"settings,omitempty"`
}

// Keyframe is a keyframe a segment may start at.
type Keyframe struct {
	// PTS is its presentation time in the video track's ticks, from the
	// origin.
	PTS int64 `json:"t"`
	// TS is its timestamp as the container has it (Matroska: timestamp
	// units), what its block is known by; Pos where it is: a Matroska
	// keyframe's Cluster (absolute), an MP4 keyframe's sample number.
	TS  int64 `json:"ts"`
	Pos int64 `json:"p"`
}

// VideoTrack is the video track copied.
type VideoTrack struct {
	Number uint64 `json:"n"` // Matroska TrackNumber, MP4 track ID
	// Codec is its CODECS entry (RFC 6381); CodecDV the one for a client that
	// plays Dolby Vision, when the stream is.
	Codec     string `json:"codec"`
	CodecDV   string `json:"codecDV,omitempty"`
	Timescale uint32 `json:"ts"`
	// FrameTicks and GridOrigin are the frame grid its timestamps are put on
	// when within a millisecond of it (0: none).
	FrameTicks int64 `json:"frame,omitempty"`
	GridOrigin int64 `json:"grid,omitempty"`
	// Shift is the composition time its edit list takes off (the media time
	// of its one edit): the frames' reordering delay, and Base.
	Shift int64 `json:"shift,omitempty"`
	// Base is how far its decode times are moved up so that none is below 0
	// (a track whose first frame is presented before the timeline's zero);
	// the edit list takes it off again.
	Base   int64 `json:"base,omitempty"`
	Width  int   `json:"w"`
	Height int   `json:"h"`
	// DVProfile is the stream's Dolby Vision profile, 0 when it is none.
	DVProfile       int `json:"dv,omitempty"`
	DVCompatibility int `json:"dvCompat,omitempty"`
	// Init is the CMAF header; InitDV the one for a client that plays Dolby
	// Vision (its configuration record kept).
	Init   []byte `json:"init"`
	InitDV []byte `json:"initDV,omitempty"`
	// DefaultFrame is a frame's duration in ticks, for the last frame of the
	// file.
	DefaultFrame int64 `json:"defaultFrame,omitempty"`
}

// AudioTrack is one audio track of the original, in file order: the n-th is
// ffmpeg's 0:a:n.
type AudioTrack struct {
	Number uint64 `json:"n"`
	// Codec is its CODECS entry when it can be copied; Why says why not
	// otherwise.
	Codec     string `json:"codec,omitempty"`
	Why       string `json:"why,omitempty"`
	Timescale uint32 `json:"ts,omitempty"`
	Channels  int    `json:"ch,omitempty"`
	// FrameSamples is how many samples a frame holds; GridOrigin the first
	// frame's presentation in samples, which every frame is put on the grid
	// of.
	FrameSamples int   `json:"frame,omitempty"`
	GridOrigin   int64 `json:"grid,omitempty"`
	// DelayUnits is a Matroska track's CodecDelay in timestamp units: taken
	// off its blocks' timestamps, as ffmpeg's demuxer does.
	DelayUnits int64 `json:"delay,omitempty"`
	// Base is how far its decode times are moved up so that none is below 0
	// (its first frame presented before the timeline's zero: an encoder's
	// priming), taken off again by its header's edit list.
	Base int64  `json:"base,omitempty"`
	Init []byte `json:"init,omitempty"`
}

// Copyable says whether the audio track can be copied.
func (a AudioTrack) Copyable() bool { return a.Codec != "" && len(a.Init) > 0 }

// Span is a segment of the copy: from keyframe First, presented from Start
// for Dur seconds (the first from 0).
type Span struct {
	First, Next int // Next: the keyframe the next segment starts at (len(Keyframes) for the last)
	Start, Dur  float64
}

// Spans are the segments of the copy, its keyframes grouped into segments of
// at least TargetSegment seconds, the last up to the end.
func (ix *Index) Spans() []Span {
	if ix.spans != nil || len(ix.Keyframes) == 0 || ix.Video.Timescale == 0 {
		return ix.spans
	}
	ts := float64(ix.Video.Timescale)
	dur := float64(ix.DurationNs) / 1e9
	var out []Span
	first := 0
	for i := 1; i < len(ix.Keyframes); i++ {
		if float64(ix.Keyframes[i].PTS-ix.Keyframes[first].PTS)/ts >= TargetSegment {
			out = append(out, Span{First: first, Next: i})
			first = i
		}
	}
	out = append(out, Span{First: first, Next: len(ix.Keyframes)})
	for i := range out {
		if i > 0 {
			out[i].Start = float64(ix.Keyframes[out[i].First].PTS) / ts
		}
		end := dur
		if i+1 < len(out) {
			end = float64(ix.Keyframes[out[i+1].First].PTS) / ts
		}
		out[i].Dur = end - out[i].Start
	}
	// A last keyframe at the very end (or past the probe's duration) leaves a
	// segment of nothing: it goes into the one before.
	if n := len(out); n > 1 && out[n-1].Dur < 0.1 {
		out[n-2].Next, out[n-2].Dur = out[n-1].Next, out[n-2].Dur+math.Max(0, out[n-1].Dur)
		out = out[:n-1]
	}
	ix.spans = out
	return out
}

// Check says whether the index is one segments can be made by: keyframes
// from the start to the end, none farther apart than maxKeyframeGap, in
// order. A Matroska index that is not is ErrNoIndex.
func (ix *Index) Check() error {
	if ix.DurationNs <= 0 {
		return unsupported("a file of no known duration")
	}
	if len(ix.Keyframes) == 0 {
		return ErrNoIndex
	}
	ts := float64(ix.Video.Timescale)
	prev := int64(-1)
	for i, k := range ix.Keyframes {
		if k.PTS <= prev {
			return fmt.Errorf("%w: keyframe %d at %d is not after %d", ErrNoIndex, i, k.PTS, prev)
		}
		if i > 0 && float64(k.PTS-prev)/ts > maxKeyframeGap {
			return fmt.Errorf("%w: %.1f s without a keyframe at %.1f s", ErrNoIndex, float64(k.PTS-prev)/ts, float64(prev)/ts)
		}
		if i > 0 && k.Pos < ix.Keyframes[i-1].Pos {
			return fmt.Errorf("%w: keyframe %d lies before the one before it", ErrNoIndex, i)
		}
		if ix.Kind == KindMatroska && (k.Pos < ix.SegmentData || k.Pos >= ix.Size) {
			return fmt.Errorf("%w: keyframe %d at %d, outside the file's %d bytes", ErrNoIndex, i, k.Pos, ix.Size)
		}
		prev = k.PTS
	}
	dur := float64(ix.DurationNs) / 1e9
	if first := float64(ix.Keyframes[0].PTS) / ts; first > maxKeyframeGap/2 {
		return fmt.Errorf("%w: the first keyframe is at %.1f s", ErrNoIndex, first)
	}
	if last := float64(prev) / ts; dur > 0 && dur-last > maxKeyframeGap {
		return fmt.Errorf("%w: the last keyframe is %.1f s before the end", ErrNoIndex, dur-last)
	}
	return nil
}

// Bitrates are the copy's peak and average bit rates (bit/s): the largest of
// its segments' and the whole's, in what a segment reads — a Matroska
// file's every track between its keyframes, an MP4's video and audio
// samples in the span (the last segment of a Matroska file, which runs to
// the end of its data, is left out of the peak).
func (ix *Index) Bitrates() (peak, average int) {
	spans := ix.Spans()
	var total, dur float64
	for i, s := range spans {
		bytes := ix.spanBytes(i, s)
		total += bytes
		dur += s.Dur
		if s.Dur > 0 && (i < len(spans)-1 || len(spans) == 1) {
			peak = max(peak, int(bytes*8/s.Dur))
		}
	}
	if dur > 0 {
		average = int(total * 8 / dur)
	}
	return max(peak, average), average
}

// spanBytes is how many bytes segment i (s) of the copy holds.
func (ix *Index) spanBytes(i int, s Span) float64 {
	switch ix.Kind {
	case KindMatroska:
		end := ix.SegmentEnd
		if s.Next < len(ix.Keyframes) {
			end = ix.Keyframes[s.Next].Pos
		}
		start := ix.Keyframes[s.First].Pos
		if i == 0 && ix.FirstData > 0 {
			start = min(start, ix.FirstData)
		}
		return float64(end - start)
	case KindMP4:
		if ix.mp4 == nil {
			return 0
		}
		v := ix.mp4.video
		v0, v1 := int(ix.Keyframes[s.First].Pos), len(v.samples)
		if s.Next < len(ix.Keyframes) {
			v1 = int(ix.Keyframes[s.Next].Pos)
		}
		var n float64
		for _, smp := range v.samples[v0:v1] {
			n += float64(smp.size)
		}
		// The audio by its share of the whole.
		for _, a := range ix.mp4.audio {
			if a == nil || len(a.samples) == 0 {
				continue
			}
			var all float64
			for _, smp := range a.samples {
				all += float64(smp.size)
			}
			if ix.DurationNs > 0 {
				n += all * s.Dur / (float64(ix.DurationNs) / 1e9)
			}
		}
		return n
	}
	return 0
}

// Output is a segment of the copy: its video fragment and the fragment of
// each audio track asked for (by its place among the audio tracks), each a
// styp, a moof and an mdat on the track's own timeline.
type Output struct {
	Video []byte
	Audio map[int][]byte
	// Read is how many bytes of the original were read.
	Read int64
}

// Produce makes segment n of the copy from the original r: its video and
// the audio tracks audio (places among the audio tracks, each copyable).
func (ix *Index) Produce(r io.ReaderAt, n int, audio []int) (*Output, error) {
	spans := ix.Spans()
	if n < 0 || n >= len(spans) {
		return nil, ErrPastEnd
	}
	for _, a := range audio {
		if a < 0 || a >= len(ix.Audio) || !ix.Audio[a].Copyable() {
			return nil, fmt.Errorf("audio track %d is not copied", a)
		}
	}
	switch ix.Kind {
	case KindMatroska:
		return ix.produceMatroska(r, n, audio)
	case KindMP4:
		return ix.produceMP4(r, n, audio)
	}
	return nil, fmt.Errorf("an index of kind %q", ix.Kind)
}

// ErrPastEnd is a segment past the last.
var ErrPastEnd = errors.New("segment past the end")

// nsToTicks is a time in nanoseconds in ticks of timescale, rounded to the
// nearest tick; exact for any timestamp of a whole number of milliseconds
// when the timescale is a multiple of 1000.
func nsToTicks(ns int64, timescale uint32) int64 {
	ts := int64(timescale)
	q, r := ns/1e9, ns%1e9
	v := q*ts + r*ts/1e9
	if rem := (r * ts) % 1e9; rem*2 >= 1e9 {
		v++
	} else if rem*2 <= -1e9 {
		v--
	}
	return v
}

// snap puts t on the grid origin + k·frame when it lies within tol of it.
func snap(t, origin, frame, tol int64) int64 {
	if frame <= 0 {
		return t
	}
	d := t - origin
	k := d / frame
	if r := d - k*frame; r*2 >= frame {
		k++
	} else if r*2 <= -frame {
		k--
	}
	g := origin + k*frame
	if t-g <= tol && g-t <= tol {
		return g
	}
	return t
}

// videoGrid is the timescale of a video track whose frames last
// defaultDurNs, and its frames' ticks on it: a timescale both its frame rate
// and whole milliseconds are exact in. 90 kHz without a frame grid when the
// rate is none of the usual ones (or not given).
func videoGrid(defaultDurNs uint64) (timescale uint32, frameTicks int64) {
	if defaultDurNs == 0 {
		return 90000, 0
	}
	for _, r := range []struct{ num, den int64 }{
		{24000, 1001}, {30000, 1001}, {60000, 1001}, {48000, 1001}, {120000, 1001},
		{24, 1}, {25, 1}, {30, 1}, {48, 1}, {50, 1}, {60, 1}, {100, 1}, {120, 1}, {15, 1}, {12, 1},
	} {
		frameNs := float64(r.den) * 1e9 / float64(r.num)
		if math.Abs(float64(defaultDurNs)-frameNs) > 1000 {
			continue
		}
		if r.den == 1001 {
			return uint32(r.num), 1001
		}
		for _, ts := range []int64{24000, 90000, 120000} {
			if ts%r.num == 0 {
				return uint32(ts), ts / r.num
			}
		}
	}
	return 90000, 0
}
