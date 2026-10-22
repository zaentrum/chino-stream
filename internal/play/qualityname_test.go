package play

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zaentrum/chino-stream/internal/streamcopy"
)

// The fixed table, at every boundary: a 16:9 frame of each height, and the
// height alone (a width not known), are named the same. 2160p and up is
// Ultra, 1080p High, 720p Medium, 480p Low, below 480p Lowest; a height
// between two steps takes the lower step's word with its own height, as do
// the heights past either end.
func TestQualityNameTable(t *testing.T) {
	cases := []struct {
		h    int
		want string
	}{
		{240, "Lowest (240p)"},
		{359, "Lowest (359p)"},
		{360, "Lowest (360p)"},
		{479, "Lowest (479p)"},
		{480, "Low (480p)"},
		{576, "Low (576p)"},
		{719, "Low (719p)"},
		{720, "Medium (720p)"},
		{900, "Medium (900p)"},
		{1079, "Medium (1079p)"},
		{1080, "High (1080p)"},
		{1440, "High (1440p)"},
		{2159, "High (2159p)"},
		{2160, "Ultra (2160p)"},
		{4320, "Ultra (4320p)"},
	}
	for _, tc := range cases {
		wide := (tc.h*16 + 4) / 9 // the 16:9 frame of the height
		for _, w := range []int{wide, 0} {
			if got := qualityName(w, tc.h, false); got != tc.want {
				t.Errorf("%dx%d: %q, want %q", w, tc.h, got, tc.want)
			}
		}
	}
}

// The original played as it is is "Original", whatever its size.
func TestQualityNameOfTheOriginal(t *testing.T) {
	for _, size := range [][2]int{{1920, 1080}, {3840, 2160}, {640, 360}, {0, 0}} {
		if got := qualityName(size[0], size[1], true); got != "Original" {
			t.Errorf("the original at %dx%d: %q", size[0], size[1], got)
		}
	}
}

// A frame is named by the height of the 16:9 frame it fills: a frame wider
// than 16:9 by the height of the 16:9 frame as wide, so a scope or a 1.85:1
// film is named as a 16:9 film of its width is. Up to 10% wider than a
// standard frame (DCI) counts as that frame, and so does up to 3% narrower
// (cropped at the sides). No height, no name.
func TestQualityNameOfFramesNot16By9(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		// 2.39:1, as the transcoder fits it into its boxes.
		{3840, 1606, "Ultra (2160p)"}, {1920, 804, "High (1080p)"}, {1920, 800, "High (1080p)"},
		{1280, 536, "Medium (720p)"}, {854, 358, "Low (480p)"}, {640, 268, "Lowest (360p)"},
		// 1.85:1.
		{1920, 1038, "High (1080p)"}, {1998, 1080, "High (1080p)"}, {1280, 692, "Medium (720p)"},
		// DCI.
		{4096, 2160, "Ultra (2160p)"}, {4096, 1716, "Ultra (2160p)"}, {2048, 1080, "High (1080p)"},
		{2048, 858, "High (1080p)"},
		// A few pixels cropped off the sides: the standard frame.
		{1916, 800, "High (1080p)"}, {1904, 800, "High (1080p)"}, {1912, 1036, "High (1080p)"},
		{1918, 1040, "High (1080p)"}, {3836, 1600, "Ultra (2160p)"}, {3816, 1600, "Ultra (2160p)"},
		{1278, 534, "Medium (720p)"}, {1276, 716, "Medium (720p)"},
		// More than 3% narrower: its own height, or the 16:9 frame as wide.
		{1856, 800, "Medium (1044p)"}, {1856, 1036, "Medium (1036p)"},
		// 4:3 and SD: the height.
		{1440, 1080, "High (1080p)"}, {960, 720, "Medium (720p)"}, {720, 576, "Low (576p)"},
		{720, 480, "Low (480p)"}, {640, 480, "Low (480p)"},
		// 21:9 and wider, between the steps.
		{2560, 1080, "High (1440p)"}, {1600, 900, "Medium (900p)"}, {1666, 900, "Medium (900p)"},
		{1728, 720, "Medium (972p)"}, {1152, 480, "Low (648p)"},
		// Past 2160p.
		{7680, 4320, "Ultra (4320p)"}, {8192, 4320, "Ultra (4320p)"},
		// No height.
		{0, 0, ""}, {1920, 0, ""}, {1920, -1, ""},
	}
	for _, tc := range cases {
		if got := qualityName(tc.w, tc.h, false); got != tc.want {
			t.Errorf("%dx%d: %q, want %q", tc.w, tc.h, got, tc.want)
		}
	}
}

