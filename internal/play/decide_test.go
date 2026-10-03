package play

import (
	"strings"
	"testing"
)

// /play/info shows the reason to the user, and it describes the HLS
// pipeline the player gets: a remux re-encodes the video there as well, so
// the reason must not say the video is stream-copied.
func TestDecideWithSaysWhatTheHLSPipelineDoes(t *testing.T) {
	const mp4, mkv = "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm"
	track := func(codec string, ch int) []TrackInfo { return []TrackInfo{{Codec: codec, Channels: ch}} }
	cases := []struct {
		name  string
		probe Probe
		mode  string
		says  string
	}{
		{"AC-3 audio", Probe{Container: mp4, VideoCodec: "h264", AudioCodec: "ac3", AudioTracks: track("ac3", 6)},
			"remux", "audio codec 'ac3' is not in the client's decoder set — audio re-encoded to stereo AAC LC, and the video too"},
		{"5.1 AAC, no multichannel", Probe{Container: mp4, VideoCodec: "h264", AudioCodec: "aac", AudioTracks: track("aac", 6)},
			"remux", "6-channel AAC and the client didn't signal multichannel support — audio re-encoded to stereo AAC LC, and the video too"},
		{"MKV container", Probe{Container: mkv, VideoCodec: "h264", AudioCodec: "aac", AudioTracks: track("aac", 2)},
			"remux", "needs repackaging to fragmented MP4 — video and audio are re-encoded"},
		{"all compatible", Probe{Container: mp4, VideoCodec: "h264", AudioCodec: "aac", AudioTracks: track("aac", 2)},
			"passthrough", "stream-copied, nothing re-encoded"},
		{"HEVC on a client without it", Probe{Container: mkv, VideoCodec: "hevc", AudioCodec: "aac", AudioTracks: track("aac", 2)},
			"transcode", "video codec 'hevc' is not in the client's decoder set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, reason := tc.probe.DecideWith(DefaultCaps)
			if mode != tc.mode || !strings.Contains(reason, tc.says) {
				t.Errorf("DecideWith = %q, %q; want %q saying %q", mode, reason, tc.mode, tc.says)
			}
			if mode == "remux" && strings.Contains(reason, "stream-copied") {
				t.Errorf("a remux reason claims a stream copy: %q", reason)
			}
		})
	}
}
