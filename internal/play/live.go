package play

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"
	"github.com/zaentrum/live-worker/cmaf"

	"github.com/zaentrum/chino-stream/internal/metrics"
)

// The live pipeline of the on-the-fly HLS routes (liveworker.go): a session's
// requests carry lw=1, and each init and segment is the live worker's (a
// video rung, an original's audio) or a Go remux (a package's audio, the
// stream copy), cached as the windows were, in folders of its own.

// liveSuffix ends the cache folders of the live pipeline ("high-lw1",
// "audio-0-lw1", "copy-lw1", "sub-lw1"): a flag change never serves one
// pipeline's segment under the other's init.
const liveSuffix = "-lw1"

// livePinParam is the query parameter that pins a session to the live
// pipeline: lw=1.
const livePinParam = "lw"

// livePrefetchAhead is how many segments after the one served are asked for
// ahead of the play head (about 18 s).
const livePrefetchAhead = 3

// liveWarmSegments is how many segments a master's or /prewarm's warm asks
// for, from t on.
const liveWarmSegments = 3

// isLivePinParam says whether a query's kv is the pin (lw=…).
func isLivePinParam(kv string) bool {
	return kv == livePinParam || strings.HasPrefix(kv, livePinParam+"=")
}

// hasLivePin says whether a raw query names lw.
func hasLivePin(raw string) bool {
	for _, kv := range strings.Split(raw, "&") {
		if isLivePinParam(kv) {
			return true
		}
	}
	return false
}

// withLivePin is the query a live session's master writes onto its URIs: the
// request's, any lw it had replaced by lw=1. The other parameters stay as
// they came, byte for byte.
func withLivePin(raw string) string {
	var out []string
	for _, kv := range strings.Split(raw, "&") {
		if kv != "" && !isLivePinParam(kv) {
			out = append(out, kv)
		}
	}
	return strings.Join(append(out, livePinParam+"=1"), "&")
}

// withoutLivePin is raw without lw: what a master that sends its session to
// ffmpeg writes, whatever the URL it was asked with said. raw as it came when
// it names none.
func withoutLivePin(raw string) string {
	if !hasLivePin(raw) {
		return raw
	}
	var out []string
	for _, kv := range strings.Split(raw, "&") {
		if kv != "" && !isLivePinParam(kv) {
			out = append(out, kv)
		}
	}
	return strings.Join(out, "&")
}

// pinned says whether r belongs to a session of the live pipeline: it carries
// lw=1, and there is a live worker. Without one lw means nothing, as before.
func (h *HLSHandler) pinned(r *http.Request) bool {
	return h.Live != nil && r.URL.Query().Get(livePinParam) == "1"
}

// liveGrid is the number of segments of the on-the-fly grid of a source of
// durationMs: floor(d/6) of 6 s and a tail when it is over 0.5 s, as the
// media playlists list them (Playlist, AudioPlaylist).
func liveGrid(durationMs int64) int {
	d := float64(durationMs) / 1000.0
	n := int(d / float64(segmentSec))
	if d-float64(n*segmentSec) > 0.5 {
		n++
	}
	return n
}

// liveSpan is segment k of the grid: [6k, min(6k+6, d)) seconds on the
// source's presentation, last for the last one listed. ok false past the end.
func liveSpan(durationMs int64, k int) (start, end float64, last, ok bool) {
	n := liveGrid(durationMs)
	if k < 0 || k >= n {
		return 0, 0, false, false
	}
	start = float64(k * segmentSec)
	end = math.Min(start+segmentSec, float64(durationMs)/1000.0)
	return start, end, k == n-1, true
}

// liveSegmentAt is the grid's segment holding t seconds, the last one past
// the end.
func liveSegmentAt(durationMs int64, t float64) int {
	k := 0
	if t > 0 {
		k = int(t / segmentSec)
	}
	return min(k, max(0, liveGrid(durationMs)-1))
}

// liveVideoSource is what a video job of src reads: the original, or the
// package's top video rendition with the manifest's frame rate (it goes into
// every segment's SPS; given, the worker need not read the whole rendition to
// work it out).
func liveVideoSource(src *source) api.Source {
	if !src.fromPackage() {
		return api.Source{Path: src.file}
	}
	v := src.mf.Renditions.Video[0]
	s := api.Source{Rendition: src.pkg.path(v.Dir, "playlist.m3u8")}
	if _, _, err := api.ParseFrameRate(v.FrameRate); err == nil {
		s.FrameRate = v.FrameRate
	}
	return s
}

