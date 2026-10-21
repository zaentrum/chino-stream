package play

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/zaentrum/live-worker/api"

	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/mkv"
)

// copyFilm makes, with a real ffmpeg, a Matroska original of dur seconds in a
// media root of its own: HEVC with open GOPs (CRA keyframes every 2 s, their
// leading pictures after them in decode order, B-pyramids), 5.1 E-AC-3 in
// English (the default) and stereo AAC in German, and a SubRip track.
func copyFilm(t *testing.T, ffmpeg string, dur int) (root, file string) {
	t.Helper()
	enc := allEncoders(t, ffmpeg)
	if !enc["libx265"] || !enc["eac3"] || !enc["aac"] {
		t.Skip("this ffmpeg has no libx265, eac3 or aac")
	}
	root = t.TempDir()
	srt := filepath.Join(root, "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:03,000\nHello.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(root, "film.mkv")
	d := strconv.Itoa(dur)
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001:duration="+d,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+d,
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration="+d,
		"-i", srt,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=48:min-keyint=48:open-gop=1:bframes=4:b-pyramid=1:scenecut=0:log-level=error",
		"-c:a:0", "eac3", "-ac:a:0", "6", "-metadata:s:a:0", "language=eng",
		"-c:a:1", "aac", "-ac:a:1", "2", "-metadata:s:a:1", "language=ger", "-disposition:a:0", "default", "-disposition:a:1", "0",
		"-c:s", "srt", "-y", file)
	return root, file
}

// copyHandler is an HLSHandler for the original file under root, its ffmpeg
// ffmpegBin, a real ffprobe.
func copyHandler(t *testing.T, root, file, ffmpegBin, ffprobe string) *HLSHandler {
	t.Helper()
	h := &HLSHandler{Catalog: fakeKatalog(t, file), MediaRoot: root, FFmpegBin: ffmpegBin, FFprobeBin: ffprobe, CacheDir: t.TempDir(),
		TranscodePreset: "ultrafast"}
	quiesce(t, h)
	return h
}

// packetMD5s are the MD5s of a stream's packets as ffmpeg's demuxer reads
// them, in order.
func packetMD5s(t *testing.T, ffmpeg, file, stream string) []string {
	t.Helper()
	out := run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", file, "-map", "0:"+stream, "-c", "copy", "-f", "framemd5", "-")
	var hs []string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Split(l, ","); l != "" && !strings.HasPrefix(l, "#") && len(f) >= 6 {
			hs = append(hs, strings.TrimSpace(f[5])) // stream, dts, pts, duration, size, hash[, side data]
		}
	}
	return hs
}

// copyMaster are a master's audio renditions (TYPE=AUDIO lines, each its
// attributes) and its variants (STREAM-INF attributes and URI).
func copyMaster(body string) (media []map[string]string, variants []map[string]string) {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA:"):
			media = append(media, hlsAttributes(l))
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:") && i+1 < len(lines):
			a := hlsAttributes(l)
			a["uri"] = lines[i+1]
			variants = append(variants, a)
		}
	}
	return media, variants
}

// follow plays a media playlist as a player does: the playlist at uri (as a
// master names it, relative to the title's /api/play/{id}/), its init and
// each segment; it returns them joined and how many segments it listed.
func follow(t *testing.T, h *HLSHandler, uri string) ([]byte, int) {
	t.Helper()
	base := "/api/play/i1/"
	pl := get(h, base+uri)
	if pl.Code != 200 {
		t.Fatalf("%s: %d %q", uri, pl.Code, pl.Body)
	}
	if !proxied(uri) {
		t.Errorf("%s is no URI the API in front forwards", uri)
	}
	dir := path.Dir(strings.SplitN(uri, "?", 2)[0]) + "/"
	var out []byte
	n := 0
	for _, l := range strings.Split(pl.Body.String(), "\n") {
		target := ""
		switch {
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			target = hlsAttributes(l)["URI"]
		case l != "" && !strings.HasPrefix(l, "#"):
			target = l
			n++
		}
		if target == "" {
			continue
		}
		if !proxied(strings.TrimPrefix(dir, "./") + target) {
			t.Errorf("%s%s is no URI the API in front forwards", dir, target)
		}
		w := get(h, base+dir+target)
		if w.Code != 200 {
			t.Fatalf("%s%s: %d %q", dir, target, w.Code, w.Body)
		}
		out = append(out, w.Body.Bytes()...)
	}
	return out, n
}

