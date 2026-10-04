package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
)

// playInfo is the /play/info answer for a packaged item, as a client reads
// it.
type playInfo struct {
	Mode           string           `json:"mode"`
	VideoCodec     string           `json:"video_codec"`
	Width          int              `json:"width"`
	Height         int              `json:"height"`
	Qualities      []map[string]any `json:"qualities"`
	DefaultQuality string           `json:"default_quality"`
	AudioTracks    []map[string]any `json:"audio_tracks"`
}

// getInfo answers GET /api/play/{id}/info?{query} for a packaged item.
func getInfo(t *testing.T, id, query string) (playInfo, map[string]any) {
	t.Helper()
	root, src := mediaFile(t, "film.mkv")
	h := &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root}
	r := chi.NewRouter()
	r.Get("/api/play/{itemId}/info", h.Info)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/play/"+id+"/info"+query, nil))
	if w.Code != 200 {
		t.Fatalf("info: %d %q", w.Code, w.Body)
	}
	var info playInfo
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return info, raw
}

// qualityNames lists the names (?q= values) and labels of a quality list.
func qualityNames(qs []map[string]any) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q["name"].(string)+" "+q["label"].(string))
	}
	return out
}

// /play/info offers a ladder package's quality choice: Auto, then the
// rungs the client decodes at its heights, tallest first, across codec
// families (a pick loads that rung's own master). Its video fields are the
// rung the client starts on; default_quality is auto.
func TestPlayInfoListsTheRungsAClientMayPick(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	cases := []struct {
		name, pkg, query string
		qualities        []string
		codec            string
		w, h             int
	}{
		{"H.264 browser", pkgLadder, "?caps=avc,aac",
			[]string{"auto Auto", "v1 720p", "v2 480p"}, "avc1.64001f", 1280, 720},
		{"HEVC browser: its ladder is the HEVC rung, the H.264 ones are picks", pkgLadder, "?caps=avc,hvc,aac",
			[]string{"auto Auto", "v0 1080p", "v1 720p", "v2 480p"}, "hvc1.1.6.L120.90", 1920, 1080},
		{"a pick: the video fields are the rung picked", pkgLadder, "?caps=avc,hvc,aac&q=v2",
			[]string{"auto Auto", "v0 1080p", "v1 720p", "v2 480p"}, "avc1.64001e", 854, 480},
		{"4K over a 1080 HEVC cap", pkgUHD, "?caps=avc:1080,hvc:1080,aac",
			[]string{"auto Auto", "v1 1080p", "v2 720p"}, "avc1.640028", 1920, 1080},
		{"4K TV", pkgUHD, "?caps=avc:2160,hvc:2160,aac,eac3",
			[]string{"auto Auto", "v0 2160p", "v1 1080p", "v2 720p"}, "hvc1.1.6.L150.90", 3840, 2160},
		{"two HEVC rungs", pkgHEVCLadder, "?caps=hvc,avc,aac",
			[]string{"auto Auto", "v0 1080p", "v1 720p", "v2 480p"}, "hvc1.1.6.L120.90", 1920, 1080},
		{"one rung it may pick: no choice", pkgHEVCLadder, "?caps=avc,aac",
			nil, "avc1.64001e", 854, 480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, raw := getInfo(t, tc.pkg, tc.query)
			if info.Mode != "packaged" || info.DefaultQuality != "auto" {
				t.Errorf("mode %q default_quality %q", info.Mode, info.DefaultQuality)
			}
			if got := qualityNames(info.Qualities); !reflect.DeepEqual(got, tc.qualities) {
				t.Errorf("qualities %v, want %v", got, tc.qualities)
			}
			if info.VideoCodec != tc.codec || info.Width != tc.w || info.Height != tc.h {
				t.Errorf("video %s %dx%d, want %s %dx%d", info.VideoCodec, info.Width, info.Height, tc.codec, tc.w, tc.h)
			}
			if tc.qualities == nil && raw["qualities"] != nil {
				t.Errorf("qualities %v, want null", raw["qualities"])
			}
		})
	}
}

// Each rung entry: name and id the rung id (name is what the TV, mobile
// and Tizen menus send as ?q=, a required field there), the label, the
// frame, the codec, the bit rate of its variant, the video range. Auto has
// a name and a label only.
func TestPlayInfoQualityEntries(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	info, _ := getInfo(t, pkgLadder, "?caps=avc,hvc,aac")
	want := []map[string]any{
		{"name": "auto", "label": "Auto"},
		{"name": "v0", "id": "v0", "label": "1080p", "width": 1920.0, "height": 1080.0,
			"codec": "hvc1.1.6.L120.90", "bitrate": 7436924.0, "video_range": "SDR"},
		{"name": "v1", "id": "v1", "label": "720p", "width": 1280.0, "height": 720.0,
			"codec": "avc1.64001f", "bitrate": 1505267.0, "video_range": "SDR"},
		{"name": "v2", "id": "v2", "label": "480p", "width": 854.0, "height": 480.0,
			"codec": "avc1.64001e", "bitrate": 706649.0, "video_range": "SDR"},
	}
	if !reflect.DeepEqual(info.Qualities, want) {
		t.Errorf("qualities\n got %v\nwant %v", info.Qualities, want)
	}
}