// liveVideoRung is the rung ql of src for a device capped at maxHeight: the
// frame the master's RESOLUTION names (transcodeGeometry) and its BANDWIDTH
// less the audio (rungVideoBitrate), SDR. A source of unknown size gets the
// rung's height and the worker's aspect-kept width.
func liveVideoRung(src *source, ql Quality, maxHeight int) *api.Video {
	var w, h int
	if p := src.probe; p != nil {
		w, h = p.Width, p.Height
	}
	g := transcodeGeometry(ql, w, h, maxHeight)
	v := &api.Video{Width: g.w, Height: g.h, Tonemap: api.TonemapAuto}
	if v.Height <= 0 {
		v.Width, v.Height = 0, rungHeight(ql, maxHeight)
	}
	v.BitrateKbps = max(1, rungVideoBitrate(ql, w, h, maxHeight)/1000)
	return v
}

// rungHeight is the height a rung encodes when the source's size is unknown.
func rungHeight(ql Quality, maxHeight int) int {
	h := scaleTargetHeight(ql.Scale)
	if h <= 0 {
		h = 1080
	}
	if maxHeight > 0 && h > maxHeight {
		h = maxHeight
	}
	return h
}

// liveJob is one init or segment of a live rung: what to ask the worker and
// where its files go.
type liveJob struct {
	kind     string // api.KindInit or api.KindSegment
	initPath string
	segPath  string // "" for an init
	req      api.Request
}

func (j liveJob) key() string {
	if j.segPath != "" {
		return j.segPath
	}
	return j.initPath
}

func (j liveJob) ready() bool { return statOK(j.initPath) && (j.segPath == "" || statOK(j.segPath)) }

// liveVideoJob is the init (seg < 0) or segment seg of the rung quality of src
// for a device capped at maxHeight; ok false past the grid's end.
func (h *HLSHandler) liveVideoJob(src *source, quality string, ql Quality, maxHeight, seg int, priority string) (liveJob, bool) {
	ns := videoCacheNamespace(src, quality, ql, maxHeight) + liveSuffix
	j := liveJob{kind: api.KindInit, initPath: h.cachePath(src.key, ns, "init"), req: api.Request{
		Source: liveVideoSource(src), Track: api.TrackVideo, Video: liveVideoRung(src, ql, maxHeight), Priority: priority,
	}}
	return h.liveSegmentOf(j, src, ns, seg)
}

// liveAudioJob is the init (seg < 0) or segment seg of the stereo AAC
// rendition of the original's audio track audioIdx, at 160 kb/s.
func (h *HLSHandler) liveAudioJob(src *source, audioIdx, seg int, priority string) (liveJob, bool) {
	ns := "audio-" + strconv.Itoa(audioIdx) + liveSuffix
	j := liveJob{kind: api.KindInit, initPath: h.cachePath(src.key, ns, "init"), req: api.Request{
		Source: api.Source{Path: src.file}, Track: api.TrackAudio,
		Audio: &api.Audio{Stream: audioIdx, BitrateKbps: windowAudioBitrateBps / 1000}, Priority: priority,
	}}
	return h.liveSegmentOf(j, src, ns, seg)
}

func (h *HLSHandler) liveSegmentOf(j liveJob, src *source, ns string, seg int) (liveJob, bool) {
	if seg < 0 {
		return j, true
	}
	start, end, last, ok := liveSpan(src.probe.DurationMs, seg)
	if !ok {
		return j, false
	}
	j.kind, j.segPath = api.KindSegment, h.cachePath(src.key, ns, strconv.Itoa(seg))
	j.req.Segment = &api.Segment{Index: seg, Start: start, End: end, Last: last}
	return j, true
}

// ensureLive makes the job's files be there: the cache's, else the worker's,
// joining the job under way for them — one in flight per source, rung and
// segment, as the window productions were. A request that joined a prefetch
// the worker had no slot to spare for asks again at its own priority.
func (h *HLSHandler) ensureLive(ctx context.Context, j liveJob) error {
	for tries := 0; ; tries++ {
		if j.ready() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f := h.Live.flights.join(j.key(), j.req.Priority, false, func(fctx context.Context) error { return h.produceLive(fctx, j) })
		err := h.Live.flights.wait(ctx, f)
		again := tries < 2
		switch {
		case err == nil && j.ready():
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil && again:
			// Made, and swept already: once more.
		case again && errors.Is(err, context.Canceled):
			// The job joined ended for lack of interest just then.
		case again && f.priority == api.PriorityPrefetch && j.req.Priority == api.PriorityPlayhead && client.ClassOf(err) == api.ClassBusy:
			// A prefetch the worker had no slot to spare for.
		case err == nil:
			return errors.New("live: made, and gone from the cache")
		default:
			return err
		}
	}
}