// The on-the-fly ladder's rungs are named by the frame each encodes from the
// source, on ffmpeg and the live worker alike: high at the source's size (no
// "source" in its name - a 720p source's high is "Medium (720p)"), fitted
// into the 1080p box past it, under a device's HW cap; medium and low by the
// height they scale to (the lines they produce, a scope film's as a 16:9
// one's), and where the source is shorter at its frame, named as high is.
func TestOnTheFlyNames(t *testing.T) {
	cases := []struct {
		name           string
		w, h, cap      int
		high, med, low string
	}{
		{"1080p", 1920, 1080, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"720p: high at the source's size", 1280, 720, 0, "Medium (720p)", "Medium (720p)", "Low (480p)"},
		{"480p", 854, 480, 0, "Low (480p)", "Low (480p)", "Low (480p)"},
		{"360p", 640, 360, 0, "Lowest (360p)", "Lowest (360p)", "Lowest (360p)"},
		{"2160p: high fitted into 1080p", 3840, 2160, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"2160p on a 720p HW cap", 3840, 2160, 720, "Medium (720p)", "Medium (720p)", "Low (480p)"},
		{"1440p", 2560, 1440, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"900p: high at the source's size", 1600, 900, 0, "Medium (900p)", "Medium (720p)", "Low (480p)"},
		{"1.85:1 1080p", 1920, 1038, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		// medium and low scale to a height: wider than a 16:9 frame of it
		// (1728x720, 1152x480), named by it.
		{"2.39:1 1080p", 1920, 800, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"2.39:1 1080p on a 1080p HW cap", 1920, 800, 1080, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"2.39:1 1080p on a 720p HW cap", 1920, 800, 720, "Medium (720p)", "Medium (720p)", "Low (480p)"},
		{"2.39:1 2160p", 3840, 1606, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"2:1 1080p", 1920, 960, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		// medium is the source's own 1280x536 frame, named as high is.
		{"2.39:1 720p", 1280, 536, 0, "Medium (720p)", "Medium (720p)", "Low (480p)"},
		// A few pixels cropped off the sides: high named as the standard frame.
		{"2.39:1 1080p cropped", 1916, 800, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"1.85:1 1080p cropped", 1912, 1036, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"4:3 SD", 720, 576, 0, "Low (576p)", "Low (576p)", "Low (480p)"},
		{"size not known: the live worker's heights", 0, 0, 0, "High (1080p)", "Medium (720p)", "Low (480p)"},
		{"size not known, a 720p cap", 0, 0, 720, "Medium (720p)", "Medium (720p)", "Low (480p)"},
	}
	for _, tc := range cases {
		p := &Probe{Width: tc.w, Height: tc.h}
		got := []string{onTheFlyName(p, QualityLadder["high"], tc.cap), onTheFlyName(p, QualityLadder["medium"], tc.cap),
			onTheFlyName(p, QualityLadder["low"], tc.cap)}
		if want := []string{tc.high, tc.med, tc.low}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, want %q", tc.name, got, want)
		}
		// The name is of the frame the live worker is asked for: the one
		// ffmpeg encodes, with the size not known the rung's height.
		for _, q := range onTheFlyRungs {
			ql := QualityLadder[q]
			v := liveVideoRung(&source{probe: p}, ql, tc.cap)
			g := transcodeGeometry(ql, tc.w, tc.h, tc.cap)
			want := [2]int{g.w, g.h}
			if g.h <= 0 {
				want = [2]int{0, rungHeight(ql, tc.cap)}
			}
			if got := [2]int{v.Width, v.Height}; got != want {
				t.Errorf("%s %s: the live worker encodes %dx%d, ffmpeg %dx%d", tc.name, q, got[0], got[1], want[0], want[1])
			}
		}
	}
}

// /play/info's on-the-fly choice: each name once, the better encode's.
func TestOnTheFlyQualities(t *testing.T) {
	cases := []struct {
		w, h, cap int
		want      []string
	}{
		{1920, 1080, 0, []string{"high High (1080p)", "medium Medium (720p)", "low Low (480p)"}},
		{1280, 720, 0, []string{"high Medium (720p)", "low Low (480p)"}},
		{3840, 2160, 720, []string{"high Medium (720p)", "low Low (480p)"}},
		{640, 360, 0, []string{"high Lowest (360p)"}},
	}
	for _, tc := range cases {
		var got []string
		for _, e := range onTheFlyQualities(&Probe{Width: tc.w, Height: tc.h}, tc.cap) {
			got = append(got, e["name"]+" "+e["label"])
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%dx%d cap %d: %q, want %q", tc.w, tc.h, tc.cap, got, tc.want)
		}
	}
}

// nameVariants names each variant of a packaged master by its RESOLUTION:
// NAME added at the end of the line (before a CRLF line's CR) or set where
// the line has one; a variant without RESOLUTION and the I-frame playlists
// stay as they are, and so does every other line.
func TestNameVariants(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",URI=\"a0/playlist.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=6000000,CODECS=\"hvc1.1.6.L150.90\",RESOLUTION=3840x1606,AUDIO=\"audio\"\nv0/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,NAME=\"720p\",RESOLUTION=1280x720,CODECS=\"avc1.64001f\"\nv1/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480,CODECS=\"avc1.64001e\"\r\nv2/playlist.m3u8\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"avc1.64001e\"\nv3/playlist.m3u8\n" +
		"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=40000,RESOLUTION=1280x720,URI=\"v1/iframes.m3u8\"\n"
	want := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",URI=\"a0/playlist.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=6000000,CODECS=\"hvc1.1.6.L150.90\",RESOLUTION=3840x1606,AUDIO=\"audio\",NAME=\"Ultra (2160p)\"\nv0/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,NAME=\"Medium (720p)\",RESOLUTION=1280x720,CODECS=\"avc1.64001f\"\nv1/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480,CODECS=\"avc1.64001e\",NAME=\"Low (480p)\"\r\nv2/playlist.m3u8\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"avc1.64001e\"\nv3/playlist.m3u8\n" +
		"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=40000,RESOLUTION=1280x720,URI=\"v1/iframes.m3u8\"\n"
	if got := nameVariants(body); got != want {
		t.Errorf("named:\n%s\nwant:\n%s", got, want)
	}
	if got := nameVariants(want); got != want {
		t.Errorf("named twice:\n%s", got)
	}
}

// variantNames maps each variant of a master to its NAME, by its URI's
// rung ("v0", "high", "copy", "video"), "" for a variant without one; the
// I-frame playlists that carry a NAME are listed under "iframes".
func variantNames(t *testing.T, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:") && i+1 < len(lines):
			rung := firstPathSegment(strings.TrimSpace(lines[i+1]))
			name := hlsAttributes(l)["NAME"]
			if prev, ok := out[rung]; ok && prev != name {
				t.Errorf("rung %s named %q and %q", rung, prev, name)
			}
			out[rung] = name
		case strings.HasPrefix(l, "#EXT-X-I-FRAME-STREAM-INF:") && hlsAttributes(l)["NAME"] != "":
			out["iframes"] = hlsAttributes(l)["NAME"]
		}
	}
	return out
}

