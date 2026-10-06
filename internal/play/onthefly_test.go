package play

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// noAsset is a katalog-api whose /asset knows no file: every original is
// retired (katalog-api answers /asset only with an item's file, never its
// package or a retired original).
func noAsset(t *testing.T) *catalog.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return catalog.New(srv.URL)
}

// retiredLibrary is the staged library (stageLibrary) with libItem's
// original retired, and handlers serving it with ffmpeg the fake at
// ffmpegBin.
func retiredLibrary(t *testing.T, ffmpegBin string) (*library, *HLSHandler, *Handler) {
	t.Helper()
	l := stageLibrary(t)
	cache := t.TempDir()
	hls := &HLSHandler{Catalog: noAsset(t), Packages: l.packages, FFmpegBin: ffmpegBin, CacheDir: cache, TranscodePreset: "veryfast"}
	play := &Handler{Catalog: hls.Catalog, Packages: l.packages, FFmpegBin: ffmpegBin, CacheDir: cache}
	return l, hls, play
}

// getPlay runs one GET against the play handler's routes, as the router
// mounts them.
func getPlay(h *Handler, path string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/api/play/{itemId}", h.Play)
	r.Get("/api/play/{itemId}/info", h.Info)
	r.Get("/api/play/{itemId}/subtitles/{streamIndex}.vtt", h.EmbeddedSubtitle)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// A title whose original is retired and whose package (HEVC only) its
// client decodes none of is transcoded on the fly from the package: the
// master of the on-the-fly ladder, its audio renditions the package's stereo
// ones, every URI pinned to the version read; each window an ffmpeg run on
// the window's segments of the package's top video rendition (or of one
// audio rendition, its one track 0:a:0), read as concat: of its init and
// segments with -seek_timestamp 1, never through the HLS demuxer; the
// transcode cached per version. Nothing of it is stream-copied (/copy/ is
// 404) and the progressive stream needs the original.
func TestATitleWithoutItsOriginalPlaysOnTheFlyFromItsPackage(t *testing.T) {
	state := t.TempDir()
	l, h, ph := retiredLibrary(t, windowFFmpeg(t, state, 0, 4, ""))
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,aac"
	pinned := query + "&v=" + libNew
	url := func(route string) string { return "/api/play/" + libItem + "/" + route }

	w := get(h, url("master.m3u8"+query))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "\nhigh/index.m3u8"+pinned+"\n") {
		t.Fatalf("master: %d\n%s", w.Code, body)
	}
	for _, rend := range []string{
		`NAME="English",LANGUAGE="eng",DEFAULT=YES,AUTOSELECT=YES,URI="audio/0/index.m3u8` + pinned + `"`,
		`NAME="German",LANGUAGE="ger",DEFAULT=NO,AUTOSELECT=YES,URI="audio/1/index.m3u8` + pinned + `"`,
	} {
		if !strings.Contains(body, rend) {
			t.Errorf("master lacks %s:\n%s", rend, body)
		}
	}
	if strings.Contains(body, "copy/") || !strings.Contains(body, `CODECS="avc1.640028,mp4a.40.2"`) || !strings.Contains(body, "RESOLUTION=1920x1080") {
		t.Errorf("want the H.264 transcode of the 1080p package:\n%s", body)
	}

	// The media playlist: the package's duration (20.021 s) in 6 s segments.
	w = get(h, url("high/index.m3u8"+pinned))
	if w.Code != 200 || strings.Count(w.Body.String(), "#EXTINF:6.000,") != 3 || !strings.Contains(w.Body.String(), "#EXTINF:2.021,\n3.m4s"+pinned) {
		t.Errorf("playlist: %d\n%s", w.Code, w.Body)
	}

	// A segment: one ffmpeg run of the window, on the package's v0.
	if w := get(h, url("high/2.m4s"+pinned)); w.Code != 200 || w.Body.String() != "seg" {
		t.Fatalf("segment: %d %q", w.Code, w.Body)
	}
	v := runWith(t, state, "0:v:0")
	dir := filepath.Join(l.version(libNew), "hls", "v0")
	wantInput := "concat:" + strings.Join([]string{filepath.Join(dir, "init.mp4"), filepath.Join(dir, "seg-00001.m4s"),
		filepath.Join(dir, "seg-00002.m4s"), filepath.Join(dir, "seg-00003.m4s"), filepath.Join(dir, "seg-00004.m4s")}, "|")
	if argAfter(v, "-i") != wantInput || argAfter(v, "-seek_timestamp") != "1" || argAfter(v, "-ss") != "0" || argAfter(v, "-map") != "0:v:0" {
		t.Errorf("video window args: -i %q -seek_timestamp %q -ss %q -map %q", argAfter(v, "-i"), argAfter(v, "-seek_timestamp"), argAfter(v, "-ss"), argAfter(v, "-map"))
	}
	if argAfter(v, "-c:v") != "libx264" || argAfter(v, "-output_ts_offset") != "0" {
		t.Errorf("video window encodes %q at %q", argAfter(v, "-c:v"), argAfter(v, "-output_ts_offset"))
	}

	// The second audio rendition: its one track, from its own folder.
	if w := get(h, url("audio/1/init.mp4"+pinned)); w.Code != 200 {
		t.Fatalf("audio init: %d %q", w.Code, w.Body)
	}
	audio := runReading(t, state, filepath.Join(l.version(libNew), "hls", "a1", "init.mp4"))
	if in := argAfter(audio, "-i"); !strings.HasPrefix(in, "concat:"+filepath.Join(l.version(libNew), "hls", "a1", "init.mp4")+"|") ||
		argAfter(audio, "-map") != "0:a:0" || argAfter(audio, "-c:a") == "" {
		t.Errorf("audio window args: -i %q -map %q", in, argAfter(audio, "-map"))
	}
	if w := get(h, url("audio/9/init.mp4"+pinned)); w.Code == 200 {
		t.Errorf("an audio rendition the package has not: %d", w.Code)
	}

	// Cached per version: the item's own key is the original's.
	if !statOK(h.cachePath(libItem+"@"+libNew, "high", "init")) || statOK(h.cachePath(libItem, "high", "init")) {
		t.Errorf("the transcode is not cached under the version's key")
	}

	// Nothing of a package is stream-copied; the progressive stream needs
	// the original.
	for _, route := range []string{"copy/index.m3u8", "copy/init.mp4", "copy/0.m4s"} {
		if w := get(h, url(route+query)); w.Code != 404 || !strings.Contains(w.Body.String(), "stream copy needs the original") {
			t.Errorf("%s: %d %q", route, w.Code, w.Body)
		}
	}
	if w := getPlay(ph, url("")[:len(url(""))-1]); w.Code != 404 || !strings.Contains(w.Body.String(), "progressive playback needs the original") {
		t.Errorf("progressive: %d %q", w.Code, w.Body)
	}

	// /play/info says what the client gets.
	w = getPlay(ph, url("info"+query))
	var info map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("info: %d %q", w.Code, w.Body)
	}
	if info["mode"] != "transcode" || !strings.Contains(info["reason"].(string), "the original is retired") ||
		info["video_codec"] != "hevc" || info["filename"] != "Clip (Remastered)" || info["default_quality"] != "high" ||
		len(info["qualities"].([]any)) != 3 || len(info["audio_tracks"].([]any)) != 2 {
		t.Errorf("info %v", info)
	}
	if names := names(t, toMaps(info["audio_tracks"])); !reflect.DeepEqual(names, []string{"English", "German"}) {
		t.Errorf("audio track names %v", names)
	}
}

