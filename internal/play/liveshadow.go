package play

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"

	"github.com/zaentrum/chino-stream/internal/metrics"
)

// LIVE_WORKER_MODE shadow (phase-1.md, "Rollout", 2): chino-stream still
// serves ffmpeg's segments, and once it has served one it asks the live
// pipeline for the same segment — the worker at prefetch priority, which
// runs only on a slot to spare, or the Go remux — checks what comes back and
// drops it: a rung's codec configuration the same in every segment, the
// grid's boundary, as many frames as ffmpeg's segment has. The worker meets
// real titles and load, and no viewer depends on it. A segment is checked
// once (shadowTTL); at most shadowSlots checks run at a time, the rest are
// skipped.

const (
	shadowSlots = 2
	shadowTTL   = time.Hour
	// shadowSeenMax bounds the segments remembered as checked.
	shadowSeenMax = 4096
)

// shadowState is the shadow checks' bookkeeping.
type shadowState struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	codecs sync.Map // a rung's first codec hash, by source and rung
	slots  chan struct{}
	wg     sync.WaitGroup // the checks under way (tests wait for them)
}

func newShadowState() *shadowState {
	return &shadowState{seen: map[string]time.Time{}, slots: make(chan struct{}, shadowSlots)}
}

// shadowOnce runs check for key — once within shadowTTL, off the request
// path, when a slot is free — and counts its result under track.
func (lw *LiveWorker) shadowOnce(track, key string, check func(ctx context.Context) string) {
	s := lw.shadow
	s.mu.Lock()
	now := time.Now()
	if at, ok := s.seen[key]; ok && now.Sub(at) < shadowTTL {
		s.mu.Unlock()
		return
	}
	if len(s.seen) >= shadowSeenMax {
		for k, at := range s.seen {
			if now.Sub(at) >= shadowTTL || len(s.seen) >= shadowSeenMax {
				delete(s.seen, k)
			}
		}
	}
	s.seen[key] = now
	s.mu.Unlock()
	select {
	case s.slots <- struct{}{}:
	default:
		metrics.LiveWorkerShadow.WithLabelValues(track, "skipped").Inc()
		s.mu.Lock()
		delete(s.seen, key) // not checked: a later request may
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), lw.prefetch+lw.grace)
		defer cancel()
		result := check(ctx)
		metrics.LiveWorkerShadow.WithLabelValues(track, result).Inc()
		if result != "ok" && result != "busy" {
			log.Printf("live shadow %s %s: %s", track, key, result)
		}
	}()
}

// shadowFailure is the result of a failed shadow call.
func shadowFailure(ctx context.Context, err error) string {
	if c := client.ClassOf(err); c != "" {
		return c
	}
	if errors.Is(err, errCodecMismatch) {
		return "codec"
	}
	if ctx.Err() != nil {
		return api.ClassDeadline
	}
	return "error"
}

// sameCodec says whether codec is the first one the rung (key) was seen with.
func (lw *LiveWorker) sameCodec(key, codec string) bool {
	first, _ := lw.shadow.codecs.LoadOrStore(key, codec)
	return first.(string) == codec
}

// shadowVideo checks the worker's segment seg of the rung quality against the
// one ffmpeg made: as many frames, within one (its windows cut at the frame
// after a boundary as often as at the one before).
func (h *HLSHandler) shadowVideo(src *source, quality string, ql Quality, maxHeight, seg int) {
	j, ok := h.liveVideoJob(src, quality, ql, maxHeight, seg, api.PriorityPrefetch)
	if !ok {
		return
	}
	ns := videoCacheQuality(quality, maxHeight)
	ffInit, ffSeg := h.cachePath(src.key, ns, "init"), h.cachePath(src.key, ns, strconv.Itoa(seg))
	h.Live.shadowOnce("video", j.key(), func(ctx context.Context) string {
		return h.Live.shadowCheck(ctx, j, src.key+"|"+ns, ffInit, ffSeg, sameFrames("vide", 1))
	})
}

