package streamcopy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/Eyevinn/mp4ff/aac"

	"github.com/zaentrum/chino-stream/internal/mkv"
)

// A Matroska original. Its index is its Cues: the Cluster of every video
// keyframe, a few kilobytes at the end of the file. Segment n is the byte
// range from its first keyframe's block to the next segment's: the video
// blocks in it, in decode order, and the audio blocks in it, so consecutive
// segments hold every block once, whichever segments are read. One read of
// the range (and of the few blocks after it that say when the next segment's
// presentation starts) makes the video fragment and every audio fragment of
// the segment.

// Options say what an index needs from the probe: presentation zero on the
// file's clock and the presentation's duration, both in nanoseconds, and
// the file's identity.
type Options struct {
	OriginNs, DurationNs int64
	Path                 string
	ModTime              int64
}

// headLimit bounds the read of a file's first Clusters that finds each audio
// track's first frame and the video's reordering.
const headLimit = 16 << 20

// headFrames is how many video frames of the first Clusters are read for the
// reordering delay (the SPS's reorder count, where it gives one, is taken
// at the least): two seconds or so, a megabyte or a few.
const headFrames = 48

// IndexMatroska indexes the Matroska original r of size bytes: its tracks'
// headers (reading its first Clusters for what they need) and the keyframes
// its Cues list. An index whose file has no usable Cues has no keyframes
// (Check says ErrNoIndex): ScanMatroska finds them.
func IndexMatroska(r io.ReaderAt, size int64, o Options) (*Index, error) {
	f, err := mkv.Open(r, size)
	if err != nil {
		return nil, err
	}
	ix := &Index{Version: IndexVersion, Kind: KindMatroska, Path: o.Path, Size: size, ModTime: o.ModTime, Source: FromCues,
		OriginNs: o.OriginNs, DurationNs: o.DurationNs, SegmentData: f.SegmentData, SegmentEnd: f.SegmentEnd,
		TSScale: f.TimestampScale, FirstData: f.FirstCluster, Encodings: map[uint64][]Encoding{}}
	if ix.DurationNs <= 0 && f.Duration > 0 {
		ix.DurationNs = int64(f.Duration*float64(f.TimestampScale)) - o.OriginNs
	}
	var vt *mkv.Track
	var audio []*mkv.Track
	for i := range f.Tracks {
		switch t := &f.Tracks[i]; t.Type {
		case mkv.TrackVideo:
			if vt == nil {
				vt = t
			}
		case mkv.TrackAudio:
			audio = append(audio, t)
		}
	}
	if vt == nil {
		return nil, unsupported("no video track")
	}
	if ix.FirstData < 0 {
		// No Cluster found after the header: the first cue's is the first.
		for _, c := range f.Cues {
			if len(c.Positions) > 0 {
				ix.FirstData = f.SegmentData + int64(c.Positions[0].Cluster)
				break
			}
		}
		if ix.FirstData < 0 {
			return nil, fmt.Errorf("%w: no Cluster found", mkv.ErrFormat)
		}
	}
	cfg, dv, err := matroskaVideo(vt)
	if err != nil {
		return nil, err
	}
	ix.Video.Number = vt.Number
	ix.Video.Timescale, ix.Video.FrameTicks = videoGrid(vt.DefaultDuration)
	ix.Video.Width, ix.Video.Height = cfg.width, cfg.height
	if vt.Video != nil && vt.Video.PixelWidth > 0 && vt.Video.PixelHeight > 0 {
		ix.Video.Width, ix.Video.Height = int(vt.Video.PixelWidth), int(vt.Video.PixelHeight)
	}
	ix.Video.Codec = cfg.codecs
	if len(vt.Encodings) > 0 {
		ix.Encodings[vt.Number] = encodingsOf(vt.Encodings)
	}

	acfg := make([]*audioConfig, len(audio))
	for i, t := range audio {
		at := AudioTrack{Number: t.Number}
		acfg[i], at.Why = matroskaAudio(t)
		if t.CodecDelay > 0 && f.TimestampScale > 0 {
			at.DelayUnits = (int64(t.CodecDelay) + f.TimestampScale/2) / f.TimestampScale
		}
		if acfg[i] != nil && len(t.Encodings) > 0 {
			ix.Encodings[t.Number] = encodingsOf(t.Encodings)
		}
		ix.Audio = append(ix.Audio, at)
	}

	// The first Clusters: each audio track's first frame (an AC-3 or E-AC-3
	// track's header is in its frames) and where it starts, the video's first
	// frames and how far they are reordered.
	head, err := ix.readHead(r, f, vt, audio, acfg, cfg)
	if err != nil {
		return nil, err
	}
	// No decode time below 0: a track presented from before the timeline's
	// zero is moved up, and its edit list takes the move off again.
	ix.Video.Base = max(0, -ix.Video.GridOrigin)
	shift := ix.Video.Shift + ix.Video.Base
	dispW, dispH := displaySize(vt, cfg)
	if ix.Video.Init, err = videoInit(cfg.sampleEntry, cfg, nil, ix.Video.Timescale, shift, dispW, dispH, vt.Language); err != nil {
		return nil, err
	}
	if dv != nil {
		ix.Video.DVProfile, ix.Video.DVCompatibility = dv.profile, dv.compatibility
		entry, codecs := cfg.sampleEntry, cfg.codecs
		if dv.compatibility == 0 {
			switch cfg.sampleEntry {
			case "hvc1":
				entry, codecs = "dvh1", dv.codecs()
			case "av01":
				entry, codecs = "dav1", fmt.Sprintf("dav1.%02d.%02d", dv.profile, dv.level)
			case "avc1":
				entry, codecs = "dva1", fmt.Sprintf("dva1.%02d.%02d", dv.profile, dv.level)
			}
		}
		if ix.Video.InitDV, err = videoInit(entry, cfg, dv, ix.Video.Timescale, shift, dispW, dispH, vt.Language); err != nil {
			return nil, err
		}
		ix.Video.CodecDV = codecs
	}
	for i, c := range acfg {
		if c == nil || ix.Audio[i].Why != "" {
			continue
		}
		a := &ix.Audio[i]
		a.Codec, a.Timescale, a.Channels, a.FrameSamples = c.codecs, uint32(c.sampleRate), c.channels, c.frameSamples
		a.Base = max(0, -a.GridOrigin)
		if a.Init, err = audioInit(c, a.Base, audio[i].Language); err != nil {
			return nil, err
		}
	}

	ix.Keyframes = cueKeyframes(f, vt.Number, ix)
	// The reordering delay in timestamp units, and a frame.
	margin := (ix.Video.Shift + ix.Video.DefaultFrame) * 1e9 / int64(ix.Video.Timescale) / max(1, ix.TSScale)
	if !head.confirms(ix.Keyframes, margin+1) {
		// Cues the head shows wrong — a cue on a frame that is no keyframe, or
		// in another Cluster than its frame's — are not gone by: the file is
		// read through for its keyframes (Check says ErrNoIndex).
		ix.Keyframes = nil
	}
	return ix, nil
}

