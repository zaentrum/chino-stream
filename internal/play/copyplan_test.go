package play

import (
	"strings"
	"testing"
)

// The caps the clients send (chino-androidtv's CodecCaps, chino-web's caps).
var (
	tvCaps   = ParseCaps("avc:4096,hvc:4096,av1:3840,aac,mp3,opus,ac3,eac3")
	webCaps  = ParseCaps("avc,hvc,av1,vp9,aac,mp3,opus")
	h264Caps = ParseCaps("avc,aac,mp3,opus")
)

const (
	mkvFormat = "matroska,webm"
	mp4Format = "mov,mp4,m4a,3gp,3g2,mj2"
)

// uhdEpisode is an episode as many unpackaged originals are: Matroska,
// 2160p HEVC Main 10, 5.1 E-AC-3 with Atmos, SubRip subtitles.
func uhdEpisode() Probe {
	return Probe{Container: mkvFormat, VideoCodec: "hevc", VideoProfile: "Main 10", PixFmt: "yuv420p10le", Width: 3840, Height: 2160,
		ColorTransfer: "bt709", AudioCodec: "eac3",
		AudioTracks:    []TrackInfo{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true, Profile: "Dolby Digital Plus + Dolby Atmos"}},
		SubtitleTracks: []TrackInfo{{Index: 0, Codec: "subrip", Language: "eng"}}}
}

// with is p changed by edit.
func with(p Probe, edit func(*Probe)) Probe {
	edit(&p)
	return p
}

func audioTrack(codec string, ch int) []TrackInfo {
	return []TrackInfo{{Index: 0, Codec: codec, Channels: ch, Language: "eng", Default: true}}
}