// shadowAudio checks the worker's segment seg of the original's audio track
// audioIdx against the one ffmpeg made: as long, within two AAC frames.
func (h *HLSHandler) shadowAudio(src *source, audioIdx, seg int) {
	j, ok := h.liveAudioJob(src, audioIdx, seg, api.PriorityPrefetch)
	if !ok {
		return
	}
	ns := "audio-" + strconv.Itoa(audioIdx)
	ffInit, ffSeg := h.cachePath(src.key, ns, "init"), h.cachePath(src.key, ns, strconv.Itoa(seg))
	h.Live.shadowOnce("audio", j.key(), func(ctx context.Context) string {
		return h.Live.shadowCheck(ctx, j, src.key+"|"+ns, ffInit, ffSeg, sameLength("soun", 0.05))
	})
}

// shadowCheck asks the worker for j and compares: the boundary the grid's,
// the codec the rung's first, and what alike says of it and ffmpeg's segment.
func (lw *LiveWorker) shadowCheck(ctx context.Context, j liveJob, rung, ffInit, ffSeg string, alike shadowCompare) string {
	out, err := lw.output(ctx, api.KindSegment, j.req)
	if err != nil {
		return shadowFailure(ctx, err)
	}
	seg := j.req.Segment
	if r := out.Result; r.Job != "" && (r.Index != seg.Index || math.Abs(r.Start-seg.Start) > 1e-3 || math.Abs(r.End-seg.End) > 1e-3) {
		return "span"
	}
	codec := out.Codec
	if codec == "" {
		codec = initCodecHash(out.Init)
	}
	if !lw.sameCodec(rung, codec) {
		return "codec"
	}
	return compareShadow(j.key(), out.Init, out.Fragment, ffInit, ffSeg, alike)
}

// shadowCompare compares a segment's tracks with ffmpeg's: "" when alike, else
// what differs.
type shadowCompare func(got, want map[string]trackSpan) string

// sameFrames: the track of handler has as many frames, within tolerance.
func sameFrames(handler string, tolerance int) shadowCompare {
	return func(got, want map[string]trackSpan) string {
		if absInt(got[handler].frames-want[handler].frames) > tolerance {
			return strconv.Itoa(got[handler].frames) + " frames, ffmpeg's " + strconv.Itoa(want[handler].frames)
		}
		return ""
	}
}

// sameLength: the track of handler lasts as long, within tolerance seconds
// (a package's audio keeps its own sample rate, ffmpeg's re-encode is 48 kHz).
func sameLength(handler string, tolerance float64) shadowCompare {
	return func(got, want map[string]trackSpan) string {
		if math.Abs(got[handler].seconds-want[handler].seconds) > tolerance {
			return strconv.FormatFloat(got[handler].seconds, 'f', 3, 64) + " s, ffmpeg's " + strconv.FormatFloat(want[handler].seconds, 'f', 3, 64)
		}
		return ""
	}
}

// compareShadow is the shadow result of a segment (init, frag) against
// ffmpeg's cached one (alike decides; "ok" when ffmpeg's cannot be read).
func compareShadow(key string, init, frag []byte, ffInit, ffSeg string, alike shadowCompare) string {
	got, err := segmentTracks(init, frag)
	if err != nil || len(got) == 0 {
		return "error"
	}
	if want, ok := cachedSegmentTracks(ffInit, ffSeg); ok {
		if diff := alike(got, want); diff != "" {
			log.Printf("live shadow %s: %s", key, diff)
			return "frames"
		}
	}
	return "ok"
}

// shadowCopy checks the Go remux of segment seg of the stream copy against the
// one ffmpeg made: as many video samples, within three (ffmpeg's -t cut keeps
// the frames decoded before the next keyframe and presented after it).
func (h *HLSHandler) shadowCopy(src *source, seg int) {
	if !isoBMFF(src.probe.Container) {
		return
	}
	ns := "copy-" + copyPipelineVersion
	ffInit, ffSeg := h.cachePath(src.key, ns, "init"), h.cachePath(src.key, ns, strconv.Itoa(seg))
	key := src.key + "|copy|" + strconv.Itoa(seg)
	h.Live.shadowOnce("copy", key, func(ctx context.Context) string {
		ix, err := loadCopyIndex(src.file, src.probe.DurationMs)
		if err != nil {
			return "error"
		}
		b, err := ix.segment(seg)
		if err != nil {
			return "error"
		}
		return compareShadow(key, ix.init, b, ffInit, ffSeg, sameFrames("vide", 3))
	})
}