// headKeys are what the read of a file's head saw of its video: its
// keyframes (each timestamp's Cluster), the latest timestamp of a frame, and
// the Cluster it stopped in (every one before it was read through).
type headKeys struct {
	at     map[int64]int64
	latest int64
	last   int64
}

// confirms says whether each of the keyframes kfs the head reached is one it
// saw, in that Cluster: each in a Cluster it read through, and each presented
// more than margin (timestamp units: the reordering delay) before the latest
// frame it read — the frames decoded before a keyframe are presented before
// it, so a frame presented after it comes after it in the file.
func (h headKeys) confirms(kfs []Keyframe, margin int64) bool {
	for _, k := range kfs {
		if k.Pos >= h.last && (len(h.at) == 0 || k.TS > h.latest-margin) {
			continue
		}
		if pos, ok := h.at[k.TS]; !ok || pos != k.Pos {
			return false
		}
	}
	return true
}

// displaySize is the size a video track is displayed at: its DisplayWidth ×
// DisplayHeight in pixels when it gives them, else its frame's.
func displaySize(t *mkv.Track, cfg *videoConfig) (int, int) {
	if v := t.Video; v != nil && v.DisplayUnit == 0 && v.DisplayWidth > 0 && v.DisplayHeight > 0 {
		return int(v.DisplayWidth), int(v.DisplayHeight)
	}
	return cfg.width, cfg.height
}