// What plays as it is, decided from the probe and the client's caps: the
// container never forces an encode, the video is copied where the client
// decodes it as it is, each audio track copied where the client decodes it
// and converted otherwise, the video never encoded for the audio's sake; an
// MP4 the client takes whole is the muxed stream copy it always was.
func TestCopyDecisionTable(t *testing.T) {
	ep := uhdEpisode()
	for _, tc := range []struct {
		name      string
		probe     Probe
		caps      Caps
		videoCopy bool
		why       string // a part of why the video is not copied
		audio     []bool // each track copied
		muxed     bool
	}{
		// A 4K Matroska episode, for a TV that decodes it all and for a browser.
		{"TV, MKV HEVC 2160p E-AC-3 5.1", ep, tvCaps, true, "", []bool{true}, false},
		{"web, MKV HEVC 2160p E-AC-3 5.1", ep, webCaps, true, "", []bool{false}, false},
		{"H.264-only, HEVC", ep, h264Caps, false, "does not play HEVC", []bool{false}, false},
		// Dolby Vision.
		{"DV 5, TV", with(ep, func(p *Probe) { p.DVProfile, p.DVCompat = 5, 0 }), tvCaps, false, "does not play Dolby Vision", []bool{true}, false},
		{"DV 5, a client that declares it", with(ep, func(p *Probe) { p.DVProfile, p.DVCompat = 5, 0 }), ParseCaps("hvc,eac3,aac,dvh"), true, "", []bool{true}, false},
		{"DV 5 in a dvh1 entry without a record", with(ep, func(p *Probe) { p.Container, p.CodecTag = mp4Format, "dvh1" }), tvCaps, false, "Dolby Vision", []bool{true}, false},
		{"DV 8.1, TV: its HDR10 base layer", with(ep, func(p *Probe) { p.DVProfile, p.DVCompat, p.ColorTransfer = 8, 1, "smpte2084" }), tvCaps, true, "", []bool{true}, false},
		{"DV 7, even declared", with(ep, func(p *Probe) { p.DVProfile, p.DVCompat = 7, 6 }), ParseCaps("hvc,eac3,dvh"), false, "enhancement layer", []bool{true}, false},
		{"HDR10 HEVC, web", with(ep, func(p *Probe) { p.ColorTransfer = "smpte2084" }), webCaps, true, "", []bool{false}, false},
		// Heights.
		{"HEVC over the height cap", ep, ParseCaps("avc:1080,hvc:1080,aac,eac3"), false, "plays HEVC up to 1080p", []bool{true}, false},
		{"HEVC within the height cap", with(ep, func(p *Probe) { p.Width, p.Height = 1920, 1080 }), ParseCaps("hvc:1080,aac,eac3"), true, "", []bool{true}, false},
		// Formats every decoder of the kind does not play.
		{"H.264 High 10", Probe{Container: mkvFormat, VideoCodec: "h264", VideoProfile: "High 10", PixFmt: "yuv420p10le", Width: 1920, Height: 1080,
			AudioCodec: "aac", AudioTracks: audioTrack("aac", 2)}, webCaps, false, "10-bit H.264", []bool{true}, false},
		{"HEVC 4:2:2", with(ep, func(p *Probe) { p.VideoProfile, p.PixFmt = "Rext", "yuv422p10le" }), tvCaps, false, "HEVC in 4:2:2", []bool{true}, false},
		{"VP9 in Matroska", Probe{Container: mkvFormat, VideoCodec: "vp9", VideoProfile: "Profile 0", PixFmt: "yuv420p", Width: 1920, Height: 1080,
			AudioCodec: "opus", AudioTracks: audioTrack("opus", 2)}, webCaps, false, "VP9 video in Matroska files", []bool{false}, false},
		{"MPEG-4 Part 2", Probe{Container: mkvFormat, VideoCodec: "mpeg4", Width: 1280, Height: 720, AudioCodec: "aac", AudioTracks: audioTrack("aac", 1)},
			tvCaps, false, "does not play MPEG-4 Part 2", []bool{true}, false},
		// Audio, track by track.
		{"AAC 5.1 without aacmc", with(ep, func(p *Probe) { p.AudioTracks = audioTrack("aac", 6) }), webCaps, true, "", []bool{false}, false},
		{"AAC 5.1 with aacmc", with(ep, func(p *Probe) { p.AudioTracks = audioTrack("aac", 6) }), ParseCaps("hvc,aac,aacmc"), true, "", []bool{true}, false},
		{"E-AC-3 and DTS, TV", with(ep, func(p *Probe) {
			p.AudioTracks = append(audioTrack("eac3", 6), TrackInfo{Index: 1, Codec: "dts", Channels: 6, Language: "ger"})
		}), tvCaps, true, "", []bool{true, false}, false},
		{"Opus is converted (not carried by the copy)", with(ep, func(p *Probe) { p.AudioTracks = audioTrack("opus", 2) }), tvCaps, true, "", []bool{false}, false},
		// MP4 originals that always copied are unchanged: the muxed copy.
		{"MP4 H.264 + AAC, web", Probe{Container: mp4Format, VideoCodec: "h264", VideoProfile: "High", PixFmt: "yuv420p", Width: 1920, Height: 1080,
			AudioCodec: "aac", AudioTracks: audioTrack("aac", 2)}, webCaps, true, "", []bool{true}, true},
		{"MP4 HEVC + AAC, TV", Probe{Container: mp4Format, VideoCodec: "hevc", VideoProfile: "Main", PixFmt: "yuv420p", Width: 1920, Height: 1080,
			AudioCodec: "aac", AudioTracks: audioTrack("aac", 2)}, tvCaps, true, "", []bool{true}, true},
		{"MP4 H.264 + AC-3, web: the video copied, the audio converted", Probe{Container: mp4Format, VideoCodec: "h264", VideoProfile: "High",
			PixFmt: "yuv420p", Width: 1920, Height: 1080, AudioCodec: "ac3", AudioTracks: audioTrack("ac3", 6)}, webCaps, true, "", []bool{false}, false},
		{"MPEG-TS H.264 + AAC, web: the muxed copy as before", Probe{Container: "mpegts", VideoCodec: "h264", VideoProfile: "High", PixFmt: "yuv420p",
			Width: 1920, Height: 1080, AudioCodec: "aac", AudioTracks: audioTrack("aac", 2)}, webCaps, true, "", []bool{true}, true},
		{"MP4 H.264 + AAC, H.264-only", Probe{Container: mp4Format, VideoCodec: "h264", VideoProfile: "Main", PixFmt: "yuv420p", Width: 1280, Height: 720,
			AudioCodec: "aac", AudioTracks: audioTrack("aac", 2)}, h264Caps, true, "", []bool{true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := planStreams(&tc.probe, tc.caps)
			if sp.videoCopy != tc.videoCopy || !strings.Contains(sp.videoWhy, tc.why) || (tc.why == "" && sp.videoWhy != "") {
				t.Errorf("video copy %v (%q), want %v (%q)", sp.videoCopy, sp.videoWhy, tc.videoCopy, tc.why)
			}
			if sp.muxed != tc.muxed {
				t.Errorf("muxed %v, want %v", sp.muxed, tc.muxed)
			}
			if len(sp.audio) != len(tc.audio) {
				t.Fatalf("%d audio plans, want %d", len(sp.audio), len(tc.audio))
			}
			for i, a := range sp.audio {
				if a.copy != tc.audio[i] {
					t.Errorf("audio %d copied %v, want %v (%s)", i, a.copy, tc.audio[i], a.why)
				}
			}
		})
	}
}