// produceLive asks the worker for the job and stores what comes back. A
// fragment whose codec configuration is not the rung's init's would not decode
// with the init a player holds: it is refused and asked for once more.
func (h *HLSHandler) produceLive(ctx context.Context, j liveJob) error {
	t0 := time.Now()
	for attempt := 0; ; attempt++ {
		out, err := h.Live.output(ctx, j.kind, j.req)
		if err != nil {
			return err
		}
		err = h.storeLive(j, out)
		if err == nil && j.req.Segment != nil {
			path := "encode"
			if j.req.Track == api.TrackAudio {
				path = "audio"
			}
			metrics.ObserveSegment(path, time.Since(t0), j.req.Segment.End-j.req.Segment.Start)
		}
		if !errors.Is(err, errCodecMismatch) || attempt > 0 {
			return err
		}
		metrics.LiveWorkerRetries.WithLabelValues(j.kind, "codec").Inc()
		log.Printf("live %s %s: codec %s is not the rung's init's; asking once more", j.kind, j.key(), out.Codec)
	}
}

// storeLive stores a worker's answer: its init as the rung's init when the
// cache has none (it comes from the fragment's own encoder), and the
// fragment, once its codec hash is the rung's init's.
func (h *HLSHandler) storeLive(j liveJob, out *client.Output) error {
	got := out.Codec
	if got == "" {
		got = initCodecHash(out.Init)
	}
	want, err := h.rungInit(j.initPath, out.Init, got)
	if err != nil {
		return err
	}
	if got != "" && want != "" && got != want {
		return errCodecMismatch
	}
	if j.segPath == "" {
		return nil
	}
	return writeFileAtomic(j.segPath, out.Fragment)
}

// rungInit is the codec hash of the rung's init at path: the cached init's,
// else that of init (got), which is written there as the rung's.
func (h *HLSHandler) rungInit(path string, init []byte, got string) (string, error) {
	mu := h.lockFor("lwinit/" + path)
	mu.Lock()
	defer mu.Unlock()
	if have, ok := cachedInitHash(path); ok {
		return have, nil
	}
	if len(init) == 0 {
		return "", errors.New("live: the worker sent no init")
	}
	if err := writeFileAtomic(path, init); err != nil {
		return "", err
	}
	return got, nil
}

// initHashes memoises the codec hash of the cached inits, by path, size and
// mtime.
var initHashes sync.Map // map[string]initHashEntry

type initHashEntry struct {
	size  int64
	mtime time.Time
	hash  string
}

// cachedInitHash is the codec hash of the init cached at path; ok false when
// there is none.
func cachedInitHash(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return "", false
	}
	if v, ok := initHashes.Load(path); ok {
		if e := v.(initHashEntry); e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
			return e.hash, true
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return "", false
	}
	hash := initCodecHash(b)
	initHashes.Store(path, initHashEntry{size: st.Size(), mtime: st.ModTime(), hash: hash})
	return hash, true
}

// initCodecHash is the hash of an init's parameter sets (SPS/PPS or
// AudioSpecificConfig), as the worker's Live-Worker-Codec says it; "" when it
// cannot be read.
func initCodecHash(init []byte) string {
	t, err := cmaf.ReadInit(init)
	if err != nil {
		return ""
	}
	return t.CodecHash
}

// writeFileAtomic writes data to path through a temporary file and a rename.
func writeFileAtomic(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("live: nothing to write to " + filepath.Base(path))
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// prefetchLive asks for the job at prefetch priority, unless its files are
// there or a job is making them. It runs to its end apart from any request;
// one the worker has no slot to spare for is dropped (503 at once), not
// queued. Prefetches are asked for only by requests and warms: a session no
// segment was asked for gets none.
func (h *HLSHandler) prefetchLive(j liveJob) {
	j.req.Priority = api.PriorityPrefetch
	if j.ready() || h.Live.flights.running(j.key()) {
		return
	}
	h.Live.flights.join(j.key(), api.PriorityPrefetch, true, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, h.Live.prefetch+h.Live.grace)
		defer cancel()
		err := h.produceLive(ctx, j)
		if err != nil && client.ClassOf(err) != api.ClassBusy {
			log.Printf("live prefetch %s: %v", j.key(), err)
		}
		return err
	})
}