// A packaged master names every variant it serves by its rung's frame, for
// every client and pick - the stereo group's variants and the 5.1 group's,
// the union's, a native player's mirror, a single-rendition package's - as
// /play/info's qualities label that rung. The I-frame playlists are no
// quality and stay unnamed.
func TestPackagedMasterNamesEveryVariant(t *testing.T) {
	packages := usePackages(t, filepath.Join("testdata", "packages"))
	h := &HLSHandler{Packages: packages}
	ladder := map[string]string{"v0": "High (1080p)", "v1": "Medium (720p)", "v2": "Low (480p)"}
	uhd := map[string]string{"v0": "Ultra (2160p)", "v1": "High (1080p)", "v2": "Medium (720p)"}
	cases := []struct {
		pkg, query string
		want       map[string]string
	}{
		{pkgLadder, "?caps=avc,aac", map[string]string{"v1": ladder["v1"], "v2": ladder["v2"]}},
		{pkgLadder, "?caps=avc,hvc,aac", map[string]string{"v0": ladder["v0"]}},
		{pkgLadder, "?caps=avc,hvc,aac&q=v2", map[string]string{"v2": ladder["v2"]}},
		{pkgLadder, "?caps=avc,hvc,aac,eac3", map[string]string{"v0": ladder["v0"]}},
		{pkgLadder, "?caps=avc,hvc,aac,eac3,native", map[string]string{"v0": ladder["v0"]}},
		{pkgUHD, "?caps=avc:2160,hvc:2160,aac", map[string]string{"v0": uhd["v0"]}},
		{pkgUHD, "?caps=avc:1080,hvc:1080,aac", map[string]string{"v1": uhd["v1"], "v2": uhd["v2"]}},
		{pkgLegacy, "?caps=avc,hvc,aac", map[string]string{"v0": "High (1080p)"}},
	}
	for _, tc := range cases {
		w := get(h, "/api/play/"+tc.pkg+"/master.m3u8"+tc.query)
		if w.Code != 200 {
			t.Fatalf("%s%s: %d %q", tc.pkg, tc.query, w.Code, w.Body)
		}
		if got := variantNames(t, w.Body.String()); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s%s: names %v, want %v:\n%s", tc.pkg, tc.query, got, tc.want, w.Body)
		}
	}
	// /play/info labels each rung as its own master names it.
	for _, tc := range []struct{ pkg, caps string }{{pkgLadder, "avc,hvc,aac"}, {pkgUHD, "avc:2160,hvc:2160,aac"}} {
		info, _ := getInfo(t, packages, tc.pkg, "?caps="+tc.caps)
		for _, e := range info.Qualities[1:] {
			rung := e["name"].(string)
			names := variantNames(t, get(h, "/api/play/"+tc.pkg+"/master.m3u8?caps="+tc.caps+"&q="+rung).Body.String())
			if names[rung] != e["label"] {
				t.Errorf("%s %s: /play/info says %q, its master %q", tc.pkg, rung, e["label"], names[rung])
			}
		}
	}
}

