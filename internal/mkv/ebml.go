// Package mkv reads Matroska (and WebM) files for the stream copy: the
// header, the tracks and the index (Cues) of a file in a few small reads, and
// its blocks from a byte range, so a segment of the original is read by byte
// range rather than by a demuxer that runs through the whole file. It reads;
// it never writes, and it starts no process.
//
// The format is EBML (RFC 8794) with the Matroska schema (RFC 9559): every
// element is an ID, a size and a payload, the size a variable-length integer
// (VINT) that may say "unknown" for a master element written live.
package mkv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Element IDs of the Matroska schema this package reads (RFC 9559). The IDs
// keep their length marker bits, as the specification writes them.
const (
	idEBML        = 0x1A45DFA3
	idDocType     = 0x4282
	idSegment     = 0x18538067
	idSeekHead    = 0x114D9B74
	idSeek        = 0x4DBB
	idSeekID      = 0x53AB
	idSeekPos     = 0x53AC
	idInfo        = 0x1549A966
	idTSScale     = 0x2AD7B1
	idDuration    = 0x4489
	idMuxingApp   = 0x4D80
	idWritingApp  = 0x5741
	idTracks      = 0x1654AE6B
	idTrackEntry  = 0xAE
	idCluster     = 0x1F43B675
	idCues        = 0x1C53BB6B
	idChapters    = 0x1043A770
	idTags        = 0x1254C367
	idAttachments = 0x1941A469
	idVoid        = 0xEC
	idCRC32       = 0xBF

	// TrackEntry.
	idTrackNumber     = 0xD7
	idTrackUID        = 0x73C5
	idTrackType       = 0x83
	idFlagEnabled     = 0xB9
	idFlagDefault     = 0x88
	idFlagForced      = 0x55AA
	idDefaultDuration = 0x23E383
	idTrackTSScale    = 0x23314F
	idName            = 0x536E
	idLanguage        = 0x22B59C
	idLanguageBCP47   = 0x22B59D
	idCodecID         = 0x86
	idCodecPrivate    = 0x63A2
	idCodecDelay      = 0x56AA
	idSeekPreRoll     = 0x56BB
	idVideo           = 0xE0
	idAudio           = 0xE1
	idContentEncs     = 0x6D80
	idBlockAddMapping = 0x41E4

	// Video.
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
	idDisplayWidth  = 0x54B0
	idDisplayHeight = 0x54BA
	idDisplayUnit   = 0x54B2
	idFlagInterlace = 0x9A
	idColour        = 0x55B0
	idMatrix        = 0x55B1
	idBitsPerChan   = 0x55B2
	idRange         = 0x55B9
	idTransfer      = 0x55BA
	idPrimaries     = 0x55BB

	// Audio.
	idSamplingFreq    = 0xB5
	idOutSamplingFreq = 0x78B5
	idChannels        = 0x9F
	idBitDepth        = 0x6264

	// BlockAdditionMapping.
	idBlockAddIDValue = 0x41F0
	idBlockAddIDName  = 0x41A4
	idBlockAddIDType  = 0x41E7
	idBlockAddIDExtra = 0x41ED

	// ContentEncodings.
	idContentEnc      = 0x6240
	idContentEncOrder = 0x5031
	idContentEncScope = 0x5032
	idContentEncType  = 0x5033
	idContentComp     = 0x5034
	idContentCompAlgo = 0x4254
	idContentCompSet  = 0x4255
	idContentEncrypt  = 0x5035

	// Cues.
	idCuePoint          = 0xBB
	idCueTime           = 0xB3
	idCueTrackPositions = 0xB7
	idCueTrack          = 0xF7
	idCueClusterPos     = 0xF1
	idCueRelativePos    = 0xF0
	idCueDuration       = 0xB2
	idCueBlockNumber    = 0x5378

	// Cluster.
	idTimestamp      = 0xE7
	idPosition       = 0xA7
	idPrevSize       = 0xAB
	idSimpleBlock    = 0xA3
	idBlockGroup     = 0xA0
	idBlock          = 0xA1
	idBlockDuration  = 0x9B
	idReferenceBlock = 0xFB
	idBlockAdditions = 0x75A1
	idDiscardPadding = 0x75A2
	idEncryptedBlock = 0xAF
	idSilentTracks   = 0x5854
)

// unknownSize is the size of an element whose size its VINT says is
// unknown: it ends where an element that cannot be its child starts.
const unknownSize = -1

