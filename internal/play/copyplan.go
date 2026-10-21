package play

import (
	"fmt"
	"strings"
)

// What an original plays as on the fly: its video copied when the client
// decodes it as it is, else encoded (the transcode ladder); each audio track
// copied when the client decodes it, else converted to AAC on its own HLS
// rendition. The container never forces an encode: Matroska and MP4 are
// repackaged into fMP4 by the stream copy (streamcopy), and a track is
// converted only for what the client cannot decode. The video is never
// encoded for the sake of the audio.
//
// Copied video is what every decoder of the client's kind plays: H.264
// Baseline, Main or High at 8 bits; HEVC Main or Main 10; AV1 Main; 4:2:0;
// no taller than the client's cap for its codec. HDR10 and HLG are copied
// where the client takes the codec at that height, as a package's HEVC is
// served. Dolby Vision is conservative: a stream whose base layer other
// decoders play (8.1, 8.4) is copied as that base layer; one without such a
// base layer (profile 5) only to a client that declares Dolby Vision; one
// with an enhancement layer (profile 7) never.

// streamPlan is what plays of an original for a client: the probe and the
// caps decide it (planStreams), before any index is looked at.
type streamPlan struct {
	// videoCopy: the client decodes the video as it is; videoWhy says why it
	// does not, in the reason's words.
	videoCopy bool
	videoWhy  string
	// audio is each of the probe's audio tracks: copied, or converted.
	audio []trackPlan
	// muxed: an original of another container than Matroska whose video and
	// first audio track the client takes (an MP4, an MPEG-TS), served as
	// before as one stream copy of both (copy/), the other tracks left out.
	muxed bool
}

// trackPlan is what plays of an audio track.
type trackPlan struct {
	// copy: the client decodes the track as it is. Otherwise it is converted
	// to stereo AAC, and with surround to 5.1 AAC as well (a client that
	// decodes AAC beyond two channels, aacmc).
	copy     bool
	surround bool
	// why is why it is converted: "E-AC-3", or "aacmc" for AAC beyond two
	// channels to a client that decodes stereo AAC only.
	why string
}

// copyAudioCodecs are the audio codecs the stream copy carries over as they
// are, from any container.
var copyAudioCodecs = map[string]bool{"aac": true, "ac3": true, "eac3": true}

// planStreams decides what of the original p plays as it is for a client
// with caps.
func planStreams(p *Probe, caps Caps) streamPlan {
	var sp streamPlan
	sp.videoCopy, sp.videoWhy = videoCopyable(p, caps)
	for _, t := range p.AudioTracks {
		tp := trackPlan{copy: audioDecodes(t, caps)}
		if !tp.copy {
			tp.why = audioWhy(t, caps)
			tp.surround = caps.AACMultichannel && t.Channels > 2
		}
		sp.audio = append(sp.audio, tp)
	}
	// An original the client takes whole is served as it always was: the
	// stream copy of the video and the first audio track together (Matroska
	// and AVI never were: DecideWith repackages them).
	if sp.videoCopy {
		if mode, _ := p.DecideWith(caps); mode == "passthrough" {
			sp.muxed = true
		}
	}
	return sp
}

// videoCopyable says whether a client with caps decodes the video of p as it
// is, and why not.
func videoCopyable(p *Probe, caps Caps) (bool, string) {
	fam := videoFamily(p.VideoCodec)
	name := codecDisplay(p.VideoCodec)
	if fam == "" || !caps.Video[p.VideoCodec] {
		return false, "this device does not play " + name
	}
	if maxH := caps.VideoMaxHeight[fam]; maxH > 0 && p.Height > maxH {
		return false, fmt.Sprintf("this device plays %s up to %dp", name, maxH)
	}
	if why := videoFormatWhy(p, fam, name); why != "" {
		return false, "this device does not play " + why
	}
	if fam == "vp9" && isMatroska(p.Container) {
		// The copy takes a VP9 stream's configuration from an MP4's sample
		// entry; a Matroska file has none to give.
		return false, "VP9 video in Matroska files is always converted"
	}
	if dv := dolbyVision(p); dv != dvNone {
		switch {
		case dv == dvEnhancement:
			return false, "this device does not play Dolby Vision with an enhancement layer"
		case dv == dvOnly && !caps.DolbyVision:
			return false, "this device does not play Dolby Vision"
		}
	}
	return true, ""
}