// An extra's master names its variants as a title's does.
func TestExtraMasterNamesItsVariants(t *testing.T) {
	h := &HLSHandler{Packages: usePackages(t, filepath.Join("testdata", "packages")),
		CacheDir: t.TempDir(), FFmpegBin: fakeBin(t, "ffmpeg", "exit 1")}
	captureLog(t)
	w := get(h, extraURL(pkgStereoLadder, extraTrailer, "master.m3u8?caps=avc,aac"))
	if w.Code != 200 {
		t.Fatalf("master: %d %q", w.Code, w.Body)
	}
	if got, want := variantNames(t, w.Body.String()), map[string]string{"v0": "Medium (720p)", "v1": "Low (480p)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names %v, want %v:\n%s", got, want, w.Body)
	}
}

// onTheFlyHandlers are the master's and /play/info's handlers for an
// original (an empty file named file) the probe describes, its ffmpeg one that fails at once (a warm
// the master starts finds nothing), its cache outside the test's temp dirs
// so a warm cannot race their cleanup.
func onTheFlyHandlers(t *testing.T, file, probe string) (*HLSHandler, *Handler) {
	t.Helper()
	root, src := mediaFile(t, file)
	cache, err := os.MkdirTemp("", "chino-stream-names-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	h := &HLSHandler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"),
		FFprobeBin: probeFFprobe(t, probe), CacheDir: cache}
	return h, &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: h.FFprobeBin, CacheDir: cache, HLS: h}
}

