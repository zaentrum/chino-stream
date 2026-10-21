package streamcopy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// What a codec configuration says about a video stream, as far as the copy
// needs it: the sample entry and CODECS string it is served with, its
// configuration box as the original has it, and how far its frames are
// reordered.

// videoConfig is a video track's configuration.
type videoConfig struct {
	sampleEntry string // avc1, hvc1, av01
	codecs      string // its CODECS entry (RFC 6381)
	boxType     string // avcC, hvcC, av1C
	box         []byte // the box's payload, the original's bytes
	width       int    // the coded frame
	height      int
	bitDepth    int
	chroma      int // chroma_format_idc: 1 is 4:2:0
	// reorder is how many frames the stream reorders at most (sps_max_num_
	// reorder_pics, max_num_reorder_frames); -1 when its SPS does not say.
	reorder int
	// lengthSize is the size of the length before each NAL unit of a sample
	// (H.264, HEVC).
	lengthSize int
}

// errUnsupported is a stream the copy does not take as it is.
var errUnsupported = errors.New("not copied")

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUnsupported, fmt.Sprintf(format, a...))
}

// hevcConfig reads an HEVCDecoderConfigurationRecord: what every HEVC
// decoder plays is Main or Main 10, 4:2:0, at most 10 bits.
func hevcConfig(rec []byte) (*videoConfig, error) {
	dcr, err := hevc.DecodeHEVCDecConfRec(rec)
	if err != nil {
		return nil, fmt.Errorf("hvcC: %w", err)
	}
	spss := dcr.GetNalusForType(hevc.NALU_SPS)
	if len(spss) == 0 {
		return nil, errors.New("hvcC holds no SPS")
	}
	sps, err := hevc.ParseSPSNALUnit(spss[0])
	if err != nil {
		return nil, fmt.Errorf("SPS: %w", err)
	}
	c := &videoConfig{sampleEntry: "hvc1", boxType: "hvcC", box: rec, chroma: int(sps.ChromaFormatIDC),
		bitDepth: int(sps.BitDepthLumaMinus8) + 8, reorder: -1, lengthSize: int(dcr.LengthSizeMinusOne) + 1}
	w, h := sps.ImageSize()
	c.width, c.height = int(w), int(h)
	c.codecs = hevc.CodecString("hvc1", sps)
	if n := len(sps.SubLayeringOrderingInfos); n > 0 {
		c.reorder = int(sps.SubLayeringOrderingInfos[n-1].MaxNumReorderPics)
	}
	ptl := sps.ProfileTierLevel
	main := ptl.GeneralProfileIDC == 1 || ptl.GeneralProfileIDC == 2 ||
		ptl.GeneralProfileCompatibilityFlags&(1<<(31-1)) != 0 || ptl.GeneralProfileCompatibilityFlags&(1<<(31-2)) != 0
	switch {
	case !main:
		return c, unsupported("HEVC profile %d", ptl.GeneralProfileIDC)
	case c.chroma != 1:
		return c, unsupported("HEVC chroma format %d", c.chroma)
	case c.bitDepth > 10:
		return c, unsupported("HEVC at %d bits", c.bitDepth)
	case c.lengthSize != 1 && c.lengthSize != 2 && c.lengthSize != 4:
		return c, unsupported("NAL length size %d", c.lengthSize)
	}
	return c, nil
}

// avcConfig reads an AVCDecoderConfigurationRecord: what every H.264
// decoder plays is Baseline, Main or High, 4:2:0 at 8 bits.
func avcConfig(rec []byte) (*videoConfig, error) {
	if len(rec) < 7 || rec[0] != 1 {
		return nil, errors.New("avcC: not a version 1 record")
	}
	c := &videoConfig{sampleEntry: "avc1", boxType: "avcC", box: rec, reorder: -1, lengthSize: int(rec[4]&3) + 1}
	n := int(rec[5] & 0x1F)
	b := rec[6:]
	var sps []byte
	for i := 0; i < n; i++ {
		if len(b) < 2 {
			return nil, errors.New("avcC: SPS cut short")
		}
		l := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+l {
			return nil, errors.New("avcC: SPS cut short")
		}
		if sps == nil {
			sps = b[2 : 2+l]
		}
		b = b[2+l:]
	}
	if sps == nil {
		return nil, errors.New("avcC holds no SPS")
	}
	s, err := avc.ParseSPSNALUnit(sps, true)
	if err != nil {
		return nil, fmt.Errorf("SPS: %w", err)
	}
	c.width, c.height = int(s.Width), int(s.Height)
	c.chroma, c.bitDepth = int(s.ChromaFormatIDC), int(s.BitDepthLumaMinus8)+8
	c.codecs = avc.CodecString("avc1", s)
	switch {
	case s.VUI != nil && s.VUI.BitstreamRestrictionFlag:
		c.reorder = int(s.VUI.MaxNumReorderFrames)
	case s.Profile == 66:
		c.reorder = 0 // Baseline has no B-frames
	}
	switch {
	case s.Profile != 66 && s.Profile != 77 && s.Profile != 100:
		return c, unsupported("H.264 profile %d", s.Profile)
	case c.chroma != 1 || c.bitDepth != 8:
		return c, unsupported("H.264 chroma format %d at %d bits", c.chroma, c.bitDepth)
	case c.lengthSize != 1 && c.lengthSize != 2 && c.lengthSize != 4:
		return c, unsupported("NAL length size %d", c.lengthSize)
	}
	return c, nil
}