// shadowPackageAudio checks the Go remux of segment seg of the package's
// stereo rendition audioIdx against ffmpeg's re-encode of it: as long, within
// a tenth of a second (two frames of the rendition's).
func (h *HLSHandler) shadowPackageAudio(src *source, audioIdx, seg int) {
	if audioIdx >= len(src.mf.Renditions.Audio) {
		return
	}
	ns := "audio-" + strconv.Itoa(audioIdx)
	ffInit, ffSeg := h.cachePath(src.key, ns, "init"), h.cachePath(src.key, ns, strconv.Itoa(seg))
	key := src.key + "|" + ns + "|" + strconv.Itoa(seg)
	h.Live.shadowOnce("package-audio", key, func(ctx context.Context) string {
		init, err := packageAudioInit(src, audioIdx)
		if err != nil {
			return "error"
		}
		b, err := packageAudioSegment(src, audioIdx, seg)
		if err != nil {
			return "error"
		}
		return compareShadow(key, init, b, ffInit, ffSeg, sameLength("soun", 0.1))
	})
}

// trackSpan is one track of a segment: its frames and how long they last.
type trackSpan struct {
	frames  int
	seconds float64
}

// segmentTracks are the tracks of a segment's fragments by handler ("vide",
// "soun"), its tracks named by init.
func segmentTracks(init, segment []byte) (map[string]trackSpan, error) {
	in, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil {
		return nil, err
	}
	if in.Moov == nil {
		return nil, errors.New("no moov")
	}
	type track struct {
		handler   string
		timescale uint32
		trex      *mp4.TrexBox
	}
	tracks := map[uint32]track{}
	for _, t := range in.Moov.Traks {
		if t.Tkhd != nil && t.Mdia != nil && t.Mdia.Hdlr != nil && t.Mdia.Mdhd != nil {
			tracks[t.Tkhd.TrackID] = track{t.Mdia.Hdlr.HandlerType, t.Mdia.Mdhd.Timescale, trexOf(in.Moov, t.Tkhd.TrackID)}
		}
	}
	seg, err := mp4.DecodeFile(bytes.NewReader(segment))
	if err != nil {
		return nil, err
	}
	out := map[string]trackSpan{}
	for _, s := range seg.Segments {
		for _, f := range s.Fragments {
			if f.Moof == nil {
				continue
			}
			for _, traf := range f.Moof.Trafs {
				tr, ok := tracks[traf.Tfhd.TrackID]
				if !ok {
					continue
				}
				span := out[tr.handler]
				for _, trun := range traf.Truns {
					dur := trun.AddSampleDefaultValues(traf.Tfhd, tr.trex)
					span.frames += int(trun.SampleCount())
					if tr.timescale > 0 {
						span.seconds += float64(dur) / float64(tr.timescale)
					}
				}
				out[tr.handler] = span
			}
		}
	}
	return out, nil
}

// cachedSegmentTracks are the tracks of a cached segment.
func cachedSegmentTracks(initPath, segPath string) (map[string]trackSpan, bool) {
	init, err := os.ReadFile(initPath)
	if err != nil {
		return nil, false
	}
	seg, err := os.ReadFile(segPath)
	if err != nil {
		return nil, false
	}
	n, err := segmentTracks(init, seg)
	return n, err == nil
}

// sampleCounts counts the samples of a segment's fragments per track handler.
func sampleCounts(init, segment []byte) (map[string]int, error) {
	tracks, err := segmentTracks(init, segment)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for h, s := range tracks {
		out[h] = s.frames
	}
	return out, nil
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