// runWith is the last ffmpeg run of the fake at state that maps stream.
func runWith(t *testing.T, state, stream string) []string {
	t.Helper()
	runs := ffmpegRuns(t, state)
	for i := len(runs) - 1; i >= 0; i-- {
		if argAfter(runs[i], "-map") == stream {
			return runs[i]
		}
	}
	t.Fatalf("no ffmpeg run maps %s", stream)
	return nil
}

// runReading is the last ffmpeg run of the fake at state whose input reads
// file.
func runReading(t *testing.T, state, file string) []string {
	t.Helper()
	runs := ffmpegRuns(t, state)
	for i := len(runs) - 1; i >= 0; i-- {
		if strings.Contains(argAfter(runs[i], "-i"), file) {
			return runs[i]
		}
	}
	t.Fatalf("no ffmpeg run reads %s", file)
	return nil
}

func toMaps(v any) []map[string]any {
	var out []map[string]any
	for _, e := range v.([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// An HEVC client over the package's height is transcoded on the fly too —
// never stream-copied, whatever DecideWith would say of the HEVC.
func TestAPackageOverTheClientsHeightIsTranscodedNotCopied(t *testing.T) {
	_, h, ph := retiredLibrary(t, windowFFmpeg(t, t.TempDir(), 0, 4, ""))
	const query = "?caps=hvc:720,avc:720,aac"
	w := get(h, "/api/play/"+libItem+"/master.m3u8"+query)
	if w.Code != 200 || strings.Contains(w.Body.String(), "copy/") || !strings.Contains(w.Body.String(), "\nhigh/index.m3u8") ||
		!strings.Contains(w.Body.String(), "RESOLUTION=1280x720") {
		t.Fatalf("master: %d\n%s", w.Code, w.Body)
	}
	var info map[string]any
	_ = json.Unmarshal(getPlay(ph, "/api/play/"+libItem+"/info"+query).Body.Bytes(), &info)
	if info["mode"] != "transcode" || !strings.Contains(info["reason"].(string), "over the client's height") {
		t.Errorf("info %v", info)
	}
}

// An embedded subtitle of a title whose original is retired is the
// package's subtitle of that stream (sub<N>), its WebVTT file extracted
// through the same ffmpeg pipeline from ?t= on; one the package has not, or
// has in a format that is not WebVTT, is 404.
func TestAnEmbeddedSubtitleOfARetiredOriginalIsThePackages(t *testing.T) {
	state := t.TempDir()
	ffmpeg := fakeBin(t, "ffmpeg", `
for a; do printf '%s\n' "$a"; done >> '`+state+`/args'
printf '\n' >> '`+state+`/args'
printf 'WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello.\n'`)
	l, _, ph := retiredLibrary(t, ffmpeg)
	if err := os.MkdirAll(filepath.Join(l.version(libNew), "subs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.version(libNew), "subs", "2.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := getPlay(ph, "/api/play/"+libItem+"/subtitles/2.vtt?t=13")
	if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "WEBVTT") {
		t.Fatalf("subtitle: %d %q", w.Code, w.Body)
	}
	run := ffmpegRuns(t, state)[0]
	if argAfter(run, "-i") != filepath.Join(l.version(libNew), "subs", "2.vtt") || argAfter(run, "-map") != "0:s:0" || argAfter(run, "-ss") != "13" {
		t.Errorf("args %v", run)
	}
	if w := getPlay(ph, "/api/play/"+libItem+"/subtitles/7.vtt"); w.Code != 404 {
		t.Errorf("a stream the package has no subtitle of: %d", w.Code)
	}
}

// A title whose original is retired and whose package katalog-api names
// but the storage does not have complete cannot be played: its master,
// /play/info and the progressive stream say so (404); only a backup brings
// it back. A title that never had a package and whose original is gone is
// 404 as it always was.
func TestATitleWithoutOriginalOrCompletePackageIsNotPlayable(t *testing.T) {
	l, h, ph := retiredLibrary(t, windowFFmpeg(t, t.TempDir(), 0, 4, ""))
	for _, v := range []string{libNew, libOld} {
		if err := os.Remove(filepath.Join(l.version(v), ".complete")); err != nil {
			t.Fatal(err)
		}
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"master":      get(h, "/api/play/"+libItem+"/master.m3u8?caps=avc,aac"),
		"segment":     get(h, "/api/play/"+libItem+"/high/0.m4s"),
		"info":        getPlay(ph, "/api/play/"+libItem+"/info?caps=avc,aac"),
		"progressive": getPlay(ph, "/api/play/"+libItem),
	} {
		if w.Code != 404 || !strings.Contains(w.Body.String(), "not playable: the original was retired and its package is missing") {
			t.Errorf("%s: %d %q", name, w.Code, w.Body)
		}
	}
	l.katalog.setItem(catalog.Playback{ItemID: "0a0a0a0a-0000-4000-8000-000000000004"})
	if w := get(h, "/api/play/0a0a0a0a-0000-4000-8000-000000000004/master.m3u8"); w.Code != 404 || strings.Contains(w.Body.String(), "not playable") {
		t.Errorf("no package, no original: %d %q", w.Code, w.Body)
	}
}

// A window's input is its rendition's init and the segments that cover it,
// from the one holding its start less a second (a playlist's EXTINFs run
// ahead of the media by the reordering delay) to the one holding its end;
// a playlist naming anything but files of its own folder is refused.
func TestPackageWindowPicksTheSegmentsThatCoverIt(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		renditions.Delete(filepath.Join(dir, "playlist.m3u8"))
		packagedCache.Delete(filepath.Join(dir, "playlist.m3u8"))
	}
	// Ten segments of 10.427 s, the first 10.26 s (shaka's, of a 250-frame GOP).
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:11\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:10.260,\nseg-00001.m4s\n")
	for i := 2; i <= 10; i++ {
		b.WriteString("#EXTINF:10.427,\nseg-" + strings.Repeat("0", 5-len(itoa(i))) + itoa(i) + ".m4s\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	write(b.String())
	segs := func(url string) []string {
		var out []string
		for _, f := range strings.Split(strings.TrimPrefix(url, "concat:"), "|")[1:] {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "seg-000"), ".m4s"))
		}
		return out
	}
	for _, tc := range []struct {
		start, dur float64
		want       []string
	}{
		{0, 60, []string{"01", "02", "03", "04", "05", "06"}},
		{60, 60, []string{"06", "07", "08", "09", "10", "11"}[:5]},
		{10.5, 6, []string{"01", "02"}}, // a second after seg 2's EXTINF start: seg 1 too
		{12, 6, []string{"02"}},
		{95, 60, []string{"10"}},
	} {
		url, err := packageWindow(dir, tc.start, tc.dur)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(url, "concat:"+filepath.Join(dir, "init.mp4")+"|") {
			t.Errorf("%v: %q", tc, url)
		}
		if got := segs(url); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("window %.1f+%.0f: segments %v, want %v", tc.start, tc.dur, got, tc.want)
		}
	}
	for _, bad := range []string{"../other/seg-00001.m4s", "/abs/seg.m4s", "seg|x.m4s", "seg.m4s?token=1", "sub/seg.m4s"} {
		write("#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\n" + bad + "\n#EXT-X-ENDLIST\n")
		if _, err := packageWindow(dir, 0, 60); err == nil {
			t.Errorf("a playlist naming %q is read", bad)
		}
	}
	write("#EXTM3U\n#EXTINF:6.0,\nseg-00001.m4s\n#EXT-X-ENDLIST\n")
	if _, err := packageWindow(dir, 0, 60); err == nil {
		t.Error("a playlist without its init segment is read")
	}
}

// mediaVersion is the real package under testdata/media: one library
// version, 24 s of 128x72 HEVC (x265, B-frames, a keyframe every 5 s, so its
// segments are not 6 s ones) and stereo AAC, as shaka-packager v3.4.2
// writes them with the packager's arguments, and a WebVTT subtitle sub0.
const (
	mediaItem    = "4d4d4d4d-0000-4000-8000-00000000004d"
	mediaVersion = "5e5e5e5e-0000-4000-8000-00000000005e"
)

func mediaDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "media", "movies", mediaItem[:2], mediaItem, "versions", mediaVersion))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// frame is one decoded frame: its pts (in its stream's time base) and the
// hash of its picture.
type frame struct {
	pts int64
	md5 string
}