// serveLiveVideo answers init.mp4 (seg < 0) or segment seg of the rung quality
// of a live session, and asks for the next segments ahead of the play head.
func (h *HLSHandler) serveLiveVideo(w http.ResponseWriter, r *http.Request, src *source, quality string, ql Quality, maxHeight, seg int) {
	j, ok := h.liveVideoJob(src, quality, ql, maxHeight, seg, api.PriorityPlayhead)
	if !ok {
		http.Error(w, "segment past end", http.StatusNotFound)
		return
	}
	what := "segment"
	if seg < 0 {
		what = "init segment"
	}
	if err := h.ensureLive(r.Context(), j); err != nil {
		log.Printf("live %s %s/%s/%d: %v", what, src.key, videoCacheNamespace(src, quality, ql, maxHeight), seg, err)
		liveError(w, err, what)
		return
	}
	if seg < 0 {
		h.serveCached(w, r, j.initPath, "video/mp4")
		return
	}
	h.serveCached(w, r, j.segPath, "video/iso.segment")
	for k := seg + 1; k <= seg+livePrefetchAhead; k++ {
		if pj, ok := h.liveVideoJob(src, quality, ql, maxHeight, k, api.PriorityPrefetch); ok {
			h.prefetchLive(pj)
		}
	}
}

// serveLiveAudio answers init.mp4 (seg < 0) or segment seg of the audio
// rendition audioIdx of a live session: an original's track from the worker
// (and the next segments asked for ahead of the play head), a package's stereo
// rendition remuxed from its own AAC (remuxaudio.go).
func (h *HLSHandler) serveLiveAudio(w http.ResponseWriter, r *http.Request, src *source, audioIdx, seg int) {
	what := "audio segment"
	if seg < 0 {
		what = "audio init"
	}
	if src.fromPackage() {
		if audioIdx >= len(src.mf.Renditions.Audio) {
			http.Error(w, "the package has no audio rendition "+strconv.Itoa(audioIdx), http.StatusNotFound)
			return
		}
		path, err := h.ensurePackageAudio(r.Context(), src, audioIdx, seg)
		if err != nil {
			log.Printf("live %s %s/%d/%d: %v", what, src.key, audioIdx, seg, err)
			status := http.StatusBadGateway
			if errors.Is(err, errNoAudioFrames) || errors.Is(err, errPastEnd) {
				status = http.StatusNotFound
			}
			http.Error(w, what+" unavailable", status)
			return
		}
		ctype := "video/iso.segment"
		if seg < 0 {
			ctype = "video/mp4"
		}
		h.serveCached(w, r, path, ctype)
		return
	}
	if src.probe == nil || audioIdx >= len(src.probe.AudioTracks) {
		http.Error(w, "the source has no audio track "+strconv.Itoa(audioIdx), http.StatusNotFound)
		return
	}
	j, ok := h.liveAudioJob(src, audioIdx, seg, api.PriorityPlayhead)
	if !ok {
		http.Error(w, "segment past end", http.StatusNotFound)
		return
	}
	if err := h.ensureLive(r.Context(), j); err != nil {
		log.Printf("live %s %s/%d/%d: %v", what, src.key, audioIdx, seg, err)
		liveError(w, err, what)
		return
	}
	if seg < 0 {
		h.serveCached(w, r, j.initPath, "video/mp4")
		return
	}
	h.serveCached(w, r, j.segPath, "video/iso.segment")
	for k := seg + 1; k <= seg+livePrefetchAhead; k++ {
		if pj, ok := h.liveAudioJob(src, audioIdx, k, api.PriorityPrefetch); ok {
			h.prefetchLive(pj)
		}
	}
}