// proxied says whether a URI of an on-the-fly master has a shape the API in
// front of chino-stream forwards: one folder and index.m3u8, init.mp4 or
// <n>.m4s in it — a folder that is no package rendition's id ([vas]<n>) —
// or audio/<n>/ and the same.
func proxied(uri string) bool {
	p := strings.SplitN(uri, "?", 2)[0]
	parts := strings.Split(p, "/")
	leafOK := func(l string) bool {
		if l == "index.m3u8" || l == "init.mp4" {
			return true
		}
		n, err := strconv.Atoi(strings.TrimSuffix(l, ".m4s"))
		return strings.HasSuffix(l, ".m4s") && err == nil && n >= 0
	}
	switch {
	case len(parts) == 2:
		f := parts[0]
		if len(f) > 1 && strings.ContainsRune("vas", rune(f[0])) {
			if _, err := strconv.Atoi(f[1:]); err == nil {
				return false
			}
		}
		return f != "" && leafOK(parts[1])
	case len(parts) == 3 && parts[0] == "audio":
		_, err := strconv.Atoi(parts[1])
		return err == nil && leafOK(parts[2])
	}
	return false
}

// tiles checks a track of a copy, its init and segments joined: its first
// packet a keyframe, decode times rising, each packet lasting until the next
// one's — no gap and no overlap at any segment's edge.
func tiles(t *testing.T, ffprobe, file, stream string) (firstPTS float64) {
	t.Helper()
	out := run(t, ffprobe, "-v", "error", "-select_streams", stream, "-show_entries", "packet=pts_time,dts,duration,flags", "-of", "csv=p=0", file)
	var prevEnd int64 = -1 << 62
	for i, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 4 {
			t.Fatalf("%s: packet line %q", stream, l)
		}
		pts, _ := strconv.ParseFloat(f[0], 64)
		dts, _ := strconv.ParseInt(f[1], 10, 64)
		dur, _ := strconv.ParseInt(f[2], 10, 64)
		if i == 0 {
			firstPTS = pts
			if !strings.HasPrefix(f[3], "K") {
				t.Errorf("%s: the first packet is no keyframe", stream)
			}
		} else if dts != prevEnd {
			t.Errorf("%s: packet %d decodes at %d, the one before ends at %d", stream, i, dts, prevEnd)
			return firstPTS
		}
		prevEnd = dts + dur
	}
	return firstPTS
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// histogramCount is how many observations a histogram of a vector has.
func histogramCount(t *testing.T, h *prometheus.HistogramVec, label string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.WithLabelValues(label).(prometheus.Histogram).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// A Matroska original of HEVC with 5.1 E-AC-3 and a second, stereo AAC
// track, for a TV that decodes the video and both audio codecs: nothing is
// encoded. The master's variant is the video copied, its audio group each
// track copied; each copied track holds exactly the original's packets, its
// decode timeline tiled across every segment's edge, the video decoding to
// the original's frames; ffmpeg never runs. /play/info says so.
func TestStreamCopyForATVThatDecodesItAll(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 13)
	state := t.TempDir()
	h := copyHandler(t, root, file, windowFFmpeg(t, state, 99, 0, "no ffmpeg here"), ffprobe)
	before := histogramCount(t, metrics.SegmentProduction, "copy")
	const caps = "?caps=avc:4096,hvc:4096,av1:3840,aac,mp3,opus,ac3,eac3"

	m := get(h, "/api/play/i1/master.m3u8"+caps)
	if m.Code != 200 {
		t.Fatalf("master: %d %q", m.Code, m.Body)
	}
	media, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/index.m3u8"+caps+"&c=") ||
		!strings.HasPrefix(variants[0]["CODECS"], "hvc1.") || !strings.Contains(variants[0]["CODECS"], "ec-3") ||
		!strings.Contains(variants[0]["CODECS"], "mp4a.40.2") || variants[0]["VIDEO-RANGE"] != "SDR" || variants[0]["RESOLUTION"] != "320x180" ||
		strings.Contains(m.Body.String(), "lw=") {
		t.Fatalf("master:\n%s", m.Body)
	}
	if len(media) != 2 || !strings.HasPrefix(media[0]["URI"], "audiocopy0/index.m3u8") || media[0]["DEFAULT"] != "YES" ||
		media[0]["CHANNELS"] != "6" || media[0]["NAME"] != "English" || !strings.HasPrefix(media[1]["URI"], "audiocopy1/index.m3u8") ||
		media[1]["NAME"] != "German" || media[1]["CHANNELS"] != "2" {
		t.Fatalf("audio renditions %v", media)
	}

	video, n := follow(t, h, variants[0]["uri"])
	if n < 3 {
		t.Fatalf("%d video segments", n)
	}
	vf := writeTemp(t, "video.mp4", video)
	if want, got := packetMD5s(t, ffmpeg, file, "v:0"), packetMD5s(t, ffmpeg, vf, "v:0"); !reflect.DeepEqual(got, want) {
		t.Errorf("video: %d packets, the original %d (equal %v)", len(got), len(want), reflect.DeepEqual(got, want))
	}
	if want, got := frameHashes(t, ffmpeg, file, "v:0", false), frameHashes(t, ffmpeg, vf, "v:0", false); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded video: %d frames, the original %d", len(got), len(want))
	}
	vStart := tiles(t, ffprobe, vf, "v:0")
	for i, stream := range []string{"a:0", "a:1"} {
		audio, an := follow(t, h, media[i]["URI"])
		if an != n {
			t.Errorf("%s: %d segments, the video %d", stream, an, n)
		}
		af := writeTemp(t, "audio"+strconv.Itoa(i)+".mp4", audio)
		if want, got := packetMD5s(t, ffmpeg, file, stream), packetMD5s(t, ffmpeg, af, "a:0"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %d packets, the original %d (equal %v)", stream, len(got), len(want), reflect.DeepEqual(got, want))
		}
		if aStart := tiles(t, ffprobe, af, "a:0"); aStart < vStart-0.05 || aStart > vStart+0.05 {
			t.Errorf("%s starts at %.3f s, the video at %.3f s", stream, aStart, vStart)
		}
	}
	if runs := ffmpegRuns(t, state); len(runs) != 0 {
		t.Errorf("ffmpeg ran %d times", len(runs))
	}
	if got := histogramCount(t, metrics.SegmentProduction, "copy"); got < before+uint64(n) {
		t.Errorf("copy segments counted: %d more, want %d", got-before, n)
	}

	ph := &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: ffprobe, HLS: h}
	var info map[string]any
	if err := json.Unmarshal(getPlay(ph, "/api/play/i1/info"+caps).Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["mode"] != "remux" || info["reason"] != "The video and the audio play as they are in the file." || info["qualities"] != nil {
		t.Errorf("info %v", info)
	}
}

