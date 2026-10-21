package play

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/cmaf"

	"github.com/zaentrum/chino-stream/internal/catalog"
	"github.com/zaentrum/chino-stream/internal/metrics"
)

// liveProbe is an ffprobe answer for an original of dur seconds: one video
// stream, two audio streams.
func liveProbe(container, vcodec string, w, h int, acodec, dur string) string {
	return `{"streams":[{"codec_type":"video","codec_name":"` + vcodec + `","width":` + strconv.Itoa(w) + `,"height":` + strconv.Itoa(h) +
		`},{"codec_type":"audio","codec_name":"` + acodec + `","channels":2,"tags":{"language":"eng"},"disposition":{"default":1}}` +
		`,{"codec_type":"audio","codec_name":"` + acodec + `","channels":2,"tags":{"language":"ger"}}]` +
		`,"format":{"format_name":"` + container + `","duration":"` + dur + `","bit_rate":"8000000"}}`
}

// hevcFilm is a 20.021 s 1080p HEVC original: a transcode for an H.264
// client, on a grid of three 6 s segments and a tail of 2.021 s.
var hevcFilm = liveProbe("matroska,webm", "hevc", 1920, 1080, "aac", "20.021")

// liveOriginal is an HLSHandler for the original film.mkv the probe
// describes, its ffmpeg the fake windowFFmpeg at state (ten segments a
// window), with the fake worker fw in mode (no live worker for fw nil).
func liveOriginal(t *testing.T, probe string, fw *fakeWorker, mode LiveWorkerMode) (h *HLSHandler, state, file string) {
	t.Helper()
	root, src := mediaFile(t, "film.mkv")
	state = t.TempDir()
	h = &HLSHandler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: windowFFmpeg(t, state, 0, 10, ""),
		FFprobeBin: probeFFprobe(t, probe), CacheDir: t.TempDir(), TranscodePreset: "veryfast"}
	if fw != nil {
		h.Live = fw.live(mode)
	}
	quiesce(t, h)
	return h, state, src
}

// quiesce waits, when the test ends and before its folders go, until h
// produces no window and runs no live job any more.
func quiesce(t *testing.T, h *HLSHandler) {
	t.Helper()
	t.Cleanup(func() {
		waitProductions(t, h)
		if h.Live != nil {
			settle(t, h.Live)
		}
		h.copyWork.Wait()
		h.streamIndexes.jobs.Wait()
	})
}

// segmentCalls are the fake's segment calls, by track.
func segmentCalls(fw *fakeWorker, track string) []workerCall {
	var out []workerCall
	for _, c := range fw.got("/v1/segment") {
		if c.req.Track == track {
			out = append(out, c)
		}
	}
	return out
}

// byIndex orders segment calls by their segment.
func byIndex(calls []workerCall) []workerCall {
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].req.Segment.Index < calls[j].req.Segment.Index })
	return calls
}

// With LIVE_WORKER_MODE off — the default — a live worker is never asked for
// anything: the master, its playlists and segments are today's, byte for
// byte, ffmpeg makes the segments, and lw is not written.
func TestLiveWorkerOffServesTodaysPipeline(t *testing.T) {
	captureLog(t)
	inPlaceWarms(t)
	fw := newFakeWorker(t)
	off, offState, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOff)
	none, _, _ := liveOriginal(t, hevcFilm, nil, LiveWorkerOff)
	for _, route := range []string{"master.m3u8?caps=avc,aac&stream=tok", "master.m3u8?q=medium", "high/index.m3u8?caps=avc,aac",
		"audio/1/index.m3u8?caps=avc,aac"} {
		a, b := get(off, "/api/play/i1/"+route), get(none, "/api/play/i1/"+route)
		if a.Code != 200 || a.Body.String() != b.Body.String() || a.Header().Get("Content-Type") != b.Header().Get("Content-Type") {
			t.Errorf("%s with the worker off:\n%s\nwithout one:\n%s", route, a.Body, b.Body)
		}
		if strings.Contains(a.Body.String(), "lw=") {
			t.Errorf("%s writes the live pin with the worker off:\n%s", route, a.Body)
		}
	}
	for _, route := range []string{"high/init.mp4", "high/2.m4s", "audio/1/init.mp4", "audio/1/0.m4s"} {
		if w := get(off, "/api/play/i1/"+route+"?caps=avc,aac"); w.Code != 200 {
			t.Errorf("%s: %d %q", route, w.Code, w.Body)
		}
	}
	if len(ffmpegRuns(t, offState)) == 0 {
		t.Error("ffmpeg made nothing")
	}
	if calls := fw.got(""); len(calls) != 0 {
		t.Errorf("the worker was asked %d times with the mode off: %+v", len(calls), calls[0])
	}
	if statOK(off.cachePath("i1", "high"+liveSuffix, "2")) {
		t.Error("a live cache folder was written")
	}
}