func encodingsOf(es []mkv.ContentEncoding) []Encoding {
	out := make([]Encoding, len(es))
	for i, e := range es {
		out[i] = Encoding{Order: e.Order, Scope: e.Scope, Type: e.Type, CompAlgo: e.CompAlgo, CompSettings: e.CompSettings}
	}
	return out
}

// matroskaVideo is the configuration of a video track and its Dolby Vision
// record (nil for none).
func matroskaVideo(t *mkv.Track) (*videoConfig, *dvRecord, error) {
	if !t.Decodable() {
		return nil, nil, unsupported("the video track's frames are encrypted or compressed")
	}
	var cfg *videoConfig
	var err error
	switch t.CodecID {
	case "V_MPEGH/ISO/HEVC":
		cfg, err = hevcConfig(t.CodecPrivate)
	case "V_MPEG4/ISO/AVC":
		cfg, err = avcConfig(t.CodecPrivate)
	case "V_AV1":
		w, h := 0, 0
		if t.Video != nil {
			w, h = int(t.Video.PixelWidth), int(t.Video.PixelHeight)
		}
		cfg, err = av1Config(t.CodecPrivate, w, h)
	default:
		return nil, nil, unsupported("video codec %s", t.CodecID)
	}
	if err != nil {
		if !errors.Is(err, errUnsupported) {
			// A configuration the copy cannot read is the file's.
			err = fmt.Errorf("%w: %v", errUnsupported, err)
		}
		return nil, nil, err
	}
	var dv *dvRecord
	for _, m := range t.BlockAdditions {
		switch m.IDType {
		case 0x64766343, 0x64767643: // dvcC, dvvC
			name := "dvcC"
			if m.IDType == 0x64767643 {
				name = "dvvC"
			}
			if dv, err = parseDV(name, m.ExtraData); err != nil {
				return nil, nil, fmt.Errorf("%w: %v", errUnsupported, err)
			}
		}
	}
	return cfg, dv, nil
}

// matroskaAudio is the configuration of an audio track the copy can take, or
// why it cannot. An AC-3 or E-AC-3 track's is made of its first frame
// (readHead): nil and no reason until then.
func matroskaAudio(t *mkv.Track) (*audioConfig, string) {
	if !t.Decodable() {
		return nil, "its frames are encrypted or compressed"
	}
	channels := 2
	if t.Audio != nil && t.Audio.Channels > 0 {
		channels = int(t.Audio.Channels)
	}
	switch id := t.CodecID; {
	case id == "A_AAC":
		c, err := aacConfig(t.CodecPrivate, channels)
		if err != nil {
			return nil, err.Error()
		}
		return c, ""
	case strings.HasPrefix(id, "A_AAC/"):
		// A legacy codec ID without an AudioSpecificConfig: made of the ID.
		if t.Audio == nil {
			return nil, "no sampling frequency"
		}
		asc := aac.AudioSpecificConfig{ObjectType: aac.AAClc, ChannelConfiguration: byte(channels), SamplingFrequency: int(t.Audio.SamplingFrequency)}
		if strings.HasSuffix(id, "/SBR") {
			asc.ObjectType, asc.SBRPresentFlag = aac.HEAACv1, true
			asc.ExtensionFrequency = int(t.Audio.OutputSamplingFrequency)
		}
		if _, ok := aac.ReverseFrequencies[asc.SamplingFrequency]; !ok || channels > 7 {
			return nil, fmt.Sprintf("AAC at %d Hz with %d channels", asc.SamplingFrequency, channels)
		}
		var b bytes.Buffer
		if err := asc.Encode(&b); err != nil {
			return nil, err.Error()
		}
		c, err := aacConfig(b.Bytes(), channels)
		if err != nil {
			return nil, err.Error()
		}
		return c, ""
	case id == "A_AC3", id == "A_EAC3":
		return nil, "" // from its first frame
	}
	return nil, "audio codec " + t.CodecID
}

