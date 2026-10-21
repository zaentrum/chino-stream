package streamcopy

import (
	"bytes"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The CMAF headers of the copy: one track each, as a package's renditions
// have them. A video track's configuration box is the original's, byte for
// byte; its edit list takes its reordering delay off, as an encoder's or a
// packager's does (one edit of media time Shift and duration 0, the form a
// fragmented file's players apply).

func copyFtyp() *mp4.FtypBox {
	return mp4.NewFtyp("iso6", 0, []string{"iso6", "cmfc", "isom", "mp41"})
}

// setEdit gives a track an edit list of one edit starting the presentation
// at mediaTime (none for 0), after its tkhd.
func setEdit(trak *mp4.TrakBox, mediaTime int64) {
	if mediaTime <= 0 {
		return
	}
	elst := &mp4.ElstBox{Entries: []mp4.ElstEntry{{SegmentDuration: 0, MediaTime: mediaTime, MediaRateInteger: 1}}}
	edts := &mp4.EdtsBox{Elst: []*mp4.ElstBox{elst}, Children: []mp4.Box{elst}}
	trak.Edts = edts
	children := make([]mp4.Box, 0, len(trak.Children)+1)
	for _, c := range trak.Children {
		children = append(children, c)
		if c.Type() == "tkhd" {
			children = append(children, edts)
		}
	}
	trak.Children = children
}

// newInit is a header of one empty track (ID 1) of kind ("video", "audio")
// on timescale, its boxes in the usual order: ftyp, then moov with mvhd, the
// trak and mvex.
func newInit(timescale uint32, kind, language string) (*mp4.InitSegment, *mp4.TrakBox) {
	init := mp4.NewMP4Init()
	init.AddChild(copyFtyp())
	moov := mp4.NewMoovBox()
	init.AddChild(moov)
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = timescale
	mvhd.NextTrackID = 2
	moov.AddChild(mvhd)
	if len(language) != 3 {
		language = "und"
	}
	trak := mp4.CreateEmptyTrak(1, timescale, kind, language)
	moov.AddChild(trak)
	mvex := mp4.NewMvexBox()
	mvex.AddChild(mp4.CreateTrex(1))
	moov.AddChild(mvex)
	return init, trak
}

// rawBox is a box of type typ with payload as it is.
func rawBox(typ string, payload []byte) mp4.Box {
	return mp4.CreateUnknownBox(typ, uint64(8+len(payload)), payload)
}

// videoInit is the header of a video track: the sample entry entry (avc1,
// hvc1, dvh1, av01) of a width×height frame with the configuration box (and
// the Dolby Vision record dv, nil for none), on timescale, its edit list
// starting at shift, displayed at dispW×dispH.
func videoInit(entry string, cfg *videoConfig, dv *dvRecord, timescale uint32, shift int64, dispW, dispH int, language string) ([]byte, error) {
	init, trak := newInit(timescale, "video", language)
	vse := mp4.CreateVisualSampleEntryBox(entry, uint16(cfg.width), uint16(cfg.height), nil)
	vse.AddChild(rawBox(cfg.boxType, cfg.box))
	if dv != nil {
		vse.AddChild(rawBox(dv.boxType, dv.data))
	}
	if dispW > 0 && dispH > 0 && (dispW != cfg.width || dispH != cfg.height) {
		// Non-square pixels: their aspect ratio, so the frame shows at the
		// display size.
		h, v := dispW*cfg.height, dispH*cfg.width
		g := gcd(h, v)
		vse.AddChild(&mp4.PaspBox{HSpacing: uint32(h / g), VSpacing: uint32(v / g)})
	} else {
		dispW, dispH = cfg.width, cfg.height
	}
	trak.Mdia.Minf.Stbl.Stsd.AddChild(vse)
	trak.Tkhd.Width = mp4.Fixed32(uint32(dispW) << 16)
	trak.Tkhd.Height = mp4.Fixed32(uint32(dispH) << 16)
	setEdit(trak, shift)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// audioInit is the header of an audio track with the sample entry of cfg,
// its edit list starting at mediaTime.
func audioInit(cfg *audioConfig, mediaTime int64, language string) ([]byte, error) {
	init, trak := newInit(uint32(cfg.sampleRate), "audio", language)
	trak.Mdia.Minf.Stbl.Stsd.AddChild(cfg.entry)
	setEdit(trak, mediaTime)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// fragmentSample is one sample of a fragment on its track's timeline.
type fragmentSample struct {
	dts, pts int64
	dur      int64
	sync     bool
	data     []byte
}

// fragment writes samples (in decode order) as fragment seq of a one-track
// file: a styp, a moof (trun version 0 when no composition offset is
// negative, else 1) and an mdat.
func fragment(seq uint32, samples []fragmentSample) ([]byte, error) {
	frag, err := mp4.CreateFragment(seq, 1)
	if err != nil {
		return nil, err
	}
	negative := false
	for _, s := range samples {
		flags := mp4.SyncSampleFlags
		if !s.sync {
			flags = mp4.SetNonSyncSampleFlags(0)
		}
		cto := s.pts - s.dts
		if cto < 0 {
			negative = true
		}
		frag.AddFullSample(mp4.FullSample{Sample: mp4.Sample{Flags: flags, Dur: uint32(s.dur), Size: uint32(len(s.data)),
			CompositionTimeOffset: int32(cto)}, DecodeTime: uint64(s.dts), Data: s.data})
	}
	if !negative && frag.Moof.Traf != nil {
		for _, trun := range frag.Moof.Traf.Truns {
			trun.Version = 0
		}
	}
	var buf bytes.Buffer
	if err := mp4.NewStyp("msdh", 0, []string{"msdh", "msix", "cmfs"}).Encode(&buf); err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		// A track that holds nothing in this segment: a moof with its track
		// fragment's decode time and no run, and an empty mdat.
		return emptyFragment(&buf, seq)
	}
	if err := frag.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// emptyFragment appends to buf a fragment without samples.
func emptyFragment(buf *bytes.Buffer, seq uint32) ([]byte, error) {
	moof := &mp4.MoofBox{}
	moof.AddChild(&mp4.MfhdBox{SequenceNumber: seq})
	traf := &mp4.TrafBox{}
	moof.AddChild(traf)
	traf.AddChild(&mp4.TfhdBox{Flags: 0x020000, TrackID: 1})
	if err := moof.Encode(buf); err != nil {
		return nil, err
	}
	mdat := &mp4.MdatBox{}
	if err := mdat.Encode(buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