// Without a live worker (no LIVE_WORKER_URL), lw means nothing: a request
// carrying it is served by ffmpeg as before.
func TestNoLiveWorkerIgnoresThePin(t *testing.T) {
	captureLog(t)
	h, state, _ := liveOriginal(t, hevcFilm, nil, LiveWorkerOff)
	if w := get(h, "/api/play/i1/high/2.m4s?lw=1"); w.Code != 200 || w.Body.String() != "seg" {
		t.Fatalf("segment: %d %q", w.Code, w.Body)
	}
	if len(ffmpegRuns(t, state)) != 1 {
		t.Errorf("ffmpeg runs %d, want 1", len(ffmpegRuns(t, state)))
	}
}

// With LIVE_WORKER_MODE on, a new on-the-fly session goes to the live
// pipeline: the master's URIs carry lw=1 (after the version pin of a
// package), the playlists are the grid's as before, and its start is warmed
// on the worker at prefetch priority: the rung's init and three segments from
// t.
func TestLiveMasterPinsTheSessionAndWarmsItsStart(t *testing.T) {
	captureLog(t)
	inPlaceWarms(t)
	fw := newFakeWorker(t)
	on, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
	none, _, _ := liveOriginal(t, hevcFilm, nil, LiveWorkerOff)

	w := get(on, "/api/play/i1/master.m3u8?caps=avc,aac&stream=tok&lw=0&t=7")
	body := w.Body.String()
	want := strings.ReplaceAll(get(none, "/api/play/i1/master.m3u8?caps=avc,aac&stream=tok&t=7").Body.String(), "&t=7", "&t=7&lw=1")
	if w.Code != 200 || body != want {
		t.Fatalf("master:\n%s\nwant:\n%s", body, want)
	}
	settle(t, on.Live)
	var warmed []string
	for _, c := range fw.got("") {
		if c.req.Priority != api.PriorityPrefetch || c.req.DeadlineMs <= 20000 {
			t.Errorf("a warm at %s, deadline %d ms", c.req.Priority, c.req.DeadlineMs)
		}
		k := "init"
		if c.req.Segment != nil {
			k = strconv.Itoa(c.req.Segment.Index)
		}
		warmed = append(warmed, c.req.Track+":"+k)
	}
	for _, want := range []string{"video:init", "video:1", "video:2", "video:3", "audio:init", "audio:1", "audio:2", "audio:3"} {
		if !strings.Contains(strings.Join(warmed, " "), want) {
			t.Errorf("the warm asked for %v, not %s", warmed, want)
		}
	}

	// The playlists: the grid, the URIs pinned.
	pl := get(on, "/api/play/i1/high/index.m3u8?caps=avc,aac&lw=1")
	plOff := get(none, "/api/play/i1/high/index.m3u8?caps=avc,aac&lw=1")
	if pl.Body.String() != plOff.Body.String() || !strings.Contains(pl.Body.String(), "#EXTINF:2.021,\n3.m4s?caps=avc,aac&lw=1") {
		t.Errorf("playlist:\n%s", pl.Body)
	}

	// A package's master: the version pin, then the live pin.
	l := stageLibrary(t)
	pkg := &HLSHandler{Catalog: noAsset(t), Packages: l.packages, FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"), CacheDir: t.TempDir(),
		Live: fw.live(LiveWorkerOn)}
	quiesce(t, pkg)
	if w := get(pkg, "/api/play/"+libItem+"/master.m3u8?caps=avc,aac"); !strings.Contains(w.Body.String(), "\nhigh/index.m3u8?caps=avc,aac&v="+libNew+"&lw=1\n") {
		t.Errorf("package master:\n%s", w.Body)
	}
	settle(t, pkg.Live)
}