// ErrFormat wraps every error about the bytes: a file that is not Matroska,
// or one that is damaged or truncated where it is read.
var ErrFormat = errors.New("mkv")

func formatErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, a...))
}

// readID reads an element ID (1 to 4 bytes, the length marker kept) at the
// start of b; n is its length, 0 when b is too short.
func readID(b []byte) (id uint32, n int, err error) {
	if len(b) == 0 {
		return 0, 0, nil
	}
	first := b[0]
	switch {
	case first&0x80 != 0:
		n = 1
	case first&0x40 != 0:
		n = 2
	case first&0x20 != 0:
		n = 3
	case first&0x10 != 0:
		n = 4
	default:
		return 0, 0, formatErr("element ID starting 0x%02x is longer than 4 bytes", first)
	}
	if len(b) < n {
		return 0, 0, nil
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n, nil
}

// readVint reads a VINT at the start of b: its value with the length marker
// taken off, its length (1 to 8 bytes), and whether every value bit is set —
// for a size, "unknown". n is 0 when b is too short.
func readVint(b []byte) (v uint64, n int, allOnes bool, err error) {
	if len(b) == 0 {
		return 0, 0, false, nil
	}
	first := b[0]
	if first == 0 {
		return 0, 0, false, formatErr("VINT longer than 8 bytes")
	}
	n = 1
	for mask := byte(0x80); first&mask == 0; mask >>= 1 {
		n++
	}
	if len(b) < n {
		return 0, 0, false, nil
	}
	v = uint64(first & (0xFF >> n))
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, v == 1<<(7*n)-1, nil
}

// readSize reads an element size: a VINT, unknownSize when it says unknown.
func readSize(b []byte) (size int64, n int, err error) {
	v, n, allOnes, err := readVint(b)
	if err != nil || n == 0 {
		return 0, n, err
	}
	if allOnes {
		return unknownSize, n, nil
	}
	if v > math.MaxInt64/2 {
		return 0, 0, formatErr("element size %d out of range", v)
	}
	return int64(v), n, nil
}

// header is an element's header: its ID, the length of the ID and size, and
// the size of its payload (unknownSize: unknown).
type header struct {
	id   uint32
	hlen int
	size int64
}

// readHeader reads the header of the element at the start of b; ok false
// when b does not hold all of it.
func readHeader(b []byte) (h header, ok bool, err error) {
	id, n, err := readID(b)
	if err != nil || n == 0 {
		return header{}, false, err
	}
	size, m, err := readSize(b[n:])
	if err != nil || m == 0 {
		return header{}, false, err
	}
	return header{id: id, hlen: n + m, size: size}, true, nil
}

// children calls fn with each child of a master element's payload, which
// must hold its children whole (a known-size element read into memory).
func children(payload []byte, fn func(id uint32, data []byte) error) error {
	for len(payload) > 0 {
		h, ok, err := readHeader(payload)
		if err != nil {
			return err
		}
		if !ok || h.size == unknownSize || int64(len(payload)-h.hlen) < h.size {
			return formatErr("element 0x%x overruns its parent", h.id)
		}
		data := payload[h.hlen : int64(h.hlen)+h.size]
		if err := fn(h.id, data); err != nil {
			return err
		}
		payload = payload[int64(h.hlen)+h.size:]
	}
	return nil
}

// uintOf is an unsigned integer element's value (0 to 8 bytes, big-endian).
func uintOf(b []byte) (uint64, error) {
	if len(b) > 8 {
		return 0, formatErr("unsigned integer of %d bytes", len(b))
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// intOf is a signed integer element's value (0 to 8 bytes, two's complement).
func intOf(b []byte) (int64, error) {
	if len(b) > 8 {
		return 0, formatErr("signed integer of %d bytes", len(b))
	}
	if len(b) == 0 {
		return 0, nil
	}
	v := int64(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int64(c)
	}
	return v, nil
}

// floatOf is a float element's value (0, 4 or 8 bytes).
func floatOf(b []byte) (float64, error) {
	switch len(b) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	}
	return 0, formatErr("float of %d bytes", len(b))
}

// stringOf is a string element's value, without the zero padding a muxer may
// leave after it.
func stringOf(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// topLevel says whether id is one of a Segment's own children: the elements
// that end a Cluster of unknown size.
func topLevel(id uint32) bool {
	switch id {
	case idSeekHead, idInfo, idTracks, idCluster, idCues, idChapters, idTags, idAttachments:
		return true
	}
	return false
}