// A browser that decodes the video but not E-AC-3: the video and the AAC
// track are copied, the E-AC-3 track converted to stereo AAC on the 6 s grid
// by ffmpeg's windows — in step with the copied video.
func TestStreamCopyConvertsOnlyTheAudioTheClientDoesNotDecode(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	aac, err := DetectAACEncoder(context.Background(), ffmpeg)
	if err != nil {
		t.Skip(err)
	}
	root, file := copyFilm(t, ffmpeg, 13)
	h := copyHandler(t, root, file, ffmpeg, ffprobe)
	h.AACEncoder = aac
	const caps = "?caps=avc,hvc,av1,vp9,aac,mp3,opus"
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	media, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") || strings.Contains(variants[0]["CODECS"], "ec-3") ||
		len(media) != 2 || !strings.HasPrefix(media[0]["URI"], "audio/0/index.m3u8") || media[0]["CHANNELS"] != "2" ||
		!strings.HasPrefix(media[1]["URI"], "audiocopy1/") {
		t.Fatalf("master:\n%s", m.Body)
	}
	video, _ := follow(t, h, variants[0]["uri"])
	vf := writeTemp(t, "video.mp4", video)
	vStart := tiles(t, ffprobe, vf, "v:0")
	converted, n := follow(t, h, media[0]["URI"])
	if want := liveGrid(13000); n < want-1 || n > want {
		t.Errorf("the converted track lists %d segments, the grid %d", n, want)
	}
	cf := writeTemp(t, "converted.mp4", converted)
	out := run(t, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,channels", "-of", "csv=p=0", cf)
	if strings.TrimSpace(string(out)) != "aac,2" {
		t.Errorf("converted track: %s", out)
	}
	// The converted audio presents from where the copied video does (its
	// first frame, the encoder's priming before it).
	if aStart := tiles(t, ffprobe, cf, "a:0"); aStart < vStart-0.05 || aStart > vStart+0.05 {
		t.Errorf("the converted audio starts at %.3f s, the video at %.3f s", aStart, vStart)
	}

	ph := &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: ffprobe, HLS: h}
	var info map[string]any
	_ = json.Unmarshal(getPlay(ph, "/api/play/i1/info"+caps).Body.Bytes(), &info)
	if want := "The video and the AAC audio play as they are in the file. The E-AC-3 audio is converted to stereo AAC, " +
		"because this device does not play E-AC-3."; info["mode"] != "remux" || info["reason"] != want {
		t.Errorf("info %v", info)
	}
}

