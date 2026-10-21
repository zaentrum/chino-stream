package mkv

import (
	"errors"
	"fmt"
	"io"
)

// Track types (TrackType).
const (
	TrackVideo    = 1
	TrackAudio    = 2
	TrackSubtitle = 17
)

// File is what Open reads of a Matroska file: its header, its tracks and its
// index, never its clusters.
type File struct {
	Size    int64
	DocType string // "matroska" or "webm"
	// SegmentData is the absolute position of the Segment's payload, where the
	// positions of the SeekHead and the Cues count from; SegmentEnd the
	// position its payload ends at (the file's size when that is unknown).
	SegmentData int64
	SegmentEnd  int64
	// TimestampScale is how many nanoseconds a timestamp unit lasts (a
	// Cluster's, a Block's, a CuePoint's): 1,000,000 by default, so
	// timestamps are milliseconds.
	TimestampScale int64
	// Duration is the Info's duration in timestamp units, 0 when it has none.
	Duration   float64
	MuxingApp  string
	WritingApp string
	Tracks     []Track
	// Cues is the index; nil when the file has none Open could find. CuesPos
	// is its element's absolute position (-1: none).
	Cues    []CuePoint
	CuesPos int64
	// FirstCluster is the absolute position of the first Cluster, -1 when the
	// elements before it did not lead there.
	FirstCluster int64
}

// Track is a TrackEntry.
type Track struct {
	Number       uint64
	UID          uint64
	Type         int
	CodecID      string
	CodecPrivate []byte
	// DefaultDuration is a frame's duration in nanoseconds, 0 when the track
	// gives none. CodecDelay and SeekPreRoll are nanoseconds too.
	DefaultDuration uint64
	CodecDelay      uint64
	SeekPreRoll     uint64
	Name            string
	Language        string // ISO 639-2 (Language), "eng" by default
	LanguageBCP47   string
	Enabled         bool
	Default         bool
	Forced          bool
	Video           *Video
	Audio           *Audio
	// BlockAdditions are the track's BlockAdditionMappings: for a Dolby Vision
	// stream, its configuration record (type dvcC or dvvC).
	BlockAdditions []BlockAddition
	Encodings      []ContentEncoding
}

// Video is a video track's settings.
type Video struct {
	PixelWidth, PixelHeight     uint64
	DisplayWidth, DisplayHeight uint64 // 0: the pixel size
	DisplayUnit                 uint64 // 0 pixels, 1 cm, 2 inches, 3 aspect ratio, 4 unknown
	Interlaced                  uint64 // FlagInterlaced: 0 undetermined, 1 interlaced, 2 progressive
	// The Colour element's values, as ISO/IEC 23091-4 numbers them; 0 when
	// absent ("unspecified" is 2 there).
	MatrixCoefficients      uint64
	BitsPerChannel          uint64
	Range                   uint64
	TransferCharacteristics uint64
	Primaries               uint64
}

// Audio is an audio track's settings.
type Audio struct {
	SamplingFrequency       float64
	OutputSamplingFrequency float64 // 0: SamplingFrequency
	Channels                uint64
	BitDepth                uint64
}

// BlockAddition is a BlockAdditionMapping.
type BlockAddition struct {
	IDValue   uint64
	IDName    string
	IDType    uint32 // a four-character code: dvcC, dvvC, …
	ExtraData []byte
}

// ContentEncoding is how a track's frames are stored: compressed (Type 0:
// zlib, or header stripping, CompAlgo 3, with CompSettings the bytes taken
// off every frame) or encrypted (Type 1).
type ContentEncoding struct {
	Order        uint64
	Scope        uint64 // 1 frames, 2 the codec private data, 4 the next encoding's
	Type         uint64
	CompAlgo     uint64
	CompSettings []byte
}

// Content encoding kinds.
const (
	compZlib            = 0
	compHeaderStripping = 3
)

// CuePoint is an index entry: a time and where its blocks are.
type CuePoint struct {
	Time      uint64 // timestamp units
	Positions []CuePosition
}

// CuePosition is where a CuePoint's block of one track is: its Cluster's
// position, counted from the Segment's payload, and its position in that
// Cluster's payload (0: not given).
type CuePosition struct {
	Track       uint64
	Cluster     uint64
	Relative    uint64
	Duration    uint64
	BlockNumber uint64
}

// Track returns the track numbered n, nil when there is none.
func (f *File) Track(n uint64) *Track {
	for i := range f.Tracks {
		if f.Tracks[i].Number == n {
			return &f.Tracks[i]
		}
	}
	return nil
}

