package streamcopy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// An MP4 original. Its index is its sample tables (a fragmented file's
// track runs): every sample's decode time, composition offset, size and
// place in the file. Its video and each audio track are copied apart, each
// sample entry as the original has it; a segment holds the video samples
// from its keyframe to the next segment's, and each audio track's samples
// presented in the same span. The tables stay in memory: they are not
// persisted.

// mp4Source is an MP4 original's sample tables.
type mp4Source struct {
	video *mp4Track
	audio []*mp4Track // by place among the audio tracks; nil: not copied
}

// mp4Track is one track's samples, laid on the copy's timeline: a sample's
// decode time there is its own plus base, its presentation that plus its
// composition offset less the header's edit (mediaTime).
type mp4Track struct {
	timescale uint32
	base      int64
	mediaTime int64
	samples   []mp4Sample
}

type mp4Sample struct {
	dts    int64 // the original's
	offset int64
	dur    uint32
	size   uint32
	cto    int32
	sync   bool
}

// pts is sample i's presentation on the copy's timeline.
func (t *mp4Track) pts(i int) int64 {
	s := t.samples[i]
	return s.dts + t.base + int64(s.cto) - t.mediaTime
}

// IndexMP4 indexes the MP4 original r of size bytes.
func IndexMP4(r io.ReaderAt, size int64, o Options) (*Index, error) {
	mf, err := mp4.DecodeFile(io.NewSectionReader(r, 0, size), mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return nil, err // the file could not be read: not its format
		}
		return nil, fmt.Errorf("%w: mp4: %v", ErrUnsupported, err)
	}
	if mf.Moov == nil || mf.Moov.Mvhd == nil {
		return nil, errors.New("mp4: no moov")
	}
	ix := &Index{Version: IndexVersion, Kind: KindMP4, Path: o.Path, Size: size, ModTime: o.ModTime, Source: FromMoov,
		OriginNs: o.OriginNs, DurationNs: o.DurationNs, mp4: &mp4Source{}}
	var vt *mp4.TrakBox
	var audio []*mp4.TrakBox
	for _, t := range mf.Moov.Traks {
		if t.Mdia == nil || t.Mdia.Hdlr == nil || t.Mdia.Mdhd == nil || t.Mdia.Minf == nil || t.Mdia.Minf.Stbl == nil || t.Tkhd == nil ||
			t.Mdia.Minf.Stbl.Stsd == nil || len(t.Mdia.Minf.Stbl.Stsd.Children) == 0 {
			continue
		}
		switch t.Mdia.Hdlr.HandlerType {
		case "vide":
			if vt == nil {
				vt = t
			}
		case "soun":
			audio = append(audio, t)
		}
	}
	if vt == nil {
		return nil, unsupported("no video track")
	}
	mvTS := mf.Moov.Mvhd.Timescale
	vtrack, err := mp4TrackOf(mf, vt, o.OriginNs, mvTS)
	if err != nil {
		return nil, fmt.Errorf("%w: video: %v", errUnsupported, err)
	}
	ix.mp4.video = vtrack
	entry := vt.Mdia.Minf.Stbl.Stsd.Children[0]
	vse, ok := entry.(*mp4.VisualSampleEntryBox)
	if !ok {
		return nil, unsupported("video sample entry %s", entry.Type())
	}
	codecs, err := videoCodecs(vse)
	if err != nil {
		if !errors.Is(err, errUnsupported) {
			err = fmt.Errorf("%w: %v", errUnsupported, err)
		}
		return nil, err
	}
	ix.Video = VideoTrack{Number: uint64(vt.Tkhd.TrackID), Codec: codecs, Timescale: vtrack.timescale, Shift: vtrack.mediaTime,
		Width: int(vse.Width), Height: int(vse.Height)}
	plain, dv := splitDV(vse)
	lang := vt.Mdia.Mdhd.GetLanguage()
	if ix.Video.Init, err = trackInit(vt, plain, vtrack.timescale, vtrack.mediaTime, "video", lang); err != nil {
		return nil, err
	}
	if dv != nil {
		ix.Video.DVProfile, ix.Video.DVCompatibility = dv.profile, dv.compatibility
		ix.Video.CodecDV = codecs
		if dv.compatibility == 0 {
			ix.Video.CodecDV = dv.codecs()
		}
		if ix.Video.InitDV, err = trackInit(vt, vse, vtrack.timescale, vtrack.mediaTime, "video", lang); err != nil {
			return nil, err
		}
	}
	for i, s := range vtrack.samples {
		if s.sync {
			ix.Keyframes = append(ix.Keyframes, Keyframe{PTS: vtrack.pts(i), TS: int64(i), Pos: int64(i)})
		}
	}
	sort.SliceStable(ix.Keyframes, func(i, j int) bool { return ix.Keyframes[i].Pos < ix.Keyframes[j].Pos })
	if n := len(vtrack.samples); n > 1 {
		ix.Video.DefaultFrame = int64(vtrack.samples[n-1].dur)
	}

	for _, at := range audio {
		a := AudioTrack{Number: uint64(at.Tkhd.TrackID)}
		track, codecs, channels, err := mp4Audio(mf, at, o.OriginNs, mvTS)
		if err != nil {
			a.Why = err.Error()
			ix.Audio = append(ix.Audio, a)
			ix.mp4.audio = append(ix.mp4.audio, nil)
			continue
		}
		a.Codec, a.Timescale, a.Channels = codecs, track.timescale, channels
		if a.Init, err = trackInit(at, at.Mdia.Minf.Stbl.Stsd.Children[0], track.timescale, track.mediaTime, "audio", at.Mdia.Mdhd.GetLanguage()); err != nil {
			return nil, err
		}
		ix.Audio = append(ix.Audio, a)
		ix.mp4.audio = append(ix.mp4.audio, track)
	}
	return ix, nil
}

