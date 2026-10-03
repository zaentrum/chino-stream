package play

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyAudioCodecIsTheSourcesCodec(t *testing.T) {
	cases := []struct{ codec, profile, want string }{
		{"aac", "LC", "mp4a.40.2"},
		{"aac", "", "mp4a.40.2"},
		{"aac", "HE-AAC", "mp4a.40.5"},
		{"aac", "HE-AACv2", "mp4a.40.29"},
		{"aac", "Main", "mp4a.40.1"},
		{"ac3", "", "ac-3"},
		{"eac3", "", "ec-3"},
		{"opus", "", "opus"},
		{"mp3", "", "mp4a.40.34"},
		{"flac", "", "fLaC"},
		// No MP4 codec string to give: left out rather than called AAC.
		{"dts", "DTS-HD MA", ""},
		{"vorbis", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := copyAudioCodec(&Probe{AudioCodec: tc.codec, AudioProfile: tc.profile}); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.codec, tc.profile, got, tc.want)
		}
	}
}

// The stream-copy variant declares the audio it copies — not mp4a.40.2 for
// an E-AC-3, Opus or HE-AAC track — and leaves out a codec it cannot name.
func TestCopyVariantDeclaresTheRealAudioCodec(t *testing.T) {
	captureLog(t)
	const mp4 = "mov,mp4,m4a,3gp,3g2,mj2"
	cases := []struct {
		name, probe, query, codecs string
	}{
		{"E-AC-3 to a client that decodes it",
			probeJSON(mp4, "h264", 1920, 1080, "eac3", 6, 9_000_000), "?caps=avc,eac3", `CODECS="avc1.640028,ec-3"`},
		{"AC-3", probeJSON(mp4, "h264", 1920, 1080, "ac3", 2, 9_000_000), "?caps=avc,ac3", `CODECS="avc1.640028,ac-3"`},
		{"Opus", probeJSON(mp4, "hevc", 1920, 1080, "opus", 2, 9_000_000), "?caps=hvc,opus", `CODECS="hvc1.1.6.L120.B0,opus"`},
		{"HE-AAC", strings.Replace(probeJSON(mp4, "h264", 1280, 720, "aac", 2, 2_000_000),
			`"codec_name":"aac"`, `"codec_name":"aac","profile":"HE-AAC"`, 1), "", `CODECS="avc1.640028,mp4a.40.5"`},
		{"AAC-LC", probeJSON(mp4, "h264", 1280, 720, "aac", 2, 2_000_000), "", `CODECS="avc1.640028,mp4a.40.2"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, src := mediaFile(t, "film.mp4")
			cache, err := os.MkdirTemp("", "chino-stream-master-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(cache) })
			h := &HLSHandler{
				Catalog:    fakeKatalog(t, src),
				MediaRoot:  root,
				FFmpegBin:  fakeBin(t, "ffmpeg", "exit 1"),
				FFprobeBin: probeFFprobe(t, tc.probe),
				CacheDir:   cache,
			}
			body := get(h, "/api/play/i1/master.m3u8"+tc.query).Body.String()
			if !strings.Contains(body, "\ncopy/index.m3u8") || !strings.Contains(body, tc.codecs+",") {
				t.Errorf("want the copy variant with %s:\n%s", tc.codecs, body)
			}
		})
	}
}

// With the ffprobe on $PATH: the probe names the codecs and AAC profiles the
// way copyAudioCodec reads them.
func TestRealFFprobeNamesTheAudioCodec(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	enc := allEncoders(t, ffmpeg)
	root := t.TempDir()
	cases := []struct{ name, encoder, want string }{
		{"aac.mp4", "aac", "mp4a.40.2"},
		{"ac3.mp4", "ac3", "ac-3"},
		{"eac3.mp4", "eac3", "ec-3"},
		{"opus.mp4", "libopus", "opus"},
	}
	for _, tc := range cases {
		if !enc[tc.encoder] {
			t.Logf("no %s encoder, skipping %s", tc.encoder, tc.name)
			continue
		}
		src := filepath.Join(root, tc.name)
		run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=duration=1:sample_rate=48000",
			"-c:a", tc.encoder, "-ac", "2", src)
		p, err := RunFFprobe(context.Background(), ffprobe, src)
		if err != nil {
			t.Fatal(err)
		}
		if got := copyAudioCodec(&p); got != tc.want {
			t.Errorf("%s: probe %q/%q gives %q, want %q", tc.name, p.AudioCodec, p.AudioProfile, got, tc.want)
		}
	}
}