// With LIVE_WORKER_MODE on the stream copy is still chino-stream's own, in Go;
// only the conversion of the audio goes to the worker, a job per segment on
// the grid in its cpu lane: the session's URIs carry lw=1, no video job is
// asked for, and ffmpeg never runs.
func TestStreamCopyOnTheLivePipeline(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 13)
	state := t.TempDir()
	fw := newFakeWorker(t)
	h := copyHandler(t, root, file, windowFFmpeg(t, state, 99, 0, "no ffmpeg here"), ffprobe)
	h.Live = fw.live(LiveWorkerOn)
	const caps = "?caps=avc,hvc,aac"
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	media, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") || !strings.Contains(variants[0]["uri"], "&lw=1") ||
		len(media) != 2 || !strings.HasPrefix(media[0]["URI"], "audio/0/") {
		t.Fatalf("master:\n%s", m.Body)
	}
	if _, n := follow(t, h, variants[0]["uri"]); n < 3 {
		t.Fatalf("%d video segments", n)
	}
	if w := get(h, "/api/play/i1/"+strings.Replace(media[0]["URI"], "index.m3u8", "1.m4s", 1)); w.Code != 200 {
		t.Fatalf("converted audio: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	audio := segmentCalls(fw, api.TrackAudio)
	if len(audio) == 0 || audio[0].req.Audio.Stream != 0 || audio[0].req.Source.Path != file {
		t.Errorf("audio jobs %+v", audio)
	}
	if v := segmentCalls(fw, api.TrackVideo); len(v) != 0 {
		t.Errorf("%d video jobs for a copied video", len(v))
	}
	if runs := ffmpegRuns(t, state); len(runs) != 0 {
		t.Errorf("ffmpeg ran %d times", len(runs))
	}
}

// voidCues turns the Cues of the Matroska file into a Void element of the same
// size: a file without an index, its SeekHead pointing at nothing.
func voidCues(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mkv.Open(bytes.NewReader(b), int64(len(b)))
	if err != nil || f.CuesPos < 0 {
		t.Fatalf("cues: %v", err)
	}
	// The Cues element's size: its ID (4 bytes) and VINT size.
	pos := f.CuesPos
	l := 1
	for mask := byte(0x80); b[pos+4]&mask == 0; mask >>= 1 {
		l++
	}
	size := int64(b[pos+4] & (0xFF >> l))
	for i := 1; i < l; i++ {
		size = size<<8 | int64(b[pos+4+int64(i)])
	}
	total := 4 + int64(l) + size
	// A Void of total bytes: its ID, an 8-byte size, zeros.
	void := make([]byte, total)
	void[0] = 0xEC
	void[1] = 0x01
	binary.BigEndian.PutUint64(void[1:9], uint64(total-9)|0x01<<56)
	copy(b[pos:], void)
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A Matroska file without Cues: its first sessions take the encode path —
// never a read of the whole file on the request path — while the file is
// read through in the background, once; then its sessions copy, and the
// index made is kept across a restart.
func TestStreamCopyOfAFileWithoutAnIndex(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 13)
	voidCues(t, file)
	release := make(chan struct{})
	beforeBuild = func() { <-release }
	t.Cleanup(func() { beforeBuild = nil })
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	fallbacks := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("index"))
	const caps = "?caps=hvc,aac,eac3"
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	if !strings.Contains(m.Body.String(), "\nhigh/index.m3u8") || strings.Contains(m.Body.String(), "video/index.m3u8") {
		t.Fatalf("the first master:\n%s", m.Body)
	}
	if got := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("index")); got != fallbacks+1 {
		t.Errorf("index fallbacks %v more, want 1", got-fallbacks)
	}
	ph := &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: ffprobe, HLS: h}
	var info map[string]any
	_ = json.Unmarshal(getPlay(ph, "/api/play/i1/info"+caps).Body.Bytes(), &info)
	if info["mode"] != "transcode" || !strings.Contains(info["reason"].(string), "read through once before it can play as it is") {
		t.Errorf("info %v", info)
	}
	close(release)
	h.streamIndexes.jobs.Wait()
	m = get(h, "/api/play/i1/master.m3u8"+caps)
	_, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") {
		t.Fatalf("the master once the file is indexed:\n%s", m.Body)
	}
	video, _ := follow(t, h, variants[0]["uri"])
	if want, got := packetMD5s(t, ffmpeg, file, "v:0"), packetMD5s(t, ffmpeg, writeTemp(t, "v.mp4", video), "v:0"); !reflect.DeepEqual(got, want) {
		t.Errorf("video of the scanned file: %d packets, the original %d", len(got), len(want))
	}

	// A restart: the index is read back from the cache, not made again.
	persisted := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("persisted"))
	again := &HLSHandler{Catalog: h.Catalog, MediaRoot: root, FFmpegBin: h.FFmpegBin, FFprobeBin: ffprobe, CacheDir: h.CacheDir}
	quiesce(t, again)
	if m := get(again, "/api/play/i1/master.m3u8"+caps); !strings.Contains(m.Body.String(), "video/index.m3u8") {
		t.Fatalf("the master after a restart:\n%s", m.Body)
	}
	if got := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("persisted")); got != persisted+1 {
		t.Errorf("persisted indexes read: %v more, want 1", got-persisted)
	}
	// The cache's sweep leaves the indexes alone.
	again.sweep(0)
	if entries, _ := os.ReadDir(again.indexDir()); len(entries) != 1 {
		t.Errorf("%d persisted indexes after the sweep", len(entries))
	}
}