// framesFrom is the first n decoded frames of the video input args name
// (framemd5).
func framesFrom(t *testing.T, ffmpeg string, n int, args ...string) []frame {
	t.Helper()
	out := run(t, ffmpeg, append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...),
		"-map", "0:v:0", "-frames:v", itoa(n), "-f", "framemd5", "-")...)
	var frames []frame
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		pts, _ := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		frames = append(frames, frame{pts: pts, md5: strings.TrimSpace(f[5])})
	}
	return frames
}

// With a real ffmpeg: a window read from a package's segments decodes to
// the frames the rendition has at those times — mid-GOP, on a keyframe, in a
// later segment than the first, through concat: and -seek_timestamp (the
// HLS demuxer cannot seek in it) — and the window transcodes into the
// on-the-fly segments, video and audio.
func TestAWindowIsTranscodedFromARealPackage(t *testing.T) {
	ffmpeg, _ := realFFmpeg(t)
	dir := mediaDir(t)
	// The rendition's frames, read whole from its first segment (the HLS
	// demuxer reads it fine without a seek); pts in 1001/24000 s.
	all := framesFrom(t, ffmpeg, 1000, "-i", filepath.Join(dir, "hls", "v0", "playlist.m3u8"))
	if len(all) < 570 {
		t.Fatalf("%d frames in the rendition", len(all))
	}
	for _, at := range []float64{0, 7.3, 10.010, 13.3, 20.5} {
		var want []string
		for _, f := range all {
			if float64(f.pts)*1001/24000 >= at-1e-6 {
				want = append(want, f.md5)
			}
		}
		url, err := packageWindow(filepath.Join(dir, "hls", "v0"), at, 6)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range framesFrom(t, ffmpeg, 5, "-seek_timestamp", "1", "-ss", strconv.FormatFloat(at, 'f', -1, 64), "-i", url) {
			got = append(got, f.md5)
		}
		if len(want) < 5 || !reflect.DeepEqual(got, want[:5]) {
			t.Errorf("-ss %v: the window's frames are not the rendition's\n got %v\nwant %v", at, got, want[:min(5, len(want))])
		}
	}

	// The window transcodes, video (libx264) and audio.
	if out, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx264") {
		t.Skip("ffmpeg has no libx264")
	}
	aac, err := DetectAACEncoder(context.Background(), ffmpeg)
	if err != nil {
		t.Skip(err)
	}
	captureLog(t)
	f := newFakeLibrary(t)
	f.setItem(catalog.Playback{ItemID: mediaItem, Package: &catalog.PackageRef{VersionID: mediaVersion, Dir: dir, Record: "package.json"}})
	h := &HLSHandler{Catalog: noAsset(t), Packages: f.resolver(), FFmpegBin: ffmpeg, CacheDir: t.TempDir(),
		TranscodePreset: "ultrafast", AACEncoder: aac}
	for seg := 0; seg < 4; seg++ {
		if w := get(h, "/api/play/"+mediaItem+"/high/"+itoa(seg)+".m4s?caps=avc,aac"); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("video segment %d: %d %.80q", seg, w.Code, w.Body)
		}
		if w := get(h, "/api/play/"+mediaItem+"/audio/0/"+itoa(seg)+".m4s"); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("audio segment %d: %d %.80q", seg, w.Code, w.Body)
		}
	}
	// The segments play as one stream: init + segments, 24 s of H.264.
	key := mediaItem + "@" + mediaVersion
	var joined []byte
	for _, leaf := range []string{"init", "0", "1", "2", "3"} {
		b, err := os.ReadFile(h.cachePath(key, "high", leaf))
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, b...)
	}
	joinedPath := filepath.Join(t.TempDir(), "window.mp4")
	if err := os.WriteFile(joinedPath, joined, 0o644); err != nil {
		t.Fatal(err)
	}
	frames := framesFrom(t, ffmpeg, 10000, "-i", joinedPath)
	if n := len(frames); n < 570 || n > 580 { // 24.024 s at 23.976 fps: 576 frames
		t.Errorf("%d frames in the transcoded window, want 576", n)
	}
}