// On the live pipeline a video rung's init is POST /v1/init and each of its
// segments one job on the grid: the frame and bitrate the master advertises,
// the segment's boundary, playhead priority with a 12 s deadline; the bytes
// served are the worker's, cached under a folder of the live pipeline's and
// served from there again; the next three segments are asked for ahead of
// the play head. ffmpeg never runs.
func TestLiveVideoSegmentsAreWorkerJobsOnTheGrid(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	h, state, file := liveOriginal(t, liveProbe("matroska,webm", "hevc", 3840, 1606, "aac", "20.021"), fw, LiveWorkerOn)
	const q = "?caps=avc:1080,aac&lw=1"

	w := get(h, "/api/play/i1/medium/init.mp4"+q)
	if w.Code != 200 {
		t.Fatalf("init: %d %q", w.Code, w.Body)
	}
	if tr, err := cmaf.ReadInit(w.Body.Bytes()); err != nil || tr.Kind != "video" {
		t.Fatalf("init: %v %+v", err, tr)
	}
	inits := fw.got("/v1/init")
	if len(inits) != 1 {
		t.Fatalf("%d init calls", len(inits))
	}
	g := transcodeGeometry(QualityLadder["medium"], 3840, 1606, 1080)
	if r := inits[0].req; r.Source.Path != file || r.Track != api.TrackVideo || r.Video.Width != g.w || r.Video.Height != g.h ||
		r.Video.BitrateKbps != transcodeBandwidth("medium", g.w, g.h, false)/1000 || r.Video.Tonemap != api.TonemapAuto ||
		r.Priority != api.PriorityPlayhead || r.DeadlineMs <= 8000 || r.DeadlineMs > 10000 || r.Segment != nil {
		t.Errorf("init request %+v %+v", r, r.Video)
	}

	w = get(h, "/api/play/i1/medium/0.m4s"+q)
	if w.Code != 200 || w.Header().Get("Content-Type") != "video/iso.segment" {
		t.Fatalf("segment: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	segs := byIndex(segmentCalls(fw, api.TrackVideo))
	if len(segs) != 4 {
		t.Fatalf("%d segment calls, want 1 and 3 ahead", len(segs))
	}
	if s := segs[0].req; s.Segment.Index != 0 || s.Segment.Start != 0 || s.Segment.End != 6 || s.Segment.Last ||
		s.Priority != api.PriorityPlayhead || s.DeadlineMs <= 10000 || s.DeadlineMs > 12000 {
		t.Errorf("segment request %+v %+v", s, s.Segment)
	}
	for i, c := range segs[1:] {
		if s := c.req; s.Segment.Index != 1+i || s.Priority != api.PriorityPrefetch || s.DeadlineMs <= 20000 {
			t.Errorf("prefetch %d: %+v %+v", i, s, s.Segment)
		}
	}
	if last := segs[3].req.Segment; last.Index != 3 || last.Start != 18 || last.End != 20.021 || !last.Last {
		t.Errorf("the last segment %+v", last)
	}
	frags, _, err := cmaf.ReadFragments(w.Body.Bytes(), nil)
	if err != nil || len(frags) != 1 || frags[0].BaseDecodeTime != 0 || len(frags[0].Samples) != 144 {
		t.Errorf("the fragment served: %v %+v", err, frags)
	}
	cached := h.cachePath("i1", "medium-h1080"+liveSuffix, "0")
	if b, err := os.ReadFile(cached); err != nil || !bytes.Equal(b, w.Body.Bytes()) {
		t.Errorf("not cached in the live folder: %v", err)
	}

	// From the cache: no new call; past the grid: 404, no call.
	n := len(fw.got(""))
	if w := get(h, "/api/play/i1/medium/2.m4s"+q); w.Code != 200 {
		t.Errorf("prefetched segment: %d", w.Code)
	}
	if w := get(h, "/api/play/i1/medium/4.m4s"+q); w.Code != 404 {
		t.Errorf("past the end: %d", w.Code)
	}
	settle(t, h.Live)
	if got := len(fw.got("")); got != n {
		t.Errorf("%d more calls for what is cached or past the end", got-n)
	}
	if runs := ffmpegRuns(t, state); len(runs) != 0 {
		t.Errorf("ffmpeg ran %d times", len(runs))
	}
	for _, c := range fw.got("") {
		if c.auth != "Bearer tok" {
			t.Errorf("a call without the token: %q", c.auth)
		}
	}
}

// What the worker answers reaches the player as the windows' failures did: a
// segment the source cannot make 404 (the player falls back at once), busy
// 503 with Retry-After once a playhead call made again after Retry-After is
// busy too, a deadline 504, a failure of every worker process 502; a refused
// or reset connection is made once more, and a fragment whose codec is not
// the rung's init's is asked for once more, then answered 502.
func TestLiveFailuresAnswerAsTheWindowsDid(t *testing.T) {
	captureLog(t)
	for _, tc := range []struct {
		name   string
		status int
		body   api.ErrorBody
		calls  int
		want   int
	}{
		{"source", 422, api.ErrorBody{Error: "undecodable", Class: api.ClassSource, Scope: api.ScopeSegment}, 1, 404},
		{"circuit", 422, api.ErrorBody{Error: "open", Class: api.ClassCircuit, RetryAfterMs: 30000}, 1, 404},
		{"busy", 503, api.ErrorBody{Error: "no slot", Class: api.ClassBusy, RetryAfterMs: 20}, 2, 503},
		{"deadline", 504, api.ErrorBody{Error: "late", Class: api.ClassDeadline}, 1, 504},
		{"internal", 502, api.ErrorBody{Error: "crashed", Class: api.ClassInternal}, 1, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWorker(t)
			fw.set(func(f *fakeWorker) {
				f.answer = func(c workerCall) (int, api.ErrorBody) { return tc.status, tc.body }
			})
			h, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
			refused := testutil.ToFloat64(metrics.LiveWorkerFallbacks.WithLabelValues("refused"))
			w := get(h, "/api/play/i1/high/0.m4s?lw=1")
			if w.Code != tc.want {
				t.Errorf("answered %d %q, want %d", w.Code, w.Body, tc.want)
			}
			if tc.want == 503 && w.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if tc.want == 404 && testutil.ToFloat64(metrics.LiveWorkerFallbacks.WithLabelValues("refused")) != refused+1 {
				t.Error("the refusal is not counted")
			}
			settle(t, h.Live)
			if n := len(segmentCalls(fw, api.TrackVideo)); n != tc.calls {
				t.Errorf("%d calls, want %d", n, tc.calls)
			}
		})
	}

	t.Run("busy once", func(t *testing.T) {
		fw := newFakeWorker(t)
		var busy atomic.Int32
		busy.Store(1)
		fw.set(func(f *fakeWorker) {
			f.answer = func(c workerCall) (int, api.ErrorBody) {
				if c.req.Priority == api.PriorityPlayhead && busy.Add(-1) >= 0 {
					return 503, api.ErrorBody{Error: "no slot", Class: api.ClassBusy, RetryAfterMs: 30}
				}
				return 0, api.ErrorBody{}
			}
		})
		h, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
		if w := get(h, "/api/play/i1/high/0.m4s?lw=1"); w.Code != 200 {
			t.Errorf("after a retry: %d %q", w.Code, w.Body)
		}
		settle(t, h.Live)
		if calls := segmentCalls(fw, api.TrackVideo); calls[1].at.Sub(calls[0].at) < 30*time.Millisecond {
			t.Error("made again before Retry-After")
		}
	})

	t.Run("connection reset", func(t *testing.T) {
		fw := newFakeWorker(t)
		fw.set(func(f *fakeWorker) { f.hangUp = 1 })
		h, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
		h.Live.unreachableRetry = 10 * time.Millisecond
		if w := get(h, "/api/play/i1/high/0.m4s?lw=1"); w.Code != 200 {
			t.Errorf("after a reset connection: %d %q", w.Code, w.Body)
		}
		settle(t, h.Live)
	})

	t.Run("codec", func(t *testing.T) {
		fw := newFakeWorker(t)
		h, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
		if w := get(h, "/api/play/i1/high/init.mp4?lw=1"); w.Code != 200 {
			t.Fatalf("init: %d", w.Code)
		}
		fw.set(func(f *fakeWorker) { f.pps = fakePPS2 })
		n := len(segmentCalls(fw, api.TrackVideo))
		if w := get(h, "/api/play/i1/high/0.m4s?lw=1"); w.Code != 502 {
			t.Errorf("a fragment of another codec: %d", w.Code)
		}
		settle(t, h.Live)
		// The playhead call and the one asked once more; nothing ahead of a
		// failure.
		if got := len(segmentCalls(fw, api.TrackVideo)) - n; got != 2 {
			t.Errorf("%d calls, want 2", got)
		}
		if statOK(h.cachePath("i1", "high"+liveSuffix, "0")) {
			t.Error("the fragment of another codec was cached")
		}
		fw.set(func(f *fakeWorker) { f.pps = fakePPS })
		if w := get(h, "/api/play/i1/high/0.m4s?lw=1"); w.Code != 200 {
			t.Errorf("the rung's codec again: %d", w.Code)
		}
		settle(t, h.Live)
	})
}

// An original's audio on the live pipeline: its stereo AAC rendition is the
// worker's, POST /v1/init and a job per segment of the track (its ordinal
// among the audio streams) at 160 kb/s, the next three asked for ahead.
func TestLiveAudioOfAnOriginalIsAWorkerJob(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	h, state, file := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
	if w := get(h, "/api/play/i1/audio/1/init.mp4?lw=1"); w.Code != 200 {
		t.Fatalf("init: %d %q", w.Code, w.Body)
	}
	w := get(h, "/api/play/i1/audio/1/0.m4s?lw=1")
	if w.Code != 200 {
		t.Fatalf("segment: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	init := fw.got("/v1/init")[0].req
	if init.Track != api.TrackAudio || init.Audio.Stream != 1 || init.Audio.BitrateKbps != 160 || init.Source.Path != file {
		t.Errorf("audio init %+v %+v", init, init.Audio)
	}
	segs := byIndex(segmentCalls(fw, api.TrackAudio))
	if len(segs) != 4 || segs[0].req.Segment.Index != 0 || segs[0].req.Segment.End != 6 || segs[3].req.Segment.Index != 3 {
		t.Errorf("audio segment calls %+v", segs)
	}
	frags, _, err := cmaf.ReadFragments(w.Body.Bytes(), nil)
	if err != nil || len(frags) != 1 || len(frags[0].Samples) != 281 {
		t.Errorf("audio fragment: %v %+v", err, frags)
	}
	if w := get(h, "/api/play/i1/audio/2/0.m4s?lw=1"); w.Code != 404 {
		t.Errorf("a track the source has not: %d", w.Code)
	}
	if len(ffmpegRuns(t, state)) != 0 {
		t.Error("ffmpeg ran")
	}
}

// liveMedia is an HLSHandler serving the real package under testdata/media
// (its original retired) with the fake worker in mode, ffmpeg a fake that
// fails.
func liveMedia(t *testing.T, fw *fakeWorker, mode LiveWorkerMode) *HLSHandler {
	t.Helper()
	f := newFakeLibrary(t)
	f.setItem(catalog.Playback{ItemID: mediaItem, Package: &catalog.PackageRef{VersionID: mediaVersion, Dir: mediaDir(t), Record: "package.json"}})
	h := &HLSHandler{Catalog: noAsset(t), Packages: f.resolver(), FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"), CacheDir: t.TempDir(),
		Live: fw.live(mode)}
	quiesce(t, h)
	return h
}

// A package's video on the live pipeline is a worker job on the package's
// top rendition, with the manifest's frame rate; its audio is the
// rendition's own AAC, remuxed in Go: the init the rendition's, each segment
// the rendition's frames whose middle falls in [6k, 6k+6), on their own
// timeline, so the segments tile the rendition — every frame once, each
// segment's decode time the previous one's end. No worker job for the audio.
func TestLivePackageAudioIsTheRenditionsOwn(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	h := liveMedia(t, fw, LiveWorkerOn)
	const q = "?caps=avc,aac&lw=1&v=" + mediaVersion
	dir := filepath.Join(mediaDir(t), "hls")

	if w := get(h, "/api/play/"+mediaItem+"/high/0.m4s"+q); w.Code != 200 {
		t.Fatalf("video segment: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	v := segmentCalls(fw, api.TrackVideo)[0].req
	if v.Source.Rendition != filepath.Join(dir, "v0", "playlist.m3u8") || v.Source.FrameRate != "24000/1001" || v.Source.Path != "" ||
		v.Video.Width != 128 || v.Video.Height != 72 {
		t.Errorf("video request %+v %+v", v.Source, v.Video)
	}

	init := get(h, "/api/play/"+mediaItem+"/audio/0/init.mp4"+q)
	want, _ := os.ReadFile(filepath.Join(dir, "a0", "init.mp4"))
	if init.Code != 200 || !bytes.Equal(init.Body.Bytes(), want) {
		t.Fatalf("audio init: %d, the rendition's: %v", init.Code, bytes.Equal(init.Body.Bytes(), want))
	}
	tr, _ := cmaf.ReadInit(want)
	var all []cmaf.FragSample
	for i := 1; i <= 4; i++ {
		b, _ := os.ReadFile(filepath.Join(dir, "a0", "seg-0000"+strconv.Itoa(i)+".m4s"))
		frags, _, err := cmaf.ReadFragments(b, tr.Trex)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frags {
			all = append(all, f.Samples...)
		}
	}
	var got []cmaf.FragSample
	var end int64 = -1
	for k := 0; k < 4; k++ {
		w := get(h, "/api/play/"+mediaItem+"/audio/0/"+strconv.Itoa(k)+".m4s"+q)
		if w.Code != 200 {
			t.Fatalf("audio segment %d: %d %q", k, w.Code, w.Body)
		}
		frags, _, err := cmaf.ReadFragments(w.Body.Bytes(), tr.Trex)
		if err != nil || len(frags) != 1 {
			t.Fatalf("audio segment %d: %v", k, err)
		}
		f := frags[0]
		if end >= 0 && int64(f.BaseDecodeTime) != end {
			t.Errorf("segment %d starts at %d, the previous one ends at %d", k, f.BaseDecodeTime, end)
		}
		end = f.DecodeEnd()
		if first := float64(f.Samples[0].PTS+tr.EditShift) / float64(tr.Timescale); k > 0 && (first < float64(6*k)-0.0215 || first > float64(6*k)+0.0215) {
			t.Errorf("segment %d starts at %.4f s", k, first)
		}
		got = append(got, f.Samples...)
	}
	if len(got) != len(all) {
		t.Fatalf("%d frames in the segments, the rendition has %d", len(got), len(all))
	}
	for i := range all {
		if got[i].DTS != all[i].DTS || !bytes.Equal(got[i].Data, all[i].Data) {
			t.Fatalf("frame %d: %d, the rendition's %d", i, got[i].DTS, all[i].DTS)
		}
	}
	if w := get(h, "/api/play/"+mediaItem+"/audio/0/4.m4s"+q); w.Code != 404 {
		t.Errorf("past the end: %d", w.Code)
	}
	if n := len(segmentCalls(fw, api.TrackAudio)) + len(fw.got("/v1/init")); n != 0 {
		t.Errorf("%d worker calls for the package's audio", n)
	}
}

// With the worker not ready a new session plays on ffmpeg, as before, and the
// fallback is counted; the master writes no pin.
func TestLiveUnreadyWorkerSendsNewSessionsToFFmpeg(t *testing.T) {
	captureLog(t)
	inPlaceWarms(t)
	fw := newFakeWorker(t)
	fw.set(func(f *fakeWorker) { f.ready = http.StatusServiceUnavailable })
	h, state, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
	before := testutil.ToFloat64(metrics.LiveWorkerFallbacks.WithLabelValues("unready"))
	w := get(h, "/api/play/i1/master.m3u8?caps=avc,aac")
	if w.Code != 200 || strings.Contains(w.Body.String(), "lw=") {
		t.Fatalf("master: %d\n%s", w.Code, w.Body)
	}
	if testutil.ToFloat64(metrics.LiveWorkerFallbacks.WithLabelValues("unready")) != before+1 {
		t.Error("the fallback is not counted")
	}
	if len(ffmpegRuns(t, state)) == 0 || len(fw.got("/v1/segment"))+len(fw.got("/v1/init")) != 0 {
		t.Error("the warm did not go to ffmpeg")
	}
}

// A session keeps the pipeline it began on, whichever way the flag turns: one
// pinned to the worker stays there with the mode off, one begun on ffmpeg
// stays there with the mode on.
func TestLiveSessionsKeepTheirPipeline(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	h, state, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOff)
	if w := get(h, "/api/play/i1/high/0.m4s?lw=1"); w.Code != 200 || w.Body.String() == "seg" {
		t.Errorf("a pinned segment with the mode off: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	if len(ffmpegRuns(t, state)) != 0 {
		t.Error("ffmpeg made a pinned segment")
	}
	h.Live = fw.live(LiveWorkerOn)
	if w := get(h, "/api/play/i1/high/0.m4s"); w.Code != 200 || w.Body.String() != "seg" {
		t.Errorf("a segment of a session begun on ffmpeg, the mode on: %d %q", w.Code, w.Body)
	}
}

// LIVE_WORKER_MODE shadow: ffmpeg serves, and the worker is asked for the
// segment ffmpeg served at prefetch priority; what comes back is checked
// (here, a rung whose codec changes) and dropped.
func TestLiveShadowAsksTheWorkerForWhatFFmpegServed(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	h, state, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerShadow)
	ok := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("video", "ok"))
	codec := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("video", "codec"))
	if w := get(h, "/api/play/i1/high/0.m4s"); w.Code != 200 || w.Body.String() != "seg" {
		t.Fatalf("segment: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	calls := segmentCalls(fw, api.TrackVideo)
	if len(calls) != 1 || calls[0].req.Priority != api.PriorityPrefetch || calls[0].req.Segment.Index != 0 {
		t.Fatalf("shadow calls %+v", calls)
	}
	if testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("video", "ok")) != ok+1 {
		t.Error("the check is not counted")
	}
	// Once per segment.
	get(h, "/api/play/i1/high/0.m4s")
	settle(t, h.Live)
	if n := len(segmentCalls(fw, api.TrackVideo)); n != 1 {
		t.Errorf("%d shadow calls for one segment", n)
	}
	fw.set(func(f *fakeWorker) { f.pps = fakePPS2 })
	get(h, "/api/play/i1/high/1.m4s")
	settle(t, h.Live)
	if testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("video", "codec")) != codec+1 {
		t.Error("a rung's codec change is not reported")
	}
	if statOK(h.cachePath("i1", "high"+liveSuffix, "0")) {
		t.Error("the shadow's segment was kept")
	}
	if len(ffmpegRuns(t, state)) != 1 {
		t.Errorf("ffmpeg runs %d, want 1 (one window)", len(ffmpegRuns(t, state)))
	}
	// Audio too.
	if w := get(h, "/api/play/i1/audio/0/0.m4s"); w.Code != 200 {
		t.Fatalf("audio: %d", w.Code)
	}
	settle(t, h.Live)
	if a := segmentCalls(fw, api.TrackAudio); len(a) != 1 || a[0].req.Priority != api.PriorityPrefetch {
		t.Errorf("audio shadow calls %+v", a)
	}
}

// With LIVE_WORKER_MODE on the progressive transcode answers 404; the
// original's byte ranges stay.
func TestLiveProgressiveTranscodeIsGone(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	root, src := mediaFile(t, "film.mp4")
	if err := os.WriteFile(src, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	ph := &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"),
		FFprobeBin: probeFFprobe(t, liveProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264", 1280, 720, "aac", "10.0")), Live: fw.live(LiveWorkerOn)}
	for _, path := range []string{"/api/play/i1?t=4", "/api/play/i1?force_transcode=1"} {
		if w := getPlay(ph, path); w.Code != 404 || !strings.Contains(w.Body.String(), "progressive transcoding moved to HLS") {
			t.Errorf("%s: %d %q", path, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/play/i1", nil)
	r.Header.Set("Range", "bytes=2-4")
	rt := chi.NewRouter()
	rt.Get("/api/play/{itemId}", ph.Play)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "234" {
		t.Errorf("byte range: %d %q", w.Code, w.Body)
	}
}

// With LIVE_WORKER_MODE on an original's embedded subtitle is the worker's
// (POST /v1/subtitle, from ?t= on), cached; a stream it cannot make (422) is
// 502, as a failed extraction always was. A package's is its WebVTT shifted
// in Go: no ffmpeg, no worker.
func TestLiveSubtitles(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	root, src := mediaFile(t, "film.mkv")
	ph := &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"), CacheDir: t.TempDir(),
		Live: fw.live(LiveWorkerOn)}
	for i := 0; i < 2; i++ {
		w := getPlay(ph, "/api/play/i1/subtitles/2.vtt?t=13")
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "WEBVTT") || w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
			t.Fatalf("subtitle: %d %q", w.Code, w.Body)
		}
	}
	subs := fw.got("/v1/subtitle")
	if len(subs) != 1 || subs[0].sub.Source.Path != src || subs[0].sub.Stream != 2 || subs[0].sub.StartSec != 13 || subs[0].sub.DeadlineMs <= 100000 {
		t.Errorf("subtitle calls %+v", subs)
	}
	fw.set(func(f *fakeWorker) {
		f.answer = func(c workerCall) (int, api.ErrorBody) {
			return 422, api.ErrorBody{Error: "subtitle stream 3 is hdmv_pgs_subtitle: not text", Class: api.ClassSource, Scope: api.ScopeSource}
		}
	})
	if w := getPlay(ph, "/api/play/i1/subtitles/3.vtt"); w.Code != 502 {
		t.Errorf("a bitmap stream: %d", w.Code)
	}

	// A package's: shifted in Go.
	state := t.TempDir()
	l, _, pph := retiredLibrary(t, windowFFmpeg(t, state, 0, 4, ""))
	pph.Live = fw.live(LiveWorkerOn)
	if err := os.MkdirAll(filepath.Join(l.version(libNew), "subs"), 0o755); err != nil {
		t.Fatal(err)
	}
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nGone.\n\n00:00:12.500 --> 00:00:14.000 line:90%\nStill on.\n\n00:00:20.000 --> 00:00:21.250\nLater.\n"
	if err := os.WriteFile(filepath.Join(l.version(libNew), "subs", "2.vtt"), []byte(vtt), 0o644); err != nil {
		t.Fatal(err)
	}
	n := len(fw.got(""))
	w := getPlay(pph, "/api/play/"+libItem+"/subtitles/2.vtt?t=13")
	want := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000 line:90%\nStill on.\n\n00:00:07.000 --> 00:00:08.250\nLater.\n\n"
	if w.Code != 200 || w.Body.String() != want {
		t.Errorf("package subtitle: %d\n%q\nwant\n%q", w.Code, w.Body, want)
	}
	if len(ffmpegRuns(t, state)) != 0 || len(fw.got("")) != n {
		t.Error("a package's subtitle went through ffmpeg or the worker")
	}
}

// The grid is the on-the-fly playlist's: floor(d/6) segments of 6 s and a
// tail over 0.5 s.
func TestLiveGridIsThePlaylists(t *testing.T) {
	for _, tc := range []struct {
		ms        int64
		n         int
		lastStart float64
		lastEnd   float64
	}{
		{20021, 4, 18, 20.021}, {18400, 3, 12, 18}, {18600, 4, 18, 18.6}, {6000, 1, 0, 6}, {300, 0, 0, 0}, {24000, 4, 18, 24},
	} {
		if n := liveGrid(tc.ms); n != tc.n {
			t.Errorf("%d ms: %d segments, want %d", tc.ms, n, tc.n)
			continue
		}
		if tc.n == 0 {
			if _, _, _, ok := liveSpan(tc.ms, 0); ok {
				t.Errorf("%d ms: a segment", tc.ms)
			}
			continue
		}
		s, e, last, ok := liveSpan(tc.ms, tc.n-1)
		if !ok || !last || s != tc.lastStart || e != tc.lastEnd {
			t.Errorf("%d ms: last segment [%v, %v) last %v ok %v", tc.ms, s, e, last, ok)
		}
		if _, _, last, _ := liveSpan(tc.ms, 0); tc.n > 1 && last {
			t.Errorf("%d ms: the first segment is the last", tc.ms)
		}
		// The playlist lists as many.
		dur := float64(tc.ms) / 1000
		segCount := int(dur / float64(segmentSec))
		if dur-float64(segCount*segmentSec) > 0.5 {
			segCount++
		}
		if segCount != tc.n {
			t.Errorf("%d ms: the playlist lists %d", tc.ms, segCount)
		}
	}
}

// The pin is written onto the URIs as v= is: the query as it came, lw
// replaced by lw=1; withoutLivePin leaves a query that names none as it is.
func TestLivePinQueries(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "lw=1",
		"caps=avc&stream=tok":    "caps=avc&stream=tok&lw=1",
		"lw=0&q=low":             "q=low&lw=1",
		"q=low&lw=1&v=x":         "q=low&v=x&lw=1",
		"lw&caps=hvc%3A1080,aac": "caps=hvc%3A1080,aac&lw=1",
	} {
		if got := withLivePin(in); got != want {
			t.Errorf("withLivePin(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"": "", "caps=avc&lwx=1": "caps=avc&lwx=1", "q=low&lw=1": "q=low", "lw=1": ""} {
		if got := withoutLivePin(in); got != want {
			t.Errorf("withoutLivePin(%q) = %q, want %q", in, got, want)
		}
	}
}

// LIVE_WORKER_MODE reads off, shadow and on; anything else is off.
func TestParseLiveWorkerMode(t *testing.T) {
	for in, want := range map[string]LiveWorkerMode{"": LiveWorkerOff, "off": LiveWorkerOff, "ON": LiveWorkerOn, " shadow ": LiveWorkerShadow} {
		if got, err := ParseLiveWorkerMode(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if got, err := ParseLiveWorkerMode("auto"); err == nil || got != LiveWorkerOff {
		t.Errorf("auto: %v %v", got, err)
	}
}

// A request that gives up leaves the job it joined, and a job nobody waits
// for any more ends; the next request starts another.
func TestLiveJobsEndWhenNobodyWaits(t *testing.T) {
	captureLog(t)
	fw := newFakeWorker(t)
	release := make(chan struct{})
	fw.set(func(f *fakeWorker) {
		f.answer = func(c workerCall) (int, api.ErrorBody) {
			if c.req.Priority == api.PriorityPlayhead {
				<-release
			}
			return 0, api.ErrorBody{}
		}
	})
	h, _, _ := liveOriginal(t, hevcFilm, fw, LiveWorkerOn)
	j, _ := h.liveVideoJob(&source{key: "i1", file: filepath.Join(h.MediaRoot, "film.mkv"), probe: &Probe{DurationMs: 20021, Width: 1920, Height: 1080}},
		"high", QualityLadder["high"], 0, 0, api.PriorityPlayhead)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.ensureLive(ctx, j); err == nil {
		t.Fatal("a request that gave up got its segment")
	}
	close(release)
	settle(t, h.Live)
	if statOK(j.segPath) {
		t.Error("a job nobody waited for was kept")
	}
	if err := h.ensureLive(context.Background(), j); err != nil || !statOK(j.segPath) {
		t.Errorf("the next request: %v", err)
	}
}