// A segment the copy cannot make (the file is damaged where the segment is):
// that request is 404 — the player falls back to the encode path — and the
// title's new sessions take the encode path, counted; the segments before it
// played.
func TestStreamCopyFallsBackWhenASegmentFails(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 13)
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=hvc,aac,eac3"
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	_, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") {
		t.Fatalf("master:\n%s", m.Body)
	}
	ix, _, _ := h.streamIndex(context.Background(), &source{key: "i1", file: file, probe: mustProbe(t, ffprobe, file)})
	spans := ix.Spans()
	if len(spans) < 2 {
		t.Fatalf("%d segments", len(spans))
	}
	// The last segment's Cluster, damaged (same size and mtime would keep the
	// index; the file is written again, so the index is made anew first).
	last := ix.Keyframes[spans[len(spans)-1].First].Pos
	for i := int64(0); i < 16; i++ {
		b[last+i] = 0
	}
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m = get(h, "/api/play/i1/master.m3u8"+caps)
	_, variants = copyMaster(m.Body.String())
	// Segment 0 is served; the damaged one fails the title once, whether its
	// prefetch or its request finds it.
	failed := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("segment"))
	uri := strings.Replace(variants[0]["uri"], "index.m3u8", "0.m4s", 1)
	if w := get(h, "/api/play/i1/"+uri); w.Code != 200 {
		t.Fatalf("segment 0: %d", w.Code)
	}
	lastURI := strings.Replace(variants[0]["uri"], "index.m3u8", strconv.Itoa(len(spans)-1)+".m4s", 1)
	if w := get(h, "/api/play/i1/"+lastURI); w.Code != 404 {
		t.Errorf("the damaged segment: %d", w.Code)
	}
	h.copyWork.Wait()
	if got := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("segment")); got != failed+1 {
		t.Errorf("segment fallbacks %v more, want 1", got-failed)
	}
	// The session goes on: its other segments are made (none of them in the
	// memory now), and the damaged one is not made again.
	h.copyCache.mu.Lock()
	h.copyCache.m, h.copyCache.bytes = nil, 0
	h.copyCache.mu.Unlock()
	var made atomic.Int32
	produceHook = func(int) { made.Add(1) }
	t.Cleanup(func() { produceHook = nil })
	if w := get(h, "/api/play/i1/"+uri); w.Code != 200 {
		t.Errorf("segment 0 of the session after the failure: %d", w.Code)
	}
	h.copyWork.Wait()
	before := made.Load()
	if w := get(h, "/api/play/i1/"+lastURI); w.Code != 404 || made.Load() != before {
		t.Errorf("the damaged segment asked again: %d, made %d more times", w.Code, made.Load()-before)
	}
	if m := get(h, "/api/play/i1/master.m3u8"+caps); strings.Contains(m.Body.String(), "video/index.m3u8") || !strings.Contains(m.Body.String(), "high/index.m3u8") {
		t.Errorf("the master after the failure:\n%s", m.Body)
	}
	ph := &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: ffprobe, HLS: h}
	var info map[string]any
	_ = json.Unmarshal(getPlay(ph, "/api/play/i1/info"+caps).Body.Bytes(), &info)
	if info["mode"] != "transcode" || !strings.Contains(info["reason"].(string), "could not be played as it is") {
		t.Errorf("info %v", info)
	}
}