// videoFamily is the caps family of an ffprobe video codec name the stream
// copy takes; "" for any other.
func videoFamily(codec string) string {
	switch codec {
	case "h264":
		return "h264"
	case "hevc":
		return "hevc"
	case "av1":
		return "av1"
	case "vp9":
		return "vp9"
	}
	return ""
}

// videoFormatWhy is what of the video's format every decoder of its kind does
// not play — its profile, bit depth or chroma — in the reason's words; ""
// when it is one they do.
func videoFormatWhy(p *Probe, fam, name string) string {
	depth, chroma := pixFmtLayout(p.PixFmt)
	switch {
	case chroma != "" && chroma != "420":
		return fmt.Sprintf("%s in %s:%s:%s", name, chroma[:1], chroma[1:2], chroma[2:])
	case fam == "h264" && depth > 8:
		return fmt.Sprintf("%d-bit H.264", depth)
	case depth > 10:
		return fmt.Sprintf("%d-bit %s", depth, name)
	}
	prof := strings.ToLower(p.VideoProfile)
	ok := prof == ""
	switch fam {
	case "h264":
		ok = ok || prof == "baseline" || prof == "constrained baseline" || prof == "main" || prof == "high"
	case "hevc":
		ok = ok || prof == "main" || prof == "main 10"
	case "av1":
		ok = ok || prof == "main"
	case "vp9":
		ok = ok || prof == "profile 0"
	}
	if !ok {
		return name + " " + p.VideoProfile
	}
	return ""
}

// pixFmtLayout is the bit depth and chroma ("420", "422", "444") of an
// ffprobe pixel format name; 0 and "" when it is none it knows.
func pixFmtLayout(f string) (depth int, chroma string) {
	f = strings.ToLower(f)
	switch {
	case f == "":
		return 0, ""
	case f == "nv12" || f == "nv21":
		return 8, "420"
	case f == "p010le" || f == "p010be":
		return 10, "420"
	}
	for _, c := range []string{"420", "422", "444", "440", "411", "410"} {
		if i := strings.Index(f, c+"p"); i >= 0 && strings.HasPrefix(f, "yuv") {
			depth = 8
			rest := strings.TrimSuffix(strings.TrimSuffix(f[i+4:], "le"), "be")
			if rest != "" {
				fmt.Sscanf(rest, "%d", &depth)
			}
			return depth, c
		}
	}
	return 0, ""
}

// Dolby Vision of a stream, as far as copying it goes.
const (
	dvNone        = iota
	dvBaseLayer   // a base layer other decoders play (8.1, 8.2, 8.4, …): copied as that
	dvOnly        // no such base layer (profile 5, a compatibility of 0, a Dolby Vision sample entry without a record)
	dvEnhancement // profile 7: a base layer and an enhancement layer
)

// dolbyVision is what Dolby Vision the video of p carries.
func dolbyVision(p *Probe) int {
	tag := strings.ToLower(p.CodecTag)
	dvTag := tag == "dvh1" || tag == "dvhe" || tag == "dav1" || tag == "dva1" || tag == "dvav"
	switch {
	case p.DVProfile == 0 && !dvTag:
		return dvNone
	case p.DVProfile == 7:
		return dvEnhancement
	case p.DVProfile == 0, p.DVProfile == 5, p.DVCompat == 0:
		return dvOnly
	}
	return dvBaseLayer
}

// audioDecodes says whether a client with caps decodes the audio track t as
// it is, in a codec the stream copy carries.
func audioDecodes(t TrackInfo, caps Caps) bool {
	if !copyAudioCodecs[t.Codec] || !caps.Audio[t.Codec] {
		return false
	}
	if t.Codec == "aac" {
		switch t.Profile {
		case "", "LC", "HE-AAC", "HE-AACv2":
		default:
			return false
		}
		return t.Channels <= 2 || caps.AACMultichannel
	}
	return true
}