// readHead reads the first Clusters of f: each audio track's first frame
// (the configuration of an AC-3 or E-AC-3 one) and its timeline's start, the
// video's first frames — its reordering delay, the composition shift its
// header takes off — and where its frame grid starts; and the keyframes it
// saw.
func (ix *Index) readHead(r io.ReaderAt, f *mkv.File, vt *mkv.Track, audio []*mkv.Track, acfg []*audioConfig, cfg *videoConfig) (headKeys, error) {
	head := headKeys{at: map[int64]int64{}, latest: math.MinInt64, last: ix.FirstData}
	rd := f.Blocks(r, ix.FirstData)
	byNumber := map[uint64]int{}
	for i, t := range audio {
		byNumber[t.Number] = i
	}
	firstSeen := make([]bool, len(audio))
	pending := 0
	for i := range audio {
		if ix.Audio[i].Why == "" {
			pending++
		}
	}
	var pts []int64
	keyframes := 0
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return head, err
		}
		head.last = b.Cluster
		if b.Track == vt.Number {
			head.latest = max(head.latest, b.Timestamp)
			if b.Keyframe {
				keyframes++
				head.at[b.Timestamp] = b.Cluster
			}
			if len(pts) < headFrames && (len(pts) > 0 || b.Keyframe) {
				pts = append(pts, ix.ticks(b.Timestamp, 0, ix.Video.Timescale))
			}
		} else if i, ok := byNumber[b.Track]; ok && !firstSeen[i] && len(b.Frames) > 0 {
			firstSeen[i] = true
			a := &ix.Audio[i]
			if a.Why == "" {
				pending--
				switch audio[i].CodecID {
				case "A_AC3":
					c, err := ac3Config(b.Frames[0])
					if err != nil {
						a.Why = err.Error()
					}
					acfg[i] = c
				case "A_EAC3":
					c, err := eac3Config(b.Frames[0], int(channelsOf(audio[i])))
					if err != nil {
						a.Why = err.Error()
					}
					acfg[i] = c
				}
				if c := acfg[i]; c != nil && a.Why == "" {
					a.GridOrigin = ix.ticks(b.Timestamp, a.DelayUnits, uint32(c.sampleRate))
				}
			}
		}
		if (len(pts) >= headFrames || keyframes > 1) && pending <= 0 {
			break
		}
		if rd.BytesRead() > headLimit {
			break
		}
	}
	for i := range audio {
		if ix.Audio[i].Why == "" && (!firstSeen[i] || acfg[i] == nil) {
			ix.Audio[i].Why = "no frame of it in the first clusters"
		}
	}
	if len(pts) == 0 {
		return head, fmt.Errorf("%w: no video keyframe in the first clusters", mkv.ErrFormat)
	}
	v := &ix.Video
	v.GridOrigin = pts[0]
	for i := range pts {
		pts[i] = snap(pts[i], v.GridOrigin, v.FrameTicks, int64(v.Timescale)/1000)
	}
	// The reordering delay: how far the frames' presentation runs behind
	// their decode order, at most — the shift that keeps every composition
	// offset at or above 0 (the SPS's reorder count, where it gives one, at
	// the least).
	sorted := append([]int64(nil), pts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var shift int64
	for i := range pts {
		shift = max(shift, sorted[i]-pts[i])
	}
	frame := v.FrameTicks
	if frame == 0 && len(sorted) > 1 {
		frame = sorted[len(sorted)-1] - sorted[len(sorted)-2]
		for i := 1; i < len(sorted); i++ {
			if d := sorted[i] - sorted[i-1]; d > 0 && d < frame {
				frame = d
			}
		}
	}
	if frame <= 0 {
		frame = int64(v.Timescale) / 25
	}
	if cfg.reorder > 0 {
		shift = max(shift, int64(cfg.reorder)*frame)
	}
	v.Shift, v.DefaultFrame = shift, frame
	return head, nil
}