func mustProbe(t *testing.T, ffprobe, file string) *Probe {
	t.Helper()
	p, err := RunFFprobe(context.Background(), ffprobe, file)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

// A session's URIs name the file it began on (c=): another file at the
// item's path is not served under them.
func TestStreamCopySessionKeepsItsFile(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 7)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	m := get(h, "/api/play/i1/master.m3u8?caps=hvc,aac,eac3")
	_, variants := copyMaster(m.Body.String())
	if len(variants) != 1 {
		t.Fatalf("master:\n%s", m.Body)
	}
	init := "/api/play/i1/" + strings.Replace(variants[0]["uri"], "index.m3u8", "init.mp4", 1)
	if w := get(h, init); w.Code != 200 {
		t.Fatalf("init: %d", w.Code)
	}
	// The original replaced by another file.
	_, other := copyFilm(t, ffmpeg, 8)
	b, _ := os.ReadFile(other)
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := get(h, init); w.Code != 404 {
		t.Errorf("the session's init on another file: %d", w.Code)
	}
}

// An MP4 original whose audio the client does not decode: before, the whole
// title was encoded; now its video is copied (from its sample tables, by
// byte range) and only the audio converted. One the client takes whole
// stays the muxed stream copy it always was.
func TestStreamCopyOfAnMP4(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	if enc := allEncoders(t, ffmpeg); !enc["libx264"] || !enc["ac3"] || !enc["aac"] {
		t.Skip("this ffmpeg has no libx264, ac3 or aac")
	}
	root := t.TempDir()
	file := filepath.Join(root, "film.mp4")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=14",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=14",
		"-map", "0", "-map", "1", "-map", "1",
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "2", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-c:a:0", "ac3", "-ac:a:0", "6", "-c:a:1", "aac", "-ac:a:1", "2", "-y", file)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	m := get(h, "/api/play/i1/master.m3u8?caps=avc,aac")
	media, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") || len(media) != 2 ||
		!strings.HasPrefix(media[0]["URI"], "audio/0/") || !strings.HasPrefix(media[1]["URI"], "audiocopy1/") ||
		!strings.HasPrefix(variants[0]["CODECS"], "avc1.") {
		t.Fatalf("master:\n%s", m.Body)
	}
	video, _ := follow(t, h, variants[0]["uri"])
	vf := writeTemp(t, "video.mp4", video)
	if want, got := packetMD5s(t, ffmpeg, file, "v:0"), packetMD5s(t, ffmpeg, vf, "v:0"); !reflect.DeepEqual(got, want) {
		t.Errorf("video: %d packets, the original %d", len(got), len(want))
	}
	vStart := tiles(t, ffprobe, vf, "v:0")
	audio, _ := follow(t, h, media[1]["URI"])
	af := writeTemp(t, "audio.mp4", audio)
	if want, got := packetMD5s(t, ffmpeg, file, "a:1"), packetMD5s(t, ffmpeg, af, "a:0"); !reflect.DeepEqual(got, want) {
		t.Errorf("AAC: %d packets, the original %d", len(got), len(want))
	}
	if aStart := tiles(t, ffprobe, af, "a:0"); aStart < vStart-0.05 || aStart > vStart+0.05 {
		t.Errorf("the copied AAC starts at %.3f s, the video at %.3f s", aStart, vStart)
	}
	// One whose first audio track the client decodes: the muxed copy, as
	// before.
	if m := get(h, "/api/play/i1/master.m3u8?caps=avc,aac,ac3"); !strings.Contains(m.Body.String(), "\ncopy/index.m3u8") ||
		strings.Contains(m.Body.String(), "EXT-X-MEDIA") {
		t.Errorf("the muxed copy:\n%s", m.Body)
	}
}