// audioWhy is why the audio track t is converted for a client with caps:
// the codec's name, or "aacmc" for AAC beyond two channels.
func audioWhy(t TrackInfo, caps Caps) string {
	if t.Codec == "aac" && caps.Audio["aac"] && t.Channels > 2 && !caps.AACMultichannel {
		return "aacmc"
	}
	return codecDisplay(t.Codec)
}

// codecDisplay is a codec's name as a reason says it.
func codecDisplay(codec string) string {
	switch c := strings.ToLower(codec); {
	case c == "h264":
		return "H.264"
	case c == "hevc":
		return "HEVC"
	case c == "av1":
		return "AV1"
	case c == "vp9":
		return "VP9"
	case c == "vp8":
		return "VP8"
	case c == "mpeg4":
		return "MPEG-4 Part 2"
	case c == "mpeg2video":
		return "MPEG-2"
	case c == "mpeg1video":
		return "MPEG-1"
	case c == "vc1":
		return "VC-1"
	case c == "aac":
		return "AAC"
	case c == "ac3":
		return "AC-3"
	case c == "eac3":
		return "E-AC-3"
	case c == "dts":
		return "DTS"
	case c == "truehd":
		return "Dolby TrueHD"
	case c == "flac":
		return "FLAC"
	case c == "opus":
		return "Opus"
	case c == "vorbis":
		return "Vorbis"
	case c == "mp3":
		return "MP3"
	case c == "mp2":
		return "MP2"
	case c == "alac":
		return "ALAC"
	case strings.HasPrefix(c, "pcm_"):
		return "PCM"
	case c == "":
		return "this format"
	}
	return strings.ToUpper(codec)
}

// The reasons /play/info gives: what happens, and the cause, in the words
// the player shows them.

// copyReason is the reason of a session that copies the video: which audio
// is copied, which converted, and why. probe gives the tracks' codecs.
func copyReason(sp streamPlan, p *Probe) string {
	if len(sp.audio) == 0 {
		return "The video plays as it is in the file."
	}
	var copied, converted, unplayed []string
	surround, stereoOnly := false, false
	for i, a := range sp.audio {
		name := codecDisplay(p.AudioTracks[i].Codec)
		if a.copy {
			copied = appendOnce(copied, name)
			continue
		}
		surround = surround || a.surround
		if a.why == "aacmc" {
			converted = appendOnce(converted, "multichannel AAC")
			stereoOnly = true
		} else {
			converted = appendOnce(converted, name)
			unplayed = appendOnce(unplayed, name)
		}
	}
	if len(converted) == 0 {
		return "The video and the audio play as they are in the file."
	}
	to := "stereo AAC"
	if surround {
		to = "AAC"
	}
	var clauses []string
	if len(unplayed) > 0 {
		clauses = append(clauses, "does not play "+orList(unplayed))
	}
	if stereoOnly {
		clauses = append(clauses, "plays AAC in stereo only")
	}
	cause := "this device " + strings.Join(clauses, " and ")
	if len(copied) == 0 {
		return "The video plays as it is in the file. The audio is converted to " + to + ", because " + cause + "."
	}
	return "The video and the " + andList(copied) + " audio play as they are in the file. The " + andList(converted) +
		" audio is converted to " + to + ", because " + cause + "."
}

func appendOnce(list []string, s string) []string {
	if containsFold(list, s) {
		return list
	}
	return append(list, s)
}

// orList joins names as "A", "A or B", "A, B or C".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// andList joins names as "A", "A and B", "A, B and C".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// encodeReason is the reason of a session whose video is encoded: what it
// is converted to and why.
func encodeReason(why string, audio bool) string {
	what := "The video is converted to H.264"
	if audio {
		what += " and the audio to stereo AAC"
	}
	if why == "" {
		return what + "."
	}
	return what + ", because " + why + "."
}