// mp4Audio is an audio track to copy: its samples, its CODECS entry and
// channels; an error says why it is not copied.
func mp4Audio(mf *mp4.File, trak *mp4.TrakBox, originNs int64, movieTS uint32) (*mp4Track, string, int, error) {
	entry := trak.Mdia.Minf.Stbl.Stsd.Children[0]
	ase, ok := entry.(*mp4.AudioSampleEntryBox)
	if !ok {
		return nil, "", 0, unsupported("audio sample entry %s", entry.Type())
	}
	channels := int(ase.ChannelCount)
	var codecs string
	switch ase.Type() {
	case "mp4a":
		if ase.Esds == nil || ase.Esds.DecConfigDescriptor == nil {
			return nil, "", 0, unsupported("mp4a without an esds")
		}
		switch oti := ase.Esds.DecConfigDescriptor.ObjectType; oti {
		case 0x40:
			codecs = "mp4a.40.2"
			if dsi := ase.Esds.DecConfigDescriptor.DecSpecificInfo; dsi != nil && len(dsi.DecConfig) > 0 {
				codecs = fmt.Sprintf("mp4a.40.%d", dsi.DecConfig[0]>>3)
			}
		case 0x6B, 0x69:
			codecs = "mp4a.40.34"
		default:
			return nil, "", 0, unsupported("mp4a object type 0x%x", oti)
		}
	case "ac-3":
		codecs = "ac-3"
	case "ec-3":
		codecs = "ec-3"
	case "Opus":
		codecs = "opus"
	default:
		return nil, "", 0, unsupported("audio sample entry %s", ase.Type())
	}
	t, err := mp4TrackOf(mf, trak, originNs, movieTS)
	if err != nil {
		return nil, "", 0, err
	}
	return t, codecs, channels, nil
}

// mp4TrackOf reads trak's samples — its sample tables and, in a fragmented
// file, its fragments — and lays them on the copy's timeline: the original's
// presentation (its edit list applied) less the origin.
func mp4TrackOf(mf *mp4.File, trak *mp4.TrakBox, originNs int64, movieTS uint32) (*mp4Track, error) {
	if err := trak.CheckDataIsSelfContained(); err != nil {
		return nil, err
	}
	ts := trak.Mdia.Mdhd.Timescale
	if ts == 0 {
		return nil, errors.New("zero timescale")
	}
	samples, err := mp4TableSamples(trak.Mdia.Minf.Stbl)
	if err != nil {
		return nil, err
	}
	if mf.IsFragmented() {
		var next int64
		if n := len(samples); n > 0 {
			next = samples[n-1].dts + int64(samples[n-1].dur)
		}
		if samples, err = mp4FragmentSamples(mf, trak.Tkhd.TrackID, trexFor(mf.Moov, trak.Tkhd.TrackID), next, samples); err != nil {
			return nil, err
		}
	}
	if len(samples) == 0 {
		return nil, errors.New("no samples")
	}
	// The edit list: empty edits delay the presentation, the first media
	// edit's media time is where it starts.
	var empty, media int64
	if trak.Edts != nil {
	edits:
		for _, elst := range trak.Edts.Elst {
			for _, e := range elst.Entries {
				if e.MediaTime < 0 {
					if movieTS > 0 {
						empty += int64(e.SegmentDuration) * int64(ts) / int64(movieTS)
					}
					continue
				}
				media = e.MediaTime
				break edits
			}
		}
	}
	t := &mp4Track{timescale: ts, samples: samples}
	// presentation − origin = dts + base + cto − mediaTime, with base and
	// mediaTime ≥ 0 so no decode time goes below 0.
	t.base = empty - nsToTicks(originNs, ts)
	t.mediaTime = media
	if t.base < 0 {
		t.mediaTime -= t.base
		t.base = 0
	}
	return t, nil
}