func channelsOf(t *mkv.Track) uint64 {
	if t.Audio == nil {
		return 0
	}
	return t.Audio.Channels
}

// ticks is the presentation of a Matroska timestamp ts (timestamp units) in
// ticks of timescale, from the origin, a codec delay of delay units taken off.
func (ix *Index) ticks(ts, delay int64, timescale uint32) int64 {
	return nsToTicks((ts-delay)*ix.TSScale-ix.OriginNs, timescale)
}

// cueKeyframes are the keyframes of the video track number the Cues of f
// list, in order, each once.
func cueKeyframes(f *mkv.File, number uint64, ix *Index) []Keyframe {
	var out []Keyframe
	for _, c := range f.Cues {
		for _, p := range c.Positions {
			if p.Track != number {
				continue
			}
			out = append(out, Keyframe{TS: int64(c.Time), Pos: f.SegmentData + int64(p.Cluster)})
			break
		}
	}
	return ix.keyframes(out)
}

// keyframes puts keyframes found by their timestamps and Clusters in order,
// each once, with their presentation.
func (ix *Index) keyframes(kfs []Keyframe) []Keyframe {
	sort.SliceStable(kfs, func(i, j int) bool { return kfs[i].TS < kfs[j].TS })
	out := kfs[:0]
	for _, k := range kfs {
		if len(out) > 0 && out[len(out)-1].TS == k.TS {
			continue
		}
		k.PTS = snap(ix.ticks(k.TS, 0, ix.Video.Timescale), ix.Video.GridOrigin, ix.Video.FrameTicks, int64(ix.Video.Timescale)/1000)
		out = append(out, k)
	}
	return out
}

// ScanMatroska finds the keyframes of the indexed Matroska original r by
// reading its Clusters through (only the blocks' headers are looked at):
// for a file whose Cues are missing or too sparse. The index's keyframes are
// replaced.
func (ix *Index) ScanMatroska(r io.ReaderAt) error {
	f := ix.mkvFile()
	rd := f.Blocks(r, ix.FirstData)
	rd.Want = func(uint64) bool { return false }
	var kfs []Keyframe
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if b.Track == ix.Video.Number && b.Keyframe {
			kfs = append(kfs, Keyframe{TS: b.Timestamp, Pos: b.Cluster})
		}
	}
	ix.Keyframes, ix.Source, ix.spans = ix.keyframes(kfs), FromScan, nil
	return nil
}

// mkvFile is the Matroska file of the index as the block reader needs it.
func (ix *Index) mkvFile() *mkv.File {
	f := &mkv.File{Size: ix.Size, SegmentData: ix.SegmentData, SegmentEnd: ix.SegmentEnd, TimestampScale: ix.TSScale}
	for n, es := range ix.Encodings {
		t := mkv.Track{Number: n}
		for _, e := range es {
			t.Encodings = append(t.Encodings, mkv.ContentEncoding{Order: e.Order, Scope: e.Scope, Type: e.Type, CompAlgo: e.CompAlgo,
				CompSettings: e.CompSettings})
		}
		f.Tracks = append(f.Tracks, t)
	}
	return f
}

// segmentChunk is the read size of a segment's byte range (a variable for
// the tests).
var segmentChunk = 4 << 20

// leadingLimit bounds the blocks read after the next segment's keyframe for
// where its presentation starts (its leading pictures, of an open GOP).
const leadingLimit = 64

// maxSegmentRead bounds what one segment reads (bytes).
const maxSegmentRead = 512 << 20