// /prewarm of a stream copy session makes its first segment.
func TestStreamCopyPrewarm(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 7)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	r := chiPost(h, "/api/play/i1/prewarm?caps=hvc,aac,eac3")
	if r.Code != 202 {
		t.Fatalf("prewarm: %d %q", r.Code, r.Body)
	}
	h.copyWork.Wait()
	ix, state, _ := h.streamIndex(context.Background(), &source{key: "i1", file: file, probe: mustProbe(t, ffprobe, file)})
	if state != indexReady {
		t.Fatalf("index state %d", state)
	}
	seg := h.copyCache.get(h.copyKey(&source{key: "i1", file: file}, ix, 0))
	if seg == nil || len(seg.track(-1)) == 0 || len(seg.track(0)) == 0 || len(seg.track(1)) == 0 {
		t.Error("the first segment is not made")
	}
}

// chiPost runs one POST against h's routes.
func chiPost(h *HLSHandler, path string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", h.Routes)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	return w
}

// A native player (AVPlayer) that decodes E-AC-3 gets Apple's shape: the
// stereo group — the E-AC-3 track converted to stereo AAC, the AAC track
// copied — and a 5.1 group of the same members, the E-AC-3 track copied
// there; the variant once per group.
func TestStreamCopyForANativePlayer(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := copyFilm(t, ffmpeg, 7)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	m := get(h, "/api/play/i1/master.m3u8?caps=avc,hvc,aac,ac3,eac3,native")
	media, variants := copyMaster(m.Body.String())
	byGroup := map[string][]map[string]string{}
	for _, md := range media {
		byGroup[md["GROUP-ID"]] = append(byGroup[md["GROUP-ID"]], md)
	}
	stereo, surround := byGroup[onTheFlyAudioGroup], byGroup[onTheFlySurroundGroup]
	if len(stereo) != 2 || len(surround) != 2 || len(variants) != 2 {
		t.Fatalf("master:\n%s", m.Body)
	}
	if !strings.HasPrefix(stereo[0]["URI"], "audio/0/") || !strings.HasPrefix(stereo[1]["URI"], "audiocopy1/") ||
		!strings.HasPrefix(surround[0]["URI"], "audiocopy0/") || surround[0]["CHANNELS"] != "6" || surround[1]["URI"] != stereo[1]["URI"] {
		t.Errorf("groups:\n%s", m.Body)
	}
	for i := range stereo {
		if stereo[i]["NAME"] != surround[i]["NAME"] || stereo[i]["DEFAULT"] != surround[i]["DEFAULT"] {
			t.Errorf("member %d: %v / %v", i, stereo[i], surround[i])
		}
	}
	if variants[0]["AUDIO"] != onTheFlyAudioGroup || strings.Contains(variants[0]["CODECS"], "ec-3") ||
		variants[1]["AUDIO"] != onTheFlySurroundGroup || !strings.Contains(variants[1]["CODECS"], "ec-3") || variants[0]["uri"] != variants[1]["uri"] {
		t.Errorf("variants %v", variants)
	}
}