// /play/info's reasons say what happens and why, in the player's words: no
// internal names of pipelines, no settings, sentence case, no dashes.
func TestCopyReasons(t *testing.T) {
	ep := uhdEpisode()
	two := with(ep, func(p *Probe) {
		p.AudioTracks = append(audioTrack("eac3", 6), TrackInfo{Index: 1, Codec: "dts", Channels: 6, Language: "ger"},
			TrackInfo{Index: 2, Codec: "aac", Channels: 6, Language: "fre"})
	})
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"all copied", copyReason(planStreams(&ep, tvCaps), &ep), "The video and the audio play as they are in the file."},
		{"audio converted", copyReason(planStreams(&ep, webCaps), &ep),
			"The video plays as it is in the file. The audio is converted to stereo AAC, because this device does not play E-AC-3."},
		{"some converted", copyReason(planStreams(&two, tvCaps), &two),
			"The video and the E-AC-3 audio play as they are in the file. The DTS and multichannel AAC audio is converted to stereo AAC, " +
				"because this device does not play DTS and plays AAC in stereo only."},
		{"no audio", copyReason(streamPlan{videoCopy: true}, &ep), "The video plays as it is in the file."},
		{"encoded", encodeReason(planStreams(&ep, h264Caps).videoWhy, true),
			"The video is converted to H.264 and the audio to stereo AAC, because this device does not play HEVC."},
		{"over the cap", encodeReason(planStreams(&ep, ParseCaps("hvc:1080,eac3")).videoWhy, true),
			"The video is converted to H.264 and the audio to stereo AAC, because this device plays HEVC up to 1080p."},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
		for _, word := range []string{"remux", "transcod", "ffmpeg", "pipeline", "client", "caps", "—", "on the fly", "stream-cop"} {
			if strings.Contains(strings.ToLower(tc.got), word) {
				t.Errorf("%s: %q says %q", tc.name, tc.got, word)
			}
		}
	}
}

// The caps token dvh says the client plays Dolby Vision; nothing else does.
func TestCapsDolbyVision(t *testing.T) {
	if !ParseCaps("hvc,dvh").DolbyVision || ParseCaps("hvc,eac3").DolbyVision || DefaultCaps.DolbyVision {
		t.Error("dvh")
	}
}

func TestSecondsNs(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0.000000": 0, "-0.021000": -21_000_000, "1.5": 1_500_000_000, "12.000001": 12_000_001_000, "x": 0} {
		if got := secondsNs(in); got != want {
			t.Errorf("secondsNs(%q) = %d, want %d", in, got, want)
		}
	}
}
