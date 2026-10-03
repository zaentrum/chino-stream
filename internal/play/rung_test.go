package play

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTranscodeGeometry(t *testing.T) {
	high, medium, low := QualityLadder["high"], QualityLadder["medium"], QualityLadder["low"]
	cases := []struct {
		name             string
		ql               Quality
		srcW, srcH, cap  int
		w, h             int
		scaleH, fitW, fH int
	}{
		{"high keeps 1080p", high, 1920, 1080, 0, 1920, 1080, 0, 0, 0},
		{"high keeps 720p", high, 1280, 720, 0, 1280, 720, 0, 0, 0},
		{"high fits 4K into 1080", high, 3840, 2160, 0, 1920, 1080, 0, 1920, 1080},
		{"high fits ultrawide by width", high, 3840, 1606, 0, 1920, 804, 0, 1920, 804},
		{"high fits 2560x1080", high, 2560, 1080, 0, 1920, 810, 0, 1920, 810},
		{"high under a 720 HW cap", high, 3840, 2160, 720, 1280, 720, 0, 1280, 720},
		{"a 1080 cap leaves 1080p", high, 1920, 1080, 1080, 1920, 1080, 0, 0, 0},
		{"medium scales 1080p to 720", medium, 1920, 1080, 0, 1280, 720, 720, 0, 0},
		{"medium keeps the ultrawide aspect", medium, 3840, 1606, 0, 1722, 720, 720, 0, 0},
		{"medium never upscales", medium, 640, 360, 0, 640, 360, 360, 0, 0},
		{"medium on an odd source height", medium, 853, 481, 0, 852, 480, 480, 0, 0},
		{"low under a 360 HW cap", low, 1280, 720, 360, 640, 360, 360, 0, 0},
		{"medium, source size unknown", medium, 0, 0, 0, 0, 720, 720, 0, 0},
		{"high, source size unknown", high, 0, 0, 0, 0, 0, 0, 0, 0},
		{"high, source height unknown", high, 3840, 0, 0, 3840, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		g := transcodeGeometry(tc.ql, tc.srcW, tc.srcH, tc.cap)
		want := rungGeometry{scaleH: tc.scaleH, fitW: tc.fitW, fitH: tc.fH, w: tc.w, h: tc.h}
		if g != want {
			t.Errorf("%s: %+v, want %+v", tc.name, g, want)
		}
	}
}

// The encoder scales to exactly the geometry the master advertises, on both
// encoder paths.
func TestVideoEncoderArgsScaleToTheGeometry(t *testing.T) {
	cases := []struct {
		rung             string
		srcW, srcH, cap  int
		libx264, nvencVF string
	}{
		{"high", 1280, 720, 0, "format=yuv420p", "scale_cuda=iw:ih:format=nv12"},
		{"high", 3840, 1606, 0, "scale=1920:804,format=yuv420p", "scale_cuda=1920:804:format=nv12"},
		{"medium", 1920, 1080, 0, "scale=-2:720,format=yuv420p", "scale_cuda=-2:720:format=nv12"},
		{"medium", 640, 360, 0, "scale=-2:360,format=yuv420p", "scale_cuda=-2:360:format=nv12"},
		{"low", 1280, 720, 360, "scale=-2:360,format=yuv420p", "scale_cuda=-2:360:format=nv12"},
	}
	for _, tc := range cases {
		ql := QualityLadder[tc.rung]
		if vf := argAfter(videoEncoderArgs(ql, "veryfast", false, false, "p5", "23", tc.srcW, tc.srcH, tc.cap), "-vf"); vf != tc.libx264 {
			t.Errorf("%s %dx%d libx264 -vf %q, want %q", tc.rung, tc.srcW, tc.srcH, vf, tc.libx264)
		}
		if vf := argAfter(videoEncoderArgs(ql, "veryfast", false, true, "p5", "23", tc.srcW, tc.srcH, tc.cap), "-vf"); vf != tc.nvencVF {
			t.Errorf("%s %dx%d nvenc -vf %q, want %q", tc.rung, tc.srcW, tc.srcH, vf, tc.nvencVF)
		}
	}
}

func TestTranscodeBandwidthScalesWithTheFrame(t *testing.T) {
	cases := []struct {
		rung      string
		w, h      int
		withAudio bool
		want      int
	}{
		{"high", 1920, 1080, false, 6_500_000},
		{"high", 1280, 720, true, 2_888_888 + 160_000},
		{"high", 1920, 810, false, 4_875_000},
		{"medium", 1280, 720, true, 3_160_000},
		{"medium", 640, 360, true, 750_000 + 160_000},
		{"low", 0, 0, false, 1_400_000},
	}
	for _, tc := range cases {
		if got := transcodeBandwidth(tc.rung, tc.w, tc.h, tc.withAudio); got != tc.want {
			t.Errorf("%s %dx%d audio=%v: %d, want %d", tc.rung, tc.w, tc.h, tc.withAudio, got, tc.want)
		}
	}
}

func TestFallbackStreamInf(t *testing.T) {
	sdr720 := &Probe{VideoCodec: "h264", Width: 1280, Height: 720, BitRate: 2_500_000}
	hdr4K := &Probe{VideoCodec: "hevc", Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}
	cases := []struct {
		name    string
		p       *Probe
		rung    string
		cap     int
		useCopy bool
		group   string
		want    string
	}{
		{"copy at the source's size and bitrate", sdr720, "high", 0, true, "",
			`#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR`},
		{"copy of a 4K HDR HEVC without a known bitrate", hdr4K, "high", 0, true, "",
			`#EXT-X-STREAM-INF:BANDWIDTH=6500000,RESOLUTION=3840x2160,CODECS="hvc1.1.6.L120.B0,mp4a.40.2",VIDEO-RANGE=PQ`},
		{"high rung of a 720p source", sdr720, "high", 0, false, "aud",
			`#EXT-X-STREAM-INF:BANDWIDTH=3048888,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="aud"`},
		{"medium rung of 4K", hdr4K, "medium", 0, false, "aud",
			`#EXT-X-STREAM-INF:BANDWIDTH=3160000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="aud"`},
		{"high rung of 4K under a 720 HW cap, no audio", hdr4K, "high", 720, false, "",
			`#EXT-X-STREAM-INF:BANDWIDTH=2888888,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR`},
		{"unknown size: no RESOLUTION", &Probe{VideoCodec: "mpeg2video"}, "medium", 0, false, "aud",
			`#EXT-X-STREAM-INF:BANDWIDTH=3160000,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="aud"`},
	}
	for _, tc := range cases {
		if got := fallbackStreamInf(tc.p, QualityLadder[tc.rung], tc.cap, tc.useCopy, tc.group); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// probeJSON is an ffprobe answer for a w×h video (vcodec) with one audio
// track (acodec, ch channels) in container, at bitrate bit/s.
func probeJSON(container, vcodec string, w, h int, acodec string, ch int, bitrate int) string {
	return `{"streams":[{"codec_type":"video","codec_name":"` + vcodec + `","width":` + strconv.Itoa(w) +
		`,"height":` + strconv.Itoa(h) + `},{"codec_type":"audio","codec_name":"` + acodec +
		`","channels":` + strconv.Itoa(ch) + `}],"format":{"format_name":"` + container +
		`","duration":"600.0","bit_rate":"` + strconv.Itoa(bitrate) + `"}}`
}

// The fallback master advertises the variant the player gets, not a fixed
// 1920x1080 @ 6.5 Mbps.
func TestMasterAdvertisesWhatTheVariantIs(t *testing.T) {
	captureLog(t)
	const mp4 = "mov,mp4,m4a,3gp,3g2,mj2"
	cases := []struct {
		name, probe, query string
		want               []string
	}{
		{"remux of a 720p source runs the high rung at 720p",
			probeJSON(mp4, "h264", 1280, 720, "ac3", 6, 4_000_000), "",
			[]string{"BANDWIDTH=3048888,RESOLUTION=1280x720,", "\nhigh/index.m3u8\n"}},
		{"stream copy at the source's size and bitrate",
			probeJSON(mp4, "h264", 1280, 720, "aac", 2, 2_500_000), "",
			[]string{"BANDWIDTH=2500000,RESOLUTION=1280x720,", "\ncopy/index.m3u8\n"}},
		{"the medium rung does not upscale a 360p source",
			probeJSON("matroska,webm", "hevc", 640, 360, "aac", 2, 900_000), "?q=medium",
			[]string{"BANDWIDTH=910000,RESOLUTION=640x360,", "\nmedium/index.m3u8?q=medium\n"}},
		{"a device's 720p HW cap",
			probeJSON("matroska,webm", "hevc", 1920, 1080, "aac", 2, 8_000_000), "?caps=avc:720,aac",
			[]string{"BANDWIDTH=3048888,RESOLUTION=1280x720,", "\nhigh/index.m3u8?caps=avc:720,aac\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, src := mediaFile(t, "film.mp4")
			// Master starts a warm in the background; its ffmpeg fails at
			// once, and its cache lives outside the test's temp dirs so the
			// warm cannot race their cleanup.
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
			w := get(h, "/api/play/i1/master.m3u8"+tc.query)
			if w.Code != 200 {
				t.Fatalf("master: %d %q", w.Code, w.Body)
			}
			body := w.Body.String()
			for _, s := range tc.want {
				if !strings.Contains(body, s) {
					t.Errorf("master lacks %q:\n%s", s, body)
				}
			}
			if strings.Contains(body, "1920x1080") {
				t.Errorf("master still claims 1920x1080:\n%s", body)
			}
		})
	}
}

// With the ffmpeg on $PATH: the frame the window transcoder encodes is the
// RESOLUTION the master advertises for it.
func TestAdvertisedResolutionIsWhatFFmpegEncodes(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	if !allEncoders(t, ffmpeg)["libx264"] {
		t.Skip("this ffmpeg has no libx264")
	}
	root := t.TempDir()
	sources := map[string][2]int{"small.mkv": {640, 360}, "wide.mkv": {2560, 1080}}
	for name, size := range sources {
		run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi",
			"-i", "testsrc2=size="+strconv.Itoa(size[0])+"x"+strconv.Itoa(size[1])+":rate=24:duration=2",
			"-c:v", "mpeg4", filepath.Join(root, name))
	}
	cases := []struct {
		src  string
		rung string
		cap  int
	}{
		{"small.mkv", "medium", 0},
		{"small.mkv", "low", 0},
		{"wide.mkv", "high", 0},
		{"wide.mkv", "medium", 0},
		{"wide.mkv", "high", 720},
	}
	for _, tc := range cases {
		size := sources[tc.src]
		src := filepath.Join(root, tc.src)
		h := &HLSHandler{FFmpegBin: ffmpeg, FFprobeBin: ffprobe, CacheDir: t.TempDir(), TranscodePreset: "ultrafast"}
		ql := QualityLadder[tc.rung]
		probe := &Probe{Width: size[0], Height: size[1], DurationMs: 2000}
		if err := h.ensureVideoWindow(context.Background(), "i1", src, tc.rung, ql, false, probe, 0, 0, tc.cap); err != nil {
			t.Fatalf("%s %s: %v", tc.src, tc.rung, err)
		}
		q := videoCacheQuality(tc.rung, tc.cap)
		var media []byte
		for _, leaf := range []string{"init", "0"} {
			b, err := os.ReadFile(h.cachePath("i1", q, leaf))
			if err != nil {
				t.Fatal(err)
			}
			media = append(media, b...)
		}
		out := filepath.Join(t.TempDir(), "v.mp4")
		if err := os.WriteFile(out, media, 0o644); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Streams []struct{ Width, Height int } `json:"streams"`
		}
		if err := json.Unmarshal(run(t, ffprobe, "-v", "error", "-of", "json", "-show_streams", "-select_streams", "v:0", out), &got); err != nil || len(got.Streams) != 1 {
			t.Fatalf("probe %s: %v %+v", out, err, got)
		}
		g := transcodeGeometry(ql, size[0], size[1], tc.cap)
		if got.Streams[0].Width != g.w || got.Streams[0].Height != g.h {
			t.Errorf("%s on %s (cap %d): ffmpeg encoded %dx%d, the master advertises %dx%d",
				tc.rung, tc.src, tc.cap, got.Streams[0].Width, got.Streams[0].Height, g.w, g.h)
		}
	}
}

// allEncoders lists the names of all of ffmpeg's encoders.
func allEncoders(t *testing.T, ffmpeg string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(string(run(t, ffmpeg, "-hide_banner", "-encoders")), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && len(f[0]) == 6 && f[1] != "=" {
			out[f[1]] = true
		}
	}
	return out
}