// splitDV is a visual sample entry without its Dolby Vision boxes (the
// entry as it is when it has none), and its Dolby Vision record.
func splitDV(vse *mp4.VisualSampleEntryBox) (mp4.Box, *dvRecord) {
	var dv *dvRecord
	plain := *vse
	plain.Children = nil
	for _, c := range vse.Children {
		switch c.Type() {
		case "dvcC", "dvvC", "dvwC":
			var b bytes.Buffer
			if err := c.Encode(&b); err == nil && b.Len() > 8 {
				dv, _ = parseDV(c.Type(), b.Bytes()[8:])
			}
			continue
		}
		plain.Children = append(plain.Children, c)
	}
	if dv == nil {
		return vse, nil
	}
	return &plain, dv
}

// videoCodecs is the CODECS entry of a visual sample entry, from its
// configuration box.
func videoCodecs(vse *mp4.VisualSampleEntryBox) (string, error) {
	name := vse.Type()
	switch {
	case vse.AvcC != nil:
		if len(vse.AvcC.SPSnalus) == 0 {
			return "", errors.New("avcC holds no SPS")
		}
		sps, err := avc.ParseSPSNALUnit(vse.AvcC.SPSnalus[0], false)
		if err != nil {
			return "", fmt.Errorf("SPS: %w", err)
		}
		return avc.CodecString(name, sps), nil
	case vse.HvcC != nil:
		spss := vse.HvcC.GetNalusForType(hevc.NALU_SPS)
		if len(spss) == 0 {
			return "", errors.New("hvcC holds no SPS")
		}
		sps, err := hevc.ParseSPSNALUnit(spss[0])
		if err != nil {
			return "", fmt.Errorf("SPS: %w", err)
		}
		return hevc.CodecString(name, sps), nil
	case vse.Av1C != nil:
		return vse.Av1C.CodecConfRec.CodecString(name), nil
	case strings.HasPrefix(name, "vp09"):
		for _, c := range vse.Children {
			if v, ok := c.(*mp4.VppCBox); ok {
				return fmt.Sprintf("vp09.%02d.%02d.%02d", v.Profile, v.Level, v.BitDepth), nil
			}
		}
		return "", errors.New("vp09 without a vpcC")
	}
	return "", unsupported("video sample entry %s", name)
}