// produceMatroska makes segment n of a Matroska original.
func (ix *Index) produceMatroska(r io.ReaderAt, n int, audio []int) (*Output, error) {
	spans := ix.Spans()
	sp := spans[n]
	kf := ix.Keyframes[sp.First]
	var next *Keyframe
	if sp.Next < len(ix.Keyframes) {
		next = &ix.Keyframes[sp.Next]
	}
	f := ix.mkvFile()
	start := kf.Pos
	if n == 0 && ix.FirstData > 0 && ix.FirstData < start {
		start = ix.FirstData
	}
	rd := f.Blocks(r, start)
	// Large reads: a segment is megabytes, and what a read takes past its end
	// is the next segment's start, which its prefetch reads next.
	rd.Chunk = segmentChunk
	want := map[uint64]int{}
	for _, a := range audio {
		want[ix.Audio[a].Number] = a
	}
	vnum := ix.Video.Number
	rd.Want = func(t uint64) bool {
		_, ok := want[t]
		return t == vnum || ok
	}
	v := &ix.Video
	tol := int64(v.Timescale) / 1000
	// A frame's presentation, moved up by the track's base: its decode time
	// sorts from there.
	vpts := func(ts int64) int64 {
		return snap(ix.ticks(ts, 0, v.Timescale), v.GridOrigin, v.FrameTicks, tol) + v.Base
	}

	var frames []videoFrame
	got := map[int][]fragmentSample{}
	// The video starts at the segment's keyframe; the audio of segment 0 at
	// the first Cluster, of any other at that keyframe too: what lies before
	// it in the file is the segment before's.
	videoIn, audioIn, after := false, n == 0, false
	nextPTS, leadMin, leading := int64(0), int64(0), 0
	// A keyframe not where the index says fails the segment at once, not at
	// the end of the file: keyframes come in the order they are presented,
	// so a later one says it was passed, and no frame of the segment is
	// presented much past its span.
	missing := func(k Keyframe, seg int) error {
		return fmt.Errorf("%w: the keyframe at %d of segment %d is not in the file where its index says", mkv.ErrFormat, k.TS, seg)
	}
	spanEnd := int64((sp.Start + sp.Dur + maxKeyframeGap) * 1e9)
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if rd.BytesRead() > maxSegmentRead {
			return nil, fmt.Errorf("%w: segment %d reads more than %d bytes", mkv.ErrFormat, n, maxSegmentRead)
		}
		if b.Track == vnum {
			if b.Timestamp*ix.TSScale-ix.OriginNs > spanEnd {
				if !videoIn {
					return nil, missing(kf, n)
				}
				if next != nil && !after {
					return nil, missing(*next, n+1)
				}
			}
			switch {
			case after:
				// The next segment's leading pictures, presented before its
				// keyframe: where its presentation starts.
				p := vpts(b.Timestamp)
				if p > nextPTS || leading >= leadingLimit {
					return ix.writeMatroska(n, frames, got, leadMin, true, rd.BytesRead())
				}
				leadMin = min(leadMin, p)
				leading++
				continue
			case !videoIn:
				if b.Keyframe && b.Timestamp > kf.TS {
					return nil, missing(kf, n)
				}
				if !b.Keyframe || b.Timestamp != kf.TS {
					continue
				}
				videoIn, audioIn = true, true
			case next != nil && b.Keyframe && b.Timestamp == next.TS:
				after, nextPTS = true, vpts(b.Timestamp)
				leadMin = nextPTS
				continue
			case next != nil && b.Keyframe && b.Timestamp > next.TS:
				return nil, missing(*next, n+1)
			}
			for _, fr := range b.Frames {
				frames = append(frames, videoFrame{pts: vpts(b.Timestamp), sync: b.Keyframe, data: fr})
			}
			continue
		}
		a, ok := want[b.Track]
		if !ok || !audioIn || after {
			continue
		}
		t := &ix.Audio[a]
		base := ix.ticks(b.Timestamp, t.DelayUnits, t.Timescale)
		for i, fr := range b.Frames {
			dur := int64(t.FrameSamples)
			if t.Codec == "ec-3" {
				dur = eac3Samples(fr, dur)
			}
			p := snap(base+int64(i)*dur, t.GridOrigin, int64(t.FrameSamples), int64(t.Timescale)/1000+1) + t.Base
			s := got[a]
			if k := len(s); k > 0 && p < s[k-1].dts+s[k-1].dur {
				p = s[k-1].dts + s[k-1].dur // overlapping timestamps: frame after frame
			}
			got[a] = append(s, fragmentSample{dts: p, pts: p, dur: dur, sync: true, data: fr})
		}
	}
	if !videoIn {
		return nil, missing(kf, n)
	}
	return ix.writeMatroska(n, frames, got, leadMin, after, rd.BytesRead())
}