// headRead is how much of a file Open reads first: the EBML header, the
// Segment's and, in most files, the SeekHead, the Info and the Tracks.
const headRead = 64 << 10

// maxElement bounds an Info, Tracks or SeekHead element read whole, and
// maxCues the Cues: an element said to be larger is damage, not an index.
const (
	maxElement = 16 << 20
	maxCues    = 64 << 20
)

// Open reads the header, the tracks and the index of the Matroska file r of
// size bytes, from the head of the file and the elements its SeekHead
// points to: a few reads of a few kilobytes, wherever the index is.
func Open(r io.ReaderAt, size int64) (*File, error) {
	head := make([]byte, min(int64(headRead), size))
	if _, err := r.ReadAt(head, 0); err != nil && err != io.EOF {
		return nil, err
	}
	h, ok, err := readHeader(head)
	if err != nil || !ok || h.id != idEBML || h.size == unknownSize || int64(h.hlen)+h.size > int64(len(head)) {
		return nil, formatErr("no EBML header")
	}
	f := &File{Size: size, TimestampScale: 1_000_000, CuesPos: -1, FirstCluster: -1}
	if err := children(head[h.hlen:int64(h.hlen)+h.size], func(id uint32, data []byte) error {
		if id == idDocType {
			f.DocType = stringOf(data)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if f.DocType != "matroska" && f.DocType != "webm" {
		return nil, formatErr("doctype %q is not matroska", f.DocType)
	}
	pos := int64(h.hlen) + h.size
	seg, ok, err := readHeader(head[pos:])
	if err != nil || !ok || seg.id != idSegment {
		return nil, formatErr("no Segment after the EBML header")
	}
	f.SegmentData = pos + int64(seg.hlen)
	f.SegmentEnd = size
	if seg.size != unknownSize && f.SegmentData+seg.size < size {
		f.SegmentEnd = f.SegmentData + seg.size
	}
	o := &opener{r: r, f: f, head: head, read: map[int64]bool{}}
	if err := o.walk(); err != nil {
		return nil, err
	}
	if len(f.Tracks) == 0 {
		return nil, formatErr("no Tracks")
	}
	return f, nil
}

// opener reads a file's top-level elements for Open.
type opener struct {
	r    io.ReaderAt
	f    *File
	head []byte
	// read are the elements read already, by position; seeks the SeekHead
	// positions still to read.
	read  map[int64]bool
	seeks []int64
}

// walk reads the Segment's elements from its start to the first Cluster, then
// those the SeekHeads point to.
func (o *opener) walk() error {
	f := o.f
	pos := f.SegmentData
	for pos < f.SegmentEnd {
		h, err := o.header(pos)
		if err != nil {
			return err
		}
		if h.id == idCluster {
			f.FirstCluster = pos
			break
		}
		if h.size == unknownSize {
			break // a live-written element before the clusters: nothing more to find here
		}
		if err := o.element(pos, h); err != nil {
			return err
		}
		pos += int64(h.hlen) + h.size
	}
	for len(o.seeks) > 0 {
		at := o.seeks[0]
		o.seeks = o.seeks[1:]
		if o.read[at] || at < f.SegmentData || at >= f.SegmentEnd {
			continue
		}
		h, err := o.header(at)
		if err != nil {
			if errors.Is(err, ErrFormat) {
				continue // a SeekHead pointing at nothing: an index lost, not a broken file
			}
			return err // the file could not be read: not its format
		}
		if err := o.element(at, h); err != nil {
			if h.id == idCues && errors.Is(err, ErrFormat) {
				// A damaged index: the file has none to go by.
				f.Cues, f.CuesPos = nil, -1
				continue
			}
			return err
		}
	}
	return nil
}

// header reads the header of the element at the absolute position pos.
func (o *opener) header(pos int64) (header, error) {
	if pos < int64(len(o.head)) {
		if h, ok, err := readHeader(o.head[pos:]); err != nil || ok {
			return h, err
		}
	}
	if pos < 0 || pos >= o.f.Size {
		return header{}, formatErr("element at %d past the end", pos)
	}
	b := make([]byte, min(16, o.f.Size-pos))
	if _, err := o.r.ReadAt(b, pos); err != nil && err != io.EOF {
		return header{}, err
	}
	h, ok, err := readHeader(b)
	if err != nil {
		return header{}, err
	}
	if !ok {
		return header{}, formatErr("truncated element at %d", pos)
	}
	return h, nil
}

// payload is the payload of the known-size element at pos.
func (o *opener) payload(pos int64, h header, limit int64) ([]byte, error) {
	if h.size < 0 {
		return nil, formatErr("element 0x%x at %d of unknown size", h.id, pos)
	}
	if h.size > limit {
		return nil, formatErr("element 0x%x at %d of %d bytes", h.id, pos, h.size)
	}
	start := pos + int64(h.hlen)
	end := start + h.size
	if end > o.f.Size {
		return nil, formatErr("element 0x%x at %d runs past the end of the file", h.id, pos)
	}
	if end <= int64(len(o.head)) {
		return o.head[start:end], nil
	}
	b := make([]byte, h.size)
	if _, err := o.r.ReadAt(b, start); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// element reads the top-level element at pos if it is one Open uses.
func (o *opener) element(pos int64, h header) error {
	if o.read[pos] {
		return nil
	}
	o.read[pos] = true
	f := o.f
	switch h.id {
	case idSeekHead:
		b, err := o.payload(pos, h, maxElement)
		if err != nil {
			return err
		}
		return o.seekHead(b)
	case idInfo:
		b, err := o.payload(pos, h, maxElement)
		if err != nil {
			return err
		}
		return f.info(b)
	case idTracks:
		if f.Tracks != nil {
			return nil
		}
		b, err := o.payload(pos, h, maxElement)
		if err != nil {
			return err
		}
		return f.tracks(b)
	case idCues:
		if f.Cues != nil {
			return nil
		}
		if h.size == unknownSize {
			return formatErr("Cues of unknown size")
		}
		b, err := o.payload(pos, h, maxCues)
		if err != nil {
			return err
		}
		cues, err := parseCues(b)
		if err != nil {
			return err
		}
		f.Cues, f.CuesPos = cues, pos
	}
	return nil
}

// seekHead notes the positions a SeekHead points to: the Info, Tracks, Cues
// and further SeekHeads.
func (o *opener) seekHead(b []byte) error {
	return children(b, func(id uint32, data []byte) error {
		if id != idSeek {
			return nil
		}
		var target uint32
		var at uint64
		var has bool
		if err := children(data, func(id uint32, data []byte) error {
			switch id {
			case idSeekID:
				t, err := uintOf(data)
				target = uint32(t)
				return err
			case idSeekPos:
				v, err := uintOf(data)
				at, has = v, true
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		switch target {
		case idSeekHead, idInfo, idTracks, idCues:
			if has {
				o.seeks = append(o.seeks, o.f.SegmentData+int64(at))
			}
		}
		return nil
	})
}

func (f *File) info(b []byte) error {
	return children(b, func(id uint32, data []byte) error {
		var err error
		switch id {
		case idTSScale:
			var v uint64
			v, err = uintOf(data)
			if err == nil && v > 0 {
				f.TimestampScale = int64(v)
			}
		case idDuration:
			f.Duration, err = floatOf(data)
		case idMuxingApp:
			f.MuxingApp = stringOf(data)
		case idWritingApp:
			f.WritingApp = stringOf(data)
		}
		return err
	})
}

func (f *File) tracks(b []byte) error {
	f.Tracks = []Track{}
	return children(b, func(id uint32, data []byte) error {
		if id != idTrackEntry {
			return nil
		}
		t := Track{Language: "eng", Enabled: true, Default: true}
		if err := children(data, t.entry); err != nil {
			return fmt.Errorf("track: %w", err)
		}
		f.Tracks = append(f.Tracks, t)
		return nil
	})
}

func (t *Track) entry(id uint32, data []byte) error {
	var err error
	u := func(dst *uint64) {
		*dst, err = uintOf(data)
	}
	flag := func(dst *bool) {
		var v uint64
		v, err = uintOf(data)
		*dst = v != 0
	}
	switch id {
	case idTrackNumber:
		u(&t.Number)
	case idTrackUID:
		u(&t.UID)
	case idTrackType:
		var v uint64
		u(&v)
		t.Type = int(v)
	case idFlagEnabled:
		flag(&t.Enabled)
	case idFlagDefault:
		flag(&t.Default)
	case idFlagForced:
		flag(&t.Forced)
	case idDefaultDuration:
		u(&t.DefaultDuration)
	case idCodecDelay:
		u(&t.CodecDelay)
	case idSeekPreRoll:
		u(&t.SeekPreRoll)
	case idName:
		t.Name = stringOf(data)
	case idLanguage:
		t.Language = stringOf(data)
	case idLanguageBCP47:
		t.LanguageBCP47 = stringOf(data)
	case idCodecID:
		t.CodecID = stringOf(data)
	case idCodecPrivate:
		t.CodecPrivate = append([]byte(nil), data...)
	case idVideo:
		t.Video = &Video{}
		err = children(data, t.Video.element)
	case idAudio:
		t.Audio = &Audio{SamplingFrequency: 8000, Channels: 1}
		err = children(data, t.Audio.element)
	case idBlockAddMapping:
		var m BlockAddition
		err = children(data, m.element)
		t.BlockAdditions = append(t.BlockAdditions, m)
	case idContentEncs:
		err = children(data, func(id uint32, data []byte) error {
			if id != idContentEnc {
				return nil
			}
			e := ContentEncoding{Scope: 1}
			if err := children(data, e.element); err != nil {
				return err
			}
			t.Encodings = append(t.Encodings, e)
			return nil
		})
	}
	return err
}

func (v *Video) element(id uint32, data []byte) error {
	var err error
	u := func(dst *uint64) { *dst, err = uintOf(data) }
	switch id {
	case idPixelWidth:
		u(&v.PixelWidth)
	case idPixelHeight:
		u(&v.PixelHeight)
	case idDisplayWidth:
		u(&v.DisplayWidth)
	case idDisplayHeight:
		u(&v.DisplayHeight)
	case idDisplayUnit:
		u(&v.DisplayUnit)
	case idFlagInterlace:
		u(&v.Interlaced)
	case idColour:
		err = children(data, func(id uint32, data []byte) error {
			var err error
			u := func(dst *uint64) { *dst, err = uintOf(data) }
			switch id {
			case idMatrix:
				u(&v.MatrixCoefficients)
			case idBitsPerChan:
				u(&v.BitsPerChannel)
			case idRange:
				u(&v.Range)
			case idTransfer:
				u(&v.TransferCharacteristics)
			case idPrimaries:
				u(&v.Primaries)
			}
			return err
		})
	}
	return err
}

func (a *Audio) element(id uint32, data []byte) error {
	var err error
	switch id {
	case idSamplingFreq:
		a.SamplingFrequency, err = floatOf(data)
	case idOutSamplingFreq:
		a.OutputSamplingFrequency, err = floatOf(data)
	case idChannels:
		a.Channels, err = uintOf(data)
	case idBitDepth:
		a.BitDepth, err = uintOf(data)
	}
	return err
}

func (m *BlockAddition) element(id uint32, data []byte) error {
	var err error
	switch id {
	case idBlockAddIDValue:
		m.IDValue, err = uintOf(data)
	case idBlockAddIDName:
		m.IDName = stringOf(data)
	case idBlockAddIDType:
		var v uint64
		v, err = uintOf(data)
		m.IDType = uint32(v)
	case idBlockAddIDExtra:
		m.ExtraData = append([]byte(nil), data...)
	}
	return err
}

func (e *ContentEncoding) element(id uint32, data []byte) error {
	var err error
	switch id {
	case idContentEncOrder:
		e.Order, err = uintOf(data)
	case idContentEncScope:
		e.Scope, err = uintOf(data)
	case idContentEncType:
		e.Type, err = uintOf(data)
	case idContentComp:
		err = children(data, func(id uint32, data []byte) error {
			var err error
			switch id {
			case idContentCompAlgo:
				e.CompAlgo, err = uintOf(data)
			case idContentCompSet:
				e.CompSettings = append([]byte(nil), data...)
			}
			return err
		})
	case idContentEncrypt:
		e.Type = 1
	}
	return err
}

// parseCues reads a Cues payload.
func parseCues(b []byte) ([]CuePoint, error) {
	cues := []CuePoint{}
	err := children(b, func(id uint32, data []byte) error {
		if id != idCuePoint {
			return nil
		}
		var p CuePoint
		var hasTime bool
		if err := children(data, func(id uint32, data []byte) error {
			switch id {
			case idCueTime:
				v, err := uintOf(data)
				p.Time, hasTime = v, true
				return err
			case idCueTrackPositions:
				var c CuePosition
				var hasCluster bool
				if err := children(data, func(id uint32, data []byte) error {
					var err error
					switch id {
					case idCueTrack:
						c.Track, err = uintOf(data)
					case idCueClusterPos:
						c.Cluster, err = uintOf(data)
						hasCluster = true
					case idCueRelativePos:
						c.Relative, err = uintOf(data)
					case idCueDuration:
						c.Duration, err = uintOf(data)
					case idCueBlockNumber:
						c.BlockNumber, err = uintOf(data)
					}
					return err
				}); err != nil {
					return err
				}
				if hasCluster && c.Track > 0 {
					p.Positions = append(p.Positions, c)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if hasTime && len(p.Positions) > 0 {
			cues = append(cues, p)
		}
		return nil
	})
	return cues, err
}