// A client that decodes AAC beyond two channels (aacmc) but not E-AC-3 gets
// the E-AC-3 track converted twice: to 5.1 AAC (surround0/, first, the
// default) and to stereo AAC (audio/0/).
func TestStreamCopyConvertsTo51ForAnAACMultichannelClient(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	aac, err := DetectAACEncoder(context.Background(), ffmpeg)
	if err != nil {
		t.Skip(err)
	}
	root, file := copyFilm(t, ffmpeg, 7)
	h := copyHandler(t, root, file, ffmpeg, ffprobe)
	h.AACEncoder = aac
	m := get(h, "/api/play/i1/master.m3u8?caps=hvc,aac,aacmc")
	media, _ := copyMaster(m.Body.String())
	if len(media) != 3 || !strings.HasPrefix(media[0]["URI"], "surround0/") || media[0]["CHANNELS"] != "6" || media[0]["DEFAULT"] != "YES" ||
		!strings.HasPrefix(media[1]["URI"], "audio/0/") || media[1]["DEFAULT"] != "NO" || !strings.HasPrefix(media[2]["URI"], "audiocopy1/") ||
		media[0]["NAME"] == media[1]["NAME"] {
		t.Fatalf("master:\n%s", m.Body)
	}
	b, _ := follow(t, h, media[0]["URI"])
	out := run(t, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,channels", "-of", "csv=p=0",
		writeTemp(t, "surround.mp4", b))
	if strings.TrimSpace(string(out)) != "aac,6" {
		t.Errorf("the 5.1 conversion: %s", out)
	}
}

// An MPEG-TS original the client takes whole keeps its muxed stream copy, but
// its keyframes (which only a read of the whole file finds) are probed in
// the background: its first session takes the encode path, later ones the
// copy.
func TestMPEGTSKeepsItsMuxedCopyFoundInTheBackground(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	_, mp4 := copyOriginal(t, ffmpeg, false)
	root := t.TempDir()
	ts := filepath.Join(root, "film.ts")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", mp4, "-c", "copy", "-y", ts)
	release := make(chan struct{})
	beforeBuild = func() { <-release }
	t.Cleanup(func() { beforeBuild = nil })
	h := copyHandler(t, root, ts, ffmpeg, ffprobe)
	const caps = "?caps=avc,aac"
	if m := get(h, "/api/play/i1/master.m3u8"+caps); !strings.Contains(m.Body.String(), "\nhigh/index.m3u8") {
		t.Fatalf("the first master:\n%s", m.Body)
	}
	close(release)
	h.streamIndexes.jobs.Wait()
	if m := get(h, "/api/play/i1/master.m3u8"+caps); !strings.Contains(m.Body.String(), "\ncopy/index.m3u8") {
		t.Fatalf("the master once its keyframes are known:\n%s", m.Body)
	}
}