// av1Config reads an AV1CodecConfigurationRecord: Main profile (4:2:0, 8 or
// 10 bits) is what every AV1 decoder plays. AV1 frames are not reordered in
// the container: a temporal unit's timestamp is its presentation.
func av1Config(rec []byte, width, height int) (*videoConfig, error) {
	r, err := av1.DecodeAV1CodecConfRec(rec)
	if err != nil {
		return nil, fmt.Errorf("av1C: %w", err)
	}
	c := &videoConfig{sampleEntry: "av01", boxType: "av1C", box: rec, width: width, height: height, reorder: 0, chroma: 1,
		bitDepth: 8}
	if r.HighBitdepth != 0 {
		c.bitDepth = 10
	}
	c.codecs = r.CodecString("av01")
	if r.SeqProfile != 0 || r.TwelveBit != 0 {
		return c, unsupported("AV1 profile %d", r.SeqProfile)
	}
	return c, nil
}

// Dolby Vision: a configuration record (ETSI GS CCM 001, the dvcC or dvvC
// box's payload) names the profile and the base layer's compatibility.

// dvRecord is a Dolby Vision configuration record.
type dvRecord struct {
	boxType       string // dvcC (profiles up to 7), dvvC (8 and up)
	data          []byte
	profile       int
	level         int
	compatibility int // dv_bl_signal_compatibility_id: 0 none, 1 HDR10, 2 SDR, 4 HLG, 6 HDR10 (Blu-ray)
}

// parseDV reads a Dolby Vision configuration record of the box type t.
func parseDV(t string, b []byte) (*dvRecord, error) {
	if len(b) < 5 {
		return nil, errors.New("Dolby Vision record cut short")
	}
	return &dvRecord{boxType: t, data: b, profile: int(b[2] >> 1), level: int(b[2]&1)<<5 | int(b[3]>>3),
		compatibility: int(b[4] >> 4)}, nil
}

// codecs is the CODECS entry of a Dolby Vision stream of its own (dvh1.PP.LL).
func (d *dvRecord) codecs() string { return fmt.Sprintf("dvh1.%02d.%02d", d.profile, d.level) }

// Audio.

// audioConfig is an audio track's sample entry and what its frames last.
type audioConfig struct {
	codecs     string // ec-3, ac-3, mp4a.40.2
	sampleRate int    // the track's timescale
	channels   int
	// frameSamples is how many samples every frame holds, 0 when frames vary.
	frameSamples int
	entry        mp4.Box // the sample entry
}

// aacConfig is the configuration of an AAC track from its
// AudioSpecificConfig.
func aacConfig(asc []byte, channels int) (*audioConfig, error) {
	cfg, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(asc))
	if err != nil {
		return nil, fmt.Errorf("AudioSpecificConfig: %w", err)
	}
	if cfg.SamplingFrequency <= 0 || cfg.SamplingFrequency > 0xFFFF {
		return nil, unsupported("AAC at %d Hz", cfg.SamplingFrequency)
	}
	if n := aac.NrChannels(cfg.ChannelConfiguration); n > 0 {
		channels = n
	}
	frame := 1024
	// GASpecificConfig's frameLengthFlag: 960-sample frames.
	if off := ascGAOffset(asc, cfg); off >= 0 && off/8 < len(asc) && asc[off/8]&(0x80>>(off%8)) != 0 {
		frame = 960
	}
	c := &audioConfig{codecs: fmt.Sprintf("mp4a.40.%d", cfg.ObjectType), sampleRate: cfg.SamplingFrequency, channels: channels, frameSamples: frame}
	c.entry = mp4.CreateAudioSampleEntryBox("mp4a", uint16(channels), 16, uint16(cfg.SamplingFrequency), mp4.CreateEsdsBox(asc))
	return c, nil
}

