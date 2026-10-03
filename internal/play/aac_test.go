package play

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// An excerpt of `ffmpeg -hide_banner -encoders`, legend included.
const encodersListing = `Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 .F.... = Frame-level multithreading
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 A....D aac                  AAC (Advanced Audio Coding)
 A....D ac3                  ATSC A/52A (AC-3)
 S..... webvtt               WebVTT subtitle
`

func TestAudioEncodersParsesTheEncoderListing(t *testing.T) {
	got := audioEncoders(encodersListing)
	want := map[string]bool{"aac": true, "ac3": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audioEncoders = %v, want %v (no video/subtitle encoders, no legend)", got, want)
	}
}

func TestPickAACEncoderKeepsLibfdkWhereTheBuildHasIt(t *testing.T) {
	cases := []struct {
		name    string
		have    map[string]bool
		want    string
		wantErr bool
	}{
		{"nonfree build", map[string]bool{"aac": true, "libfdk_aac": true}, "libfdk_aac", false},
		{"distro / static build", map[string]bool{"aac": true, "ac3": true}, "aac", false},
		{"no AAC encoder", map[string]bool{"ac3": true}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickAACEncoder(tc.have)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("pickAACEncoder = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestDetectAACEncoderAsksFFmpeg(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		want    string
		wantErr bool
	}{
		{"native only", "cat <<'EOF'\n" + encodersListing + "EOF", "aac", false},
		{"libfdk too", "cat <<'EOF'\n" + encodersListing + " A....D libfdk_aac           Fraunhofer FDK AAC (codec aac)\nEOF", "libfdk_aac", false},
		{"no AAC encoder", "echo ' A....D ac3   ATSC A/52A (AC-3)'", "", true},
		{"ffmpeg fails", "echo boom >&2; exit 1", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectAACEncoder(context.Background(), fakeBin(t, "ffmpeg", tc.script))
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("DetectAACEncoder = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := DetectAACEncoder(context.Background(), filepath.Join(t.TempDir(), "no-ffmpeg")); err == nil {
		t.Error("a missing ffmpeg must fail the detection")
	}
}

// The progressive pipeline encodes with the detected encoder in both modes
// that touch audio, stereo at 48 kHz.
func TestBuildFFmpegEncodesAudioWithTheDetectedEncoder(t *testing.T) {
	for _, mode := range []string{"remux", "transcode"} {
		for _, enc := range []string{"aac", "libfdk_aac"} {
			args := BuildFFmpeg(context.Background(), "ffmpeg", "veryfast", enc, "/m/a.mkv", mode, QualityLadder["high"], 0, 0).Args
			if argAfter(args, "-c:a") != enc || argAfter(args, "-ac") != "2" || argAfter(args, "-ar") != "48000" {
				t.Errorf("%s with %s: %v", mode, enc, args)
			}
		}
	}
	args := BuildFFmpeg(context.Background(), "ffmpeg", "veryfast", "", "/m/a.mkv", "remux", QualityLadder["high"], 0, 0).Args
	if argAfter(args, "-c:a") != "aac" {
		t.Errorf("no detected encoder: -c:a %q, want the native aac", argAfter(args, "-c:a"))
	}
}

// The audio rendition — the request that answered 500/502 with "Unknown
// encoder 'libfdk_aac'" — runs ffmpeg with the encoder the handler was given,
// stereo, at windowAudioBitrate.
func TestAudioWindowEncodesWithTheDetectedEncoder(t *testing.T) {
	root, src := mediaFile(t, "film.mkv")
	state := t.TempDir()
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  windowFFmpeg(t, state, 0, 2, ""),
		FFprobeBin: probeFFprobe(t, `{"streams":[{"codec_type":"audio","codec_name":"ac3","channels":6}],"format":{"format_name":"matroska,webm","duration":"9.0"}}`),
		CacheDir:   t.TempDir(),
		AACEncoder: "aac",
	}
	if w := get(h, "/api/play/i1/audio/0/init.mp4"); w.Code != 200 || w.Body.String() != "init" {
		t.Fatalf("audio init: %d %q", w.Code, w.Body)
	}
	runs := ffmpegRuns(t, state)
	if len(runs) != 1 {
		t.Fatalf("%d ffmpeg runs, want 1", len(runs))
	}
	args := runs[0]
	if argAfter(args, "-c:a") != "aac" || argAfter(args, "-ac") != "2" || argAfter(args, "-b:a") != windowAudioBitrate ||
		slices.Contains(args, "libfdk_aac") {
		t.Errorf("audio window args %v", args)
	}
}

// End to end with the ffmpeg on $PATH: a 5.1 AC-3 source comes out of the
// audio rendition as AAC-LC stereo at 48 kHz that ffprobe can read.
func TestAudioRenditionWithRealFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	enc, err := DetectAACEncoder(context.Background(), ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	src := filepath.Join(root, "surround.mkv")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=8",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=8",
		"-filter_complex", "[1:a]pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0[a]",
		"-map", "0:v", "-map", "[a]", "-c:v", "mpeg4", "-c:a", "ac3", src)
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  ffmpeg,
		FFprobeBin: ffprobe,
		CacheDir:   t.TempDir(),
		AACEncoder: enc,
	}
	var media []byte
	for _, leaf := range []string{"init.mp4", "0.m4s", "1.m4s"} {
		w := get(h, "/api/play/i1/audio/0/"+leaf)
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("audio %s: %d %q", leaf, w.Code, w.Body)
		}
		media = append(media, w.Body.Bytes()...)
	}
	out := filepath.Join(t.TempDir(), "audio.mp4")
	if err := os.WriteFile(out, media, 0o644); err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			Profile    string `json:"profile"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(run(t, ffprobe, "-v", "error", "-of", "json", "-show_streams", "-show_format", out), &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 1 {
		t.Fatalf("streams %+v, want one audio stream", probe.Streams)
	}
	s := probe.Streams[0]
	if s.CodecName != "aac" || s.Profile != "LC" || s.Channels != 2 || s.SampleRate != "48000" {
		t.Errorf("rendition is %+v, want AAC LC stereo 48 kHz (encoder %s)", s, enc)
	}
	if !strings.HasPrefix(probe.Format.Duration, "8.") && !strings.HasPrefix(probe.Format.Duration, "7.9") {
		t.Errorf("rendition lasts %ss, want the source's 8 s", probe.Format.Duration)
	}
}
