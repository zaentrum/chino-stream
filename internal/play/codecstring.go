package play

// copyAudioCodec is the CODECS entry (RFC 6381, as HLS uses it) for the audio
// the stream-copy variant carries: the source's first audio track (`-map
// 0:a:0`), byte for byte. The copy variant used to declare mp4a.40.2 whatever
// the source had, so an AC-3, E-AC-3 or Opus track was announced as AAC-LC and
// players picked the wrong decoder or refused the track.
//
// The strings are the ones ffmpeg's own HLS muxer writes (ac-3, ec-3,
// mp4a.40.34 for MP3, mp4a.40.33 for MP2), plus opus and fLaC, and for AAC the
// audio object type of the profile ffprobe reports. "" for a codec without a
// known MP4 codec string: the entry is then left out — better absent than
// wrong.
func copyAudioCodec(p *Probe) string {
	switch p.AudioCodec {
	case "aac":
		return "mp4a.40." + aacObjectType(p.AudioProfile)
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	case "opus":
		return "opus"
	case "mp3":
		return "mp4a.40.34"
	case "mp2":
		return "mp4a.40.33"
	case "flac":
		return "fLaC"
	}
	return ""
}

// aacObjectType is the MPEG-4 audio object type of an AAC profile as ffprobe
// names it; LC (2) when it names none or one this does not know.
func aacObjectType(profile string) string {
	switch profile {
	case "Main":
		return "1"
	case "SSR":
		return "3"
	case "LTP":
		return "4"
	case "HE-AAC":
		return "5"
	case "LD":
		return "23"
	case "HE-AACv2":
		return "29"
	case "ELD":
		return "39"
	case "xHE-AAC":
		return "42"
	}
	return "2"
}