// ascGAOffset is the bit position of the GASpecificConfig in an
// AudioSpecificConfig (-1 when unknown): after the object type, the
// frequency index (24 bits more with an explicit frequency), the channel
// configuration, and for HE-AAC the extension frequency and object type.
func ascGAOffset(asc []byte, cfg *aac.AudioSpecificConfig) int {
	if len(asc) < 2 {
		return -1
	}
	pos := 5
	freqIdx := int(asc[0]&0x07)<<1 | int(asc[1]>>7)
	pos += 4
	if freqIdx == 15 {
		pos += 24
	}
	pos += 4 // channel configuration
	if cfg.ObjectType == aac.HEAACv1 || cfg.ObjectType == aac.HEAACv2 {
		ext := 4
		// The extension frequency index sits at pos.
		if pos/8+1 < len(asc) {
			v := (int(asc[pos/8])<<8 | int(asc[pos/8+1])) >> (12 - pos%8) & 0xF
			if v == 15 {
				ext += 24
			}
		}
		pos += ext + 5
	}
	return pos
}

// bitReader reads bits MSB first.
type bitReader struct {
	b   []byte
	pos int
	err bool
}

func (r *bitReader) read(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		if r.pos/8 >= len(r.b) {
			r.err = true
			return 0
		}
		v = v<<1 | int(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

// ac3Rates are the sample rates of AC-3's and E-AC-3's fscod.
var ac3Rates = [3]int{48000, 44100, 32000}

// acmodChannels is the full-bandwidth channel count of an acmod.
var acmodChannels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}

// ac3Config is the configuration of an AC-3 track from a frame of it
// (ETSI TS 102 366, 4.4 and F.4).
func ac3Config(frame []byte) (*audioConfig, error) {
	if len(frame) < 8 || frame[0] != 0x0B || frame[1] != 0x77 {
		return nil, errors.New("AC-3 frame without a syncword")
	}
	r := &bitReader{b: frame, pos: 32}
	fscod := r.read(2)
	frmsizecod := r.read(6)
	bsid := r.read(5)
	bsmod := r.read(3)
	acmod := r.read(3)
	if acmod&1 != 0 && acmod != 1 {
		r.read(2)
	}
	if acmod&4 != 0 {
		r.read(2)
	}
	if acmod == 2 {
		r.read(2)
	}
	lfeon := r.read(1)
	switch {
	case r.err:
		return nil, errors.New("AC-3 header cut short")
	case fscod == 3 || bsid > 10 || frmsizecod > 37:
		return nil, unsupported("AC-3 frame fscod %d bsid %d", fscod, bsid)
	}
	dac3 := &mp4.Dac3Box{FSCod: byte(fscod), BSID: byte(bsid), BSMod: byte(bsmod), ACMod: byte(acmod), LFEOn: byte(lfeon),
		BitRateCode: byte(frmsizecod >> 1)}
	c := &audioConfig{codecs: "ac-3", sampleRate: ac3Rates[fscod], channels: acmodChannels[acmod] + lfeon, frameSamples: 1536}
	c.entry = mp4.CreateAudioSampleEntryBox("ac-3", uint16(c.channels), 16, uint16(c.sampleRate), dac3)
	return c, nil
}

// eac3Config is the configuration of an E-AC-3 track from an access unit of
// it: its independent substreams and their dependent ones (ETSI TS 102 366,
// E.1 and F.6). The JOC extension is not signalled: an object-coded stream
// is served as the E-AC-3 it is compatible with.
func eac3Config(frame []byte, channels int) (*audioConfig, error) {
	type sub struct {
		fscod, bsid, acmod, lfeon int
		numDep                    int
		chanLoc                   int
	}
	var subs []sub
	rate, blocks, bytesTotal := 0, 0, 0
	for off := 0; off+6 <= len(frame); {
		if frame[off] != 0x0B || frame[off+1] != 0x77 {
			if off == 0 {
				return nil, errors.New("E-AC-3 frame without a syncword")
			}
			break // padding after the last syncframe
		}
		r := &bitReader{b: frame[off:], pos: 16}
		strmtyp := r.read(2)
		substreamid := r.read(3)
		frmsiz := r.read(11)
		fscod := r.read(2)
		numblkscod := 3
		fs := 0
		if fscod == 3 {
			fscod2 := r.read(2)
			if fscod2 == 3 {
				return nil, errors.New("E-AC-3 fscod2 reserved")
			}
			fs = ac3Rates[fscod2] / 2
		} else {
			numblkscod = r.read(2)
			fs = ac3Rates[fscod]
		}
		acmod := r.read(3)
		lfeon := r.read(1)
		bsid := r.read(5)
		if bsid <= 10 {
			return nil, unsupported("an AC-3 frame in E-AC-3")
		}
		if bsid > 16 {
			return nil, unsupported("E-AC-3 bsid %d", bsid)
		}
		chanmap := -1
		if strmtyp == 1 {
			r.read(5) // dialnorm
			if r.read(1) == 1 {
				r.read(8)
			}
			if acmod == 0 {
				r.read(5)
				if r.read(1) == 1 {
					r.read(8)
				}
			}
			if r.read(1) == 1 {
				chanmap = r.read(16)
			}
		}
		if r.err {
			return nil, errors.New("E-AC-3 header cut short")
		}
		size := (frmsiz + 1) * 2
		nb := [4]int{1, 2, 3, 6}[numblkscod]
		switch strmtyp {
		case 0, 2: // independent
			if substreamid == 0 && len(subs) > 0 && rate > 0 {
				// The next access unit: this one is complete.
				off = len(frame)
				continue
			}
			subs = append(subs, sub{fscod: fscod, bsid: bsid, acmod: acmod, lfeon: lfeon})
			if substreamid == 0 {
				rate, blocks = fs, nb
			}
		case 1: // dependent on the last independent one
			if len(subs) == 0 {
				return nil, errors.New("E-AC-3 dependent substream first")
			}
			s := &subs[len(subs)-1]
			s.numDep++
			if chanmap >= 0 {
				s.chanLoc |= locBits(chanmap)
			}
		default:
			return nil, unsupported("E-AC-3 stream type %d", strmtyp)
		}
		bytesTotal += size
		off += size
	}
	if len(subs) == 0 || rate == 0 {
		return nil, errors.New("E-AC-3 frame without an independent substream 0")
	}
	samples := blocks * 256
	dec3 := &mp4.Dec3Box{DataRate: uint16(bytesTotal * 8 * rate / samples / 1000), NumIndSub: uint16(len(subs) - 1)}
	for _, s := range subs {
		dec3.EC3Subs = append(dec3.EC3Subs, mp4.EC3Sub{FSCod: byte(s.fscod), BSID: byte(s.bsid), ACMod: byte(s.acmod),
			LFEOn: byte(s.lfeon), NumDepSub: byte(s.numDep), ChanLoc: uint16(s.chanLoc)})
	}
	// The sample entry's channel count is informative (F.6); the track's own
	// count, else the independent substream's.
	if channels <= 0 {
		channels = acmodChannels[subs[0].acmod] + subs[0].lfeon
	}
	c := &audioConfig{codecs: "ec-3", sampleRate: rate, channels: channels, frameSamples: samples}
	if rate > 0xFFFF {
		return nil, unsupported("E-AC-3 at %d Hz", rate)
	}
	c.entry = mp4.CreateAudioSampleEntryBox("ec-3", uint16(channels), 16, uint16(rate), dec3)
	return c, nil
}

// locBits are the chan_loc bits (ETSI TS 102 366, Table F.6.1: Lc/Rc,
// Lrs/Rrs, Cs, Ts, Lsd/Rsd, Lw/Rw, Lvh/Rvh, Cvh, LFE2) of a dependent
// substream's chanmap (Table E.1.4, the first bit L, then C, R, Ls, Rs,
// Lc/Rc, Lrs/Rrs, Cs, Ts, Lsd/Rsd, Lw/Rw, Vhl/Vhr, Vhc, Lts/Rts, LFE2, LFE).
func locBits(chanmap int) int {
	loc := 0
	for j := 0; j < 8; j++ { // chanmap's bits 5 … 12 (counted from the first)
		if chanmap>>(15-(5+j))&1 != 0 {
			loc |= 1 << j
		}
	}
	if chanmap>>(15-14)&1 != 0 {
		loc |= 1 << 8
	}
	return loc
}