// ensurePackageAudio makes the init (seg < 0) or segment seg of the stereo
// rendition audioIdx of the package src be in the cache, remuxed from the
// package's own AAC, and returns its path.
func (h *HLSHandler) ensurePackageAudio(ctx context.Context, src *source, audioIdx, seg int) (string, error) {
	ns := "audio-" + strconv.Itoa(audioIdx) + liveSuffix
	leaf, kind := "init", "audio-init"
	build := func() ([]byte, error) { return packageAudioInit(src, audioIdx) }
	if seg >= 0 {
		leaf, kind = strconv.Itoa(seg), "audio-segment"
		build = func() ([]byte, error) { return packageAudioSegment(src, audioIdx, seg) }
	}
	path := h.cachePath(src.key, ns, leaf)
	err := h.ensureCached(ctx, path, func(w io.Writer) error {
		t0 := time.Now()
		b, err := build()
		if err != nil {
			return err
		}
		if seg >= 0 {
			if start, end, _, ok := liveSpan(src.probe.DurationMs, seg); ok {
				metrics.ObserveSegment("copy", time.Since(t0), end-start)
			}
		}
		_, err = w.Write(b)
		return err
	})
	countRemux(kind, err)
	return path, err
}

// countRemux counts a Go remux.
func countRemux(kind string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.Remuxes.WithLabelValues(kind, result).Inc()
}

// liveSession decides how a new on-the-fly session of src is served: with
// LIVE_WORKER_MODE on, on the live pipeline — the stream copy (copy) remuxed
// in Go when the original is one mp4ff reads, else the transcode ladder on the
// worker while the worker is ready. An original mp4ff cannot read takes the
// transcode ladder (remux), as Matroska always has. Not ready, the session
// goes to ffmpeg as before, and the fallback is counted. useCopy is what the
// codecs and the client say (Master); streamCopy is what the session gets.
func (h *HLSHandler) liveSession(ctx context.Context, src *source, useCopy bool) (streamCopy, live bool) {
	if !h.Live.On() {
		return useCopy, false
	}
	if useCopy && copyRemuxable(src) {
		return true, true
	}
	if h.Live.Ready(ctx) {
		return false, true
	}
	metrics.LiveWorkerFallbacks.WithLabelValues("unready").Inc()
	log.Printf("live worker not ready: %s plays on ffmpeg", src.label())
	return useCopy, false
}

// warmLive warms the start of a live session off the request path: the
// rung's init and its first segments from t, at prefetch priority, and the
// audio it starts on — an original's track alike, a package's stereo
// rendition remuxed, a package's companion as packaged (phase-1.md).
func (h *HLSHandler) warmLive(src *source, ql Quality, maxHeight int, start *audioChoice, tSec float64) {
	k0 := liveSegmentAt(src.probe.DurationMs, tSec)
	if j, ok := h.liveVideoJob(src, ql.Name, ql, maxHeight, -1, api.PriorityPrefetch); ok {
		h.prefetchLive(j)
	}
	for k := k0; k < k0+liveWarmSegments; k++ {
		if j, ok := h.liveVideoJob(src, ql.Name, ql, maxHeight, k, api.PriorityPrefetch); ok {
			h.prefetchLive(j)
		}
	}
	probe := src.probe
	switch {
	case start != nil && start.companion() && src.fromPackage():
		goWarm(func() { warmRendition(src.pkg, start.rendition, 0, true) })
	case probe != nil && len(probe.AudioTracks) > 0 && src.fromPackage():
		idx := defaultAudioIndex(probe.AudioTracks)
		goWarm(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for _, k := range append([]int{-1}, liveRange(probe.DurationMs, k0, liveWarmSegments)...) {
				if _, err := h.ensurePackageAudio(ctx, src, idx, k); err != nil {
					log.Printf("live warm audio %s/%d/%d: %v", src.key, idx, k, err)
					return
				}
			}
		})
	case probe != nil && len(probe.AudioTracks) > 0:
		idx := defaultAudioIndex(probe.AudioTracks)
		for _, k := range append([]int{-1}, liveRange(probe.DurationMs, k0, liveWarmSegments)...) {
			if j, ok := h.liveAudioJob(src, idx, k, api.PriorityPrefetch); ok {
				h.prefetchLive(j)
			}
		}
	}
}

// liveRange is the segments k0 … k0+n-1 of the grid that exist.
func liveRange(durationMs int64, k0, n int) []int {
	var out []int
	for k := k0; k < k0+n && k < liveGrid(durationMs); k++ {
		out = append(out, k)
	}
	return out
}

// tParam is a request's ?t= in seconds, 0 without one.
func tParam(r *http.Request) float64 {
	if v := r.URL.Query().Get("t"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}