// eac3Samples is how many samples an E-AC-3 frame holds (its first
// syncframe's blocks), def when its header does not say.
func eac3Samples(frame []byte, def int64) int64 {
	if len(frame) < 5 || frame[0] != 0x0B || frame[1] != 0x77 {
		return def
	}
	if frame[4]>>6 == 3 { // fscod 3: six blocks
		return 1536
	}
	return [4]int64{256, 512, 768, 1536}[(frame[4]>>4)&3]
}

// videoFrame is a video frame of a segment: its presentation time, whether
// it is a sync sample, and its data.
type videoFrame struct {
	pts  int64
	sync bool
	data []byte
}

// writeMatroska writes segment n's fragments: the video's frames, decode
// times their presentation times sorted, each lasting until the next one's
// decode time and the last until where the next segment's presentation
// starts (end, when known: the segment after it was looked into), and each
// audio track's frames as they are.
func (ix *Index) writeMatroska(n int, frames []videoFrame, got map[int][]fragmentSample, end int64, known bool, read int64) (*Output, error) {
	samples, err := ix.videoSamples(frames, end, known)
	if err != nil {
		return nil, fmt.Errorf("segment %d: %w", n, err)
	}
	out := &Output{Audio: map[int][]byte{}, Read: read}
	if out.Video, err = fragment(uint32(n+1), samples); err != nil {
		return nil, err
	}
	for a, s := range got {
		if out.Audio[a], err = fragment(uint32(n+1), s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// videoSamples lays a segment's video frames (in decode order) on the
// decode timeline: decode times are the presentation times sorted, so the
// segment's decode time starts where its presentation does and ends where
// the next segment's starts; each composition offset is the presentation
// time, shifted by the header's edit, less the decode time.
func (ix *Index) videoSamples(frames []videoFrame, end int64, known bool) ([]fragmentSample, error) {
	if len(frames) == 0 {
		return nil, errors.New("no video frame")
	}
	if !frames[0].sync {
		return nil, errors.New("the first frame is no keyframe")
	}
	dts := make([]int64, len(frames))
	for i, f := range frames {
		dts[i] = f.pts
	}
	sort.Slice(dts, func(i, j int) bool { return dts[i] < dts[j] })
	if dts[0] < 0 {
		return nil, fmt.Errorf("a frame presented at %d, before the start", dts[0])
	}
	for i := 1; i < len(dts); i++ {
		if dts[i] == dts[i-1] {
			return nil, fmt.Errorf("two frames presented at %d", dts[i])
		}
	}
	last := dts[len(dts)-1]
	if !known || end <= last {
		if known && end <= last {
			return nil, fmt.Errorf("the next segment starts at %d, before this one's last frame at %d", end, last)
		}
		end = last + ix.Video.DefaultFrame
	}
	out := make([]fragmentSample, len(frames))
	for i, f := range frames {
		next := end
		if i+1 < len(dts) {
			next = dts[i+1]
		}
		out[i] = fragmentSample{dts: dts[i], pts: f.pts + ix.Video.Shift, dur: next - dts[i], sync: f.sync, data: f.data}
	}
	return out, nil
}