// trackInit is the header of a copied track of trak: its sample entry
// entry, on timescale, its edit list starting at mediaTime.
func trackInit(trak *mp4.TrakBox, entry mp4.Box, timescale uint32, mediaTime int64, kind, language string) ([]byte, error) {
	init, t := newInit(timescale, kind, language)
	t.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	if kind == "video" {
		t.Tkhd.Width, t.Tkhd.Height = trak.Tkhd.Width, trak.Tkhd.Height
	}
	setEdit(t, mediaTime)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mp4TableSamples are the samples of a progressive track's sample tables
// (none for a fragmented one's empty tables).
func mp4TableSamples(stbl *mp4.StblBox) ([]mp4Sample, error) {
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
	out := make([]mp4Sample, n)
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

// trexFor is the trex of trackID.
func trexFor(moov *mp4.MoovBox, trackID uint32) *mp4.TrexBox {
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

// mp4FragmentSamples appends the samples of trackID in a fragmented file's
// fragments, from the decode time next on where a fragment has no tfdt.
func mp4FragmentSamples(mf *mp4.File, trackID uint32, trex *mp4.TrexBox, next int64, out []mp4Sample) ([]mp4Sample, error) {
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
						out = append(out, mp4Sample{dts: dts, offset: pos, dur: s.Dur, size: s.Size, cto: s.CompositionTimeOffset,
							sync: s.Flags&mp4.NonSyncSampleFlags == 0})
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

// produceMP4 makes segment n of an MP4 original: the video samples from its
// keyframe to the next segment's, and each audio track's samples presented
// in the same span (each where its middle falls), read by byte range.
func (ix *Index) produceMP4(r io.ReaderAt, n int, audio []int) (*Output, error) {
	if ix.mp4 == nil {
		return nil, errors.New("an MP4 index without its sample tables")
	}
	spans := ix.Spans()
	sp := spans[n]
	v := ix.mp4.video
	v0, v1 := int(ix.Keyframes[sp.First].Pos), len(v.samples)
	if sp.Next < len(ix.Keyframes) {
		v1 = int(ix.Keyframes[sp.Next].Pos)
	}
	out := &Output{Audio: map[int][]byte{}}
	read, err := mp4Fragment(r, v, v.samples[v0:v1], uint32(n+1), &out.Video)
	if err != nil {
		return nil, err
	}
	out.Read += read
	// The span on the presentation timeline: from this segment's first
	// keyframe to the next one's (from the start, to the end).
	from, to := int64(-1<<62), int64(1<<62)
	ts := float64(ix.Video.Timescale)
	if n > 0 {
		from = ix.Keyframes[sp.First].PTS
	}
	if sp.Next < len(ix.Keyframes) {
		to = ix.Keyframes[sp.Next].PTS
	}
	for _, a := range audio {
		t := ix.mp4.audio[a]
		if t == nil {
			return nil, fmt.Errorf("audio track %d is not copied", a)
		}
		scale := float64(t.timescale) / ts
		// Twice the presentation of a sample's middle, against twice the
		// boundaries in the audio's ticks.
		bound := func(v int64) int64 {
			if v <= -1<<61 || v >= 1<<61 {
				return v
			}
			return int64(float64(2*v) * scale)
		}
		lo, hi := bound(from), bound(to)
		mid := func(i int) int64 { return 2*t.pts(i) + int64(t.samples[i].dur) }
		a0 := sort.Search(len(t.samples), func(i int) bool { return mid(i) >= lo })
		a1 := sort.Search(len(t.samples), func(i int) bool { return mid(i) >= hi })
		var b []byte
		read, err := mp4Fragment(r, t, t.samples[a0:a1], uint32(n+1), &b)
		if err != nil {
			return nil, err
		}
		out.Audio[a], out.Read = b, out.Read+read
	}
	return out, nil
}

// mp4Fragment writes samples of track t as fragment seq into *dst, their data
// read from r by byte range; it returns the bytes read.
func mp4Fragment(r io.ReaderAt, t *mp4Track, samples []mp4Sample, seq uint32, dst *[]byte) (int64, error) {
	data, read, err := readRanges(r, samples)
	if err != nil {
		return read, err
	}
	fs := make([]fragmentSample, len(samples))
	for i, s := range samples {
		dts := s.dts + t.base
		fs[i] = fragmentSample{dts: dts, pts: dts + int64(s.cto), dur: int64(s.dur), sync: s.sync, data: data[i]}
	}
	b, err := fragment(seq, fs)
	*dst = b
	return read, err
}

// readRanges reads the samples' data by byte range: one read per run of
// samples that follow each other in the file (at most 8 MiB).
func readRanges(r io.ReaderAt, samples []mp4Sample) ([][]byte, int64, error) {
	out := make([][]byte, len(samples))
	var read int64
	for i := 0; i < len(samples); {
		j, end := i+1, samples[i].offset+int64(samples[i].size)
		for j < len(samples) && samples[j].offset == end && end-samples[i].offset < 8<<20 {
			end += int64(samples[j].size)
			j++
		}
		buf := make([]byte, end-samples[i].offset)
		n, err := r.ReadAt(buf, samples[i].offset)
		read += int64(n)
		if err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
			return nil, read, fmt.Errorf("read %d bytes at %d: %w", len(buf), samples[i].offset, err)
		}
		pos := 0
		for k := i; k < j; k++ {
			out[k] = buf[pos : pos+int(samples[k].size)]
			pos += int(samples[k].size)
		}
		i = j
	}
	return out, read, nil
}