// On the fly, /play/info's qualities are the ladder's rungs by the frame
// each encodes for this source and client - each name once - and the master
// a client asks with an entry's name names its variant that entry's label,
// a rung the list leaves out (it plays what one above it does) named as the
// one above. A stream copy (passthrough) offers no choice, as before, and its
// variant is "Original".
func TestOnTheFlyInfoAndMasterNameTheRungs(t *testing.T) {
	captureLog(t)
	const mkv = "matroska,webm"
	cases := []struct {
		name, probe, caps string
		mode              string
		qualities         []string
		medium            string // the NAME of q=medium's variant
	}{
		{"a 1080p transcode", probeJSON(mkv, "hevc", 1920, 1080, "aac", 2, 8_000_000), "avc,aac", "transcode",
			[]string{"high High (1080p)", "medium Medium (720p)", "low Low (480p)"}, "Medium (720p)"},
		{"a 720p transcode: high at its size", probeJSON(mkv, "hevc", 1280, 720, "aac", 2, 4_000_000), "avc,aac", "transcode",
			[]string{"high Medium (720p)", "low Low (480p)"}, "Medium (720p)"},
		{"4K on a 720p HW cap", probeJSON(mkv, "hevc", 3840, 2160, "aac", 2, 20_000_000), "avc:720,aac", "transcode",
			[]string{"high Medium (720p)", "low Low (480p)"}, "Medium (720p)"},
		{"a 2.39:1 transcode", probeJSON(mkv, "hevc", 1920, 800, "aac", 2, 6_000_000), "avc,aac", "transcode",
			[]string{"high High (1080p)", "medium Medium (720p)", "low Low (480p)"}, "Medium (720p)"},
		{"a 2.39:1 transcode on a 720p HW cap", probeJSON(mkv, "hevc", 1920, 800, "aac", 2, 6_000_000), "avc:720,aac", "transcode",
			[]string{"high Medium (720p)", "low Low (480p)"}, "Medium (720p)"},
		{"a 360p transcode", probeJSON(mkv, "hevc", 640, 360, "aac", 2, 900_000), "avc,aac", "transcode",
			[]string{"high Lowest (360p)"}, "Lowest (360p)"},
		{"a stream copy", probeJSON("mov,mp4,m4a,3gp,3g2,mj2", "h264", 1920, 1080, "aac", 2, 8_000_000), "avc,aac", "passthrough",
			nil, "Medium (720p)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := "film.mkv"
			if tc.mode == "passthrough" {
				file = "film.mp4"
			}
			h, ph := onTheFlyHandlers(t, file, tc.probe)
			w := getPlay(ph, "/api/play/i1/info?caps="+tc.caps)
			var info struct {
				Mode      string              `json:"mode"`
				Qualities []map[string]string `json:"qualities"`
				Default   string              `json:"default_quality"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || w.Code != 200 {
				t.Fatalf("info: %d %q", w.Code, w.Body)
			}
			var got []string
			for _, e := range info.Qualities {
				got = append(got, e["name"]+" "+e["label"])
			}
			if info.Mode != tc.mode || info.Default != "high" || !reflect.DeepEqual(got, tc.qualities) {
				t.Errorf("info: mode %q default %q qualities %q, want %s %q", info.Mode, info.Default, got, tc.mode, tc.qualities)
			}
			for _, e := range info.Qualities {
				m := get(h, "/api/play/i1/master.m3u8?caps="+tc.caps+"&q="+e["name"])
				if names := variantNames(t, m.Body.String()); names[e["name"]] != e["label"] || len(names) != 1 {
					t.Errorf("q=%s: the master names %v, /play/info %q:\n%s", e["name"], names, e["label"], m.Body)
				}
			}
			m := get(h, "/api/play/i1/master.m3u8?caps="+tc.caps+"&q=medium")
			if names := variantNames(t, m.Body.String()); names["medium"] != tc.medium {
				t.Errorf("q=medium: %v, want %q:\n%s", names, tc.medium, m.Body)
			}
			if tc.mode == "passthrough" {
				m := get(h, "/api/play/i1/master.m3u8?caps="+tc.caps)
				if names := variantNames(t, m.Body.String()); !reflect.DeepEqual(names, map[string]string{"copy": "Original"}) {
					t.Errorf("the stream copy's master names %v:\n%s", names, m.Body)
				}
			}
		})
	}
}

// The live worker's sessions name their rung as ffmpeg's do: the master is
// the same but for the pin.
func TestLiveMasterNamesTheRung(t *testing.T) {
	captureLog(t)
	inPlaceWarms(t)
	fw := newFakeWorker(t)
	on, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
	for query, want := range map[string]map[string]string{
		"?caps=avc,aac":          {"high": "High (1080p)"},
		"?caps=avc,aac&q=medium": {"medium": "Medium (720p)"},
		"?caps=avc:720,aac":      {"high": "Medium (720p)"},
		"?caps=avc,aac&q=low":    {"low": "Low (480p)"},
	} {
		w := get(on, "/api/play/i1/master.m3u8"+query)
		if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "lw=1") {
			t.Fatalf("%s: not a live session: %d\n%s", query, w.Code, body)
		}
		if got := variantNames(t, w.Body.String()); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v:\n%s", query, got, want, w.Body)
		}
	}
	settle(t, on.Live)
}

// The stream copy's variant (the remux path's: the video copied, its audio
// copied or converted) is "Original".
func TestCopyStreamInfIsTheOriginal(t *testing.T) {
	ix := &streamcopy.Index{Video: streamcopy.VideoTrack{Codec: "hvc1.2.4.L153.B0", Width: 3840, Height: 2160}}
	for _, choices := range [][]audioChoice{nil, {{rendition: "audiocopy0", codec: "ec-3", channels: 6}}} {
		line := copyStreamInf(ix, &Probe{ColorTransfer: "smpte2084"}, Caps{}, onTheFlyAudioGroup, choices)
		if a := hlsAttributes(line); a["NAME"] != "Original" || a["RESOLUTION"] != "3840x2160" || a["VIDEO-RANGE"] != "PQ" {
			t.Errorf("%s", line)
		}
	}
}