// A package with one rendition offers no choice, as before (qualities
// null); its video fields are that rendition's from the manifest.
func TestPlayInfoOfASingleRenditionPackage(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	for _, q := range []string{"", "&q=v0", "&q=high"} {
		info, raw := getInfo(t, pkgLegacy, "?caps=avc,hvc,aac"+q)
		if raw["qualities"] != nil || info.DefaultQuality != "auto" {
			t.Errorf("q=%q: qualities %v, default_quality %q", q, raw["qualities"], info.DefaultQuality)
		}
		if info.VideoCodec != "hev1.1.6.L120.B0" || info.Width != 1920 || info.Height != 1080 || len(info.AudioTracks) != 2 {
			t.Errorf("q=%q: %+v", q, info)
		}
	}
}

// Of two rungs of one size, the one in the family the client's ladder is
// in: an HEVC client's 1080p is the HEVC rung, an H.264 client's the
// H.264 one.
func TestPlayInfoOneEntryPerSize(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=6000000,CODECS=\"hvc1.1.6.L120.90,mp4a.40.2\",RESOLUTION=1920x1080\nv0/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080\nv1/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720\nv2/playlist.m3u8\n"
	if got := qualityNames(packagedQualities(body, ParseCaps("avc,hvc,aac"))); !reflect.DeepEqual(got,
		[]string{"auto Auto", "v0 1080p", "v2 720p"}) {
		t.Errorf("HEVC client: %v", got)
	}
	if got := qualityNames(packagedQualities(body, ParseCaps("avc,aac"))); !reflect.DeepEqual(got,
		[]string{"auto Auto", "v1 1080p", "v2 720p"}) {
		t.Errorf("H.264 client: %v", got)
	}
	// Whatever the master's order: an H.264 1080p listed before the HEVC
	// one is still not an HEVC client's 1080p.
	h264First := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS=\"avc1.640028\",RESOLUTION=1920x1080\nv0/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=6000000,CODECS=\"hvc1.1.6.L120.90\",RESOLUTION=1920x1080\nv1/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS=\"avc1.64001f\",RESOLUTION=1280x720\nv2/playlist.m3u8\n"
	if got := qualityNames(packagedQualities(h264First, ParseCaps("avc,hvc"))); !reflect.DeepEqual(got,
		[]string{"auto Auto", "v1 1080p", "v2 720p"}) {
		t.Errorf("H.264 listed first, HEVC client: %v", got)
	}
	// Listed tallest first whatever the master's order.
	reordered := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS=\"avc1.64001f\",RESOLUTION=1280x720\nv2/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS=\"avc1.640028\",RESOLUTION=1920x1080\nv1/playlist.m3u8\n"
	if got := qualityNames(packagedQualities(reordered, ParseCaps("avc"))); !reflect.DeepEqual(got,
		[]string{"auto Auto", "v1 1080p", "v2 720p"}) {
		t.Errorf("reordered: %v", got)
	}
}

// A rung is named for its picture size the way the ladder names rungs:
// the smallest 16:9 box that holds it.
func TestRungLabel(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{1920, 1080, "1080p"}, {1280, 720, "720p"}, {854, 480, "480p"}, {640, 360, "360p"},
		{426, 240, "240p"}, {3840, 2160, "2160p"},
		{3840, 1606, "2160p"}, {1920, 804, "1080p"}, {1280, 536, "720p"}, // 2.39:1 rungs
		{1440, 1080, "1080p"}, {720, 576, "576p"}, {720, 480, "480p"}, // 4:3, SD
		{4096, 2160, "2160p"}, {2048, 858, "1080p"}, // DCI
		{2560, 1080, "1440p"}, {7680, 4320, "4320p"}, {8192, 4320, "4320p"}, {9000, 5000, "5000p"},
		{0, 1080, "1080p"}, {0, 0, "v3"},
	}
	for _, tc := range cases {
		if got := rungLabel(ladderRung{id: "v3", width: tc.w, height: tc.h}); got != tc.want {
			t.Errorf("%dx%d: %s, want %s", tc.w, tc.h, got, tc.want)
		}
	}
}
