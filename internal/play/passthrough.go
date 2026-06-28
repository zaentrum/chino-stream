package play

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/go-chi/chi/v5"
)

// Stream-copy passthrough for legacy on-demand items whose codecs are
// already browser-compatible. ffmpeg still runs, but with `-c copy` —
// no re-encode, just container repackaging into CMAF segments. CPU
// cost drops by ~95 % vs the libx264 transcode rung, and the bytes
// the browser decodes are the source bytes verbatim.
//
// Segment boundaries align with source IDRs: a one-time `ffprobe
// -skip_frame nokey` enumerates keyframe positions and we group them
// into ≥ passTargetSec chunks. Plan + segments are cached on disk just
// like the transcode pipeline, so warm hits are pure file IO.

// passTargetSec is the target minimum segment duration. The last
// keyframe before this threshold rolls into the previous segment;
// the next keyframe starts a new one. Actual segment durations are
// therefore source-GOP-dependent — typically 6–10 s.
const passTargetSec = 6.0

type passSegment struct {
	StartSec float64 `json:"start"`
	DurSec   float64 `json:"dur"`
}

type passPlan struct {
	DurationMs int64         `json:"duration_ms"`
	Segments   []passSegment `json:"segments"`
}

// loadOrBuildPlan returns the cached segment plan for itemID, building
// one via ffprobe on a miss. The plan is small (~few KB per movie) so
// we just JSON-encode it next to the segment cache.
func (h *HLSHandler) loadOrBuildPlan(ctx context.Context, itemID, src string) (*passPlan, error) {
	planPath := h.cachePath(itemID, "copy-"+copyPipelineVersion, "plan")
	if b, err := os.ReadFile(planPath); err == nil {
		var p passPlan
		if json.Unmarshal(b, &p) == nil && len(p.Segments) > 0 {
			return &p, nil
		}
	}
	mu := h.lockFor("copyplan/" + itemID)
	mu.Lock()
	defer mu.Unlock()
	if b, err := os.ReadFile(planPath); err == nil {
		var p passPlan
		if json.Unmarshal(b, &p) == nil && len(p.Segments) > 0 {
			return &p, nil
		}
	}
	plan, err := buildPassPlan(ctx, h.FFprobeBin, src)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(planPath, b, 0o644); err != nil {
		return nil, err
	}
	return plan, nil
}

// buildPassPlan enumerates the video keyframes in src and groups them
// into segments of ≥ passTargetSec. The final segment runs to EOF.
//
// Keyframe extraction prefers the in-memory mp4ff path which reads the
// MP4 stss + stts boxes directly (~100 ms for a 4 GB remux). ffprobe
// fallback exists for non-MP4 containers but the on-demand /copy/ path
// is only viable when ffmpeg can stream-copy from the source, so in
// practice nearly everything that lands here is .mp4 / .m4v / .mov.
func buildPassPlan(ctx context.Context, ffprobeBin, src string) (*passPlan, error) {
	durMs, err := probeDurationMs(ctx, ffprobeBin, src)
	if err != nil {
		return nil, fmt.Errorf("probe duration: %w", err)
	}
	kf, err := keyframesViaMp4ff(src)
	if err != nil {
		log.Printf("mp4ff keyframes %s: %v — falling back to ffprobe", filepath.Base(src), err)
		kf, err = probeKeyframes(ctx, ffprobeBin, src)
		if err != nil {
			return nil, fmt.Errorf("probe keyframes: %w", err)
		}
	}
	if len(kf) == 0 {
		return nil, fmt.Errorf("source has no detectable keyframes")
	}
	durSec := float64(durMs) / 1000.0
	segs := make([]passSegment, 0, len(kf)/2+1)
	lastStart := kf[0]
	for i := 1; i < len(kf); i++ {
		if kf[i]-lastStart >= passTargetSec {
			segs = append(segs, passSegment{StartSec: lastStart, DurSec: kf[i] - lastStart})
			lastStart = kf[i]
		}
	}
	if durSec > lastStart+0.001 {
		segs = append(segs, passSegment{StartSec: lastStart, DurSec: durSec - lastStart})
	}
	return &passPlan{DurationMs: durMs, Segments: segs}, nil
}

// keyframesViaMp4ff reads keyframe timestamps from the MP4 sample
// tables directly: stss lists the 1-based sample numbers that are
// keyframes, stts gives a run-length-encoded sample-to-duration table,
// mdhd carries the track timescale. DecModeLazyMdat tells mp4ff to
// skip reading the mdat payload (which is the entire video bitstream —
// gigabytes), so this finishes in O(moov size) instead of O(file size).
// On a 4 GB HEVC remux this runs in ~100 ms vs ~20 s for the ffprobe
// -show_packets path.
func keyframesViaMp4ff(src string) ([]float64, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mf, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("mp4 decode: %w", err)
	}
	if mf.Moov == nil {
		return nil, errors.New("no moov")
	}
	for _, trak := range mf.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}
		if trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			continue
		}
		stbl := trak.Mdia.Minf.Stbl
		if stbl.Stss == nil {
			return nil, errors.New("video track has no stss (every frame would be a sync sample — fragmented MP4?)")
		}
		if stbl.Stts == nil || trak.Mdia.Mdhd == nil {
			return nil, errors.New("missing stts/mdhd")
		}
		ts := float64(trak.Mdia.Mdhd.Timescale)
		if ts == 0 {
			return nil, errors.New("zero timescale")
		}
		// Build cumulative time table: sampleStart[i] = total ticks
		// up to and including the start of sample i (0-based).
		// stts SampleCount[k] consecutive samples share SampleDelta[k].
		var totalSamples uint32
		for _, c := range stbl.Stts.SampleCount {
			totalSamples += c
		}
		sampleStart := make([]uint64, totalSamples)
		var cum uint64
		idx := uint32(0)
		for k := 0; k < len(stbl.Stts.SampleCount); k++ {
			count := stbl.Stts.SampleCount[k]
			delta := uint64(stbl.Stts.SampleTimeDelta[k])
			for i := uint32(0); i < count; i++ {
				sampleStart[idx] = cum
				cum += delta
				idx++
			}
		}
		out := make([]float64, 0, len(stbl.Stss.SampleNumber))
		for _, sn := range stbl.Stss.SampleNumber {
			si := int(sn) - 1 // stss is 1-based
			if si < 0 || si >= len(sampleStart) {
				continue
			}
			out = append(out, float64(sampleStart[si])/ts)
		}
		if len(out) == 0 {
			return nil, errors.New("stss empty")
		}
		return out, nil
	}
	return nil, errors.New("no video track")
}

// probeDurationMs reads the format duration via a small ffprobe call.
func probeDurationMs(ctx context.Context, ffprobeBin, src string) (int64, error) {
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		src,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var secs float64
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &secs)
	return int64(secs * 1000), nil
}

// probeKeyframes lists the PTS of every keyframe in the primary video
// stream. `-skip_frame nokey` skips non-keyframe decode entirely so
// this scales O(keyframes) rather than O(frames) — a 2 h movie with
// 4 s GOP takes ~1 s on a fast disk.
//
// Field name note: ffmpeg ≤ 5.x exposed `pkt_pts_time`; ffmpeg 6.0
// renamed it to `pts_time` and dropped the alias. The current
// chino-stream image runs ffmpeg 7, so we use the modern name. CSV
// output may include a trailing comma on the first row when ffprobe
// attaches side-data fields, so we split on comma and parse the first
// token rather than the raw line.
func probeKeyframes(ctx context.Context, ffprobeBin, src string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries", "frame=pts_time",
		"-of", "csv=p=0",
		src,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	kf := make([]float64, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l == "N/A" {
			continue
		}
		if i := strings.IndexByte(l, ','); i >= 0 {
			l = l[:i]
		}
		v, err := strconv.ParseFloat(l, 64)
		if err == nil {
			kf = append(kf, v)
		}
	}
	return kf, nil
}

// PassthroughPlaylist serves the single-rendition media playlist for a
// stream-copy passthrough. EXTINF durations come from the source's
// actual IDR spacing because -c copy can't re-time samples.
func (h *HLSHandler) PassthroughPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	src, _, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	plan, err := h.loadOrBuildPlan(r.Context(), itemID, src)
	if err != nil {
		log.Printf("passthrough plan %s: %v", itemID, err)
		http.Error(w, "plan failed", http.StatusBadGateway)
		return
	}
	q := r.URL.RawQuery
	if q != "" {
		q = "?" + q
	}
	maxDur := 0
	var body strings.Builder
	body.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"init.mp4%s\"\n", q))
	for n, seg := range plan.Segments {
		if d := int(seg.DurSec) + 1; d > maxDur {
			maxDur = d
		}
		body.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%d.m4s%s\n", seg.DurSec, n, q))
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")
	sb.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", maxDur))
	sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString(body.String())
	sb.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(sb.String()))
}

// PassthroughInit serves the CMAF init segment for the copy rendition.
// Produced once by a tiny ffmpeg run (-t 0.05 -c copy) that emits a
// valid init.mp4 with codec configs derived from the source's avcC /
// esds boxes.
func (h *HLSHandler) PassthroughInit(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	src, _, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	initPath := h.cachePath(itemID, "copy-"+copyPipelineVersion, "init")
	if err := h.ensurePassthroughInit(r.Context(), src, initPath); err != nil {
		log.Printf("passthrough init %s: %v", itemID, err)
		http.Error(w, "init failed", http.StatusBadGateway)
		return
	}
	h.serveCached(w, r, initPath, "video/mp4")
}

func (h *HLSHandler) ensurePassthroughInit(ctx context.Context, src, initPath string) error {
	if statOK(initPath) {
		return nil
	}
	mu := h.lockFor("copyinit/" + initPath)
	mu.Lock()
	defer mu.Unlock()
	if statOK(initPath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(initPath), "passinit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	// True passthrough: -c copy on both tracks. The caller (Master)
	// only routes here when the dispatch decided the audio is also
	// browser-OK at the client's reported channel count; multichannel
	// AAC and AC-3/EAC-3/DTS sources get the remux pipeline instead.
	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning",
		"-ss", "0",
		"-i", src,
		"-t", "0.05",
		"-map", "0:v:0", "-map", "0:a:0",
		"-c", "copy",
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmpDir, "seg_%d.m4s"),
		filepath.Join(tmpDir, "playlist.m3u8"),
	}
	if err := h.runFFmpeg(ctx, args, "passthrough-init "+filepath.Base(src)); err != nil {
		return err
	}
	produced := filepath.Join(tmpDir, "init.mp4")
	if !statOK(produced) {
		return fmt.Errorf("ffmpeg produced no init.mp4 for %s", src)
	}
	return os.Rename(produced, initPath)
}

// PassthroughSegment serves segment N from the plan. Cache miss runs
// one ffmpeg with -ss/-t/-c copy targeting that segment's exact
// keyframe range — no encoding work, just demuxing + repackaging.
func (h *HLSHandler) PassthroughSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	segStr := chi.URLParam(r, "seg")
	seg, err := strconv.Atoi(segStr)
	if err != nil || seg < 0 {
		http.Error(w, "bad segment", http.StatusBadRequest)
		return
	}
	src, _, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	plan, err := h.loadOrBuildPlan(r.Context(), itemID, src)
	if err != nil {
		log.Printf("passthrough plan %s: %v", itemID, err)
		http.Error(w, "plan failed", http.StatusBadGateway)
		return
	}
	if seg >= len(plan.Segments) {
		http.Error(w, "segment past end", http.StatusNotFound)
		return
	}
	cachePath := h.cachePath(itemID, "copy-"+copyPipelineVersion, strconv.Itoa(seg))
	if err := h.ensurePassthroughSegment(r.Context(), itemID, src, plan, seg, cachePath); err != nil {
		log.Printf("passthrough seg %s/%d: %v", itemID, seg, err)
		http.Error(w, "segment failed", http.StatusBadGateway)
		return
	}
	h.serveCached(w, r, cachePath, "video/iso.segment")
}

func (h *HLSHandler) ensurePassthroughSegment(ctx context.Context, itemID, src string, plan *passPlan, seg int, cachePath string) error {
	if statOK(cachePath) {
		return nil
	}
	mu := h.lockFor(fmt.Sprintf("copyseg/%s/%d", itemID, seg))
	mu.Lock()
	defer mu.Unlock()
	if statOK(cachePath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(cachePath), fmt.Sprintf("passseg%d-", seg))
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	s := plan.Segments[seg]
	// -copyts preserves source PTS in the muxer output, so the
	// segment's tfdt carries the absolute source-time start instead
	// of resetting to 0. hls.js uses tfdt to anchor samples on the
	// timeline, so any imprecision in EXTINF (which inevitably drifts
	// with -c copy because the muxer can't trim mid-frame) doesn't
	// translate into visible boundary glitches.
	//
	// %.6f precision on -ss / -t: my keyframe timestamps come from
	// the source's stts box (rational ticks / timescale), so 3-decimal
	// rounding could land us ~1/2 ms off a keyframe edge and the
	// fast-seek would skip an entire GOP — the user's "cuts frames"
	// symptom.
	// True stream-copy on both tracks. Master only dispatches here
	// when the client's caps allow the source audio as-is (codec OK
	// AND ≤2ch OR aacmc opt-in). Sources that don't fit go through
	// the remux pipeline in transcode.go which re-encodes audio to
	// stereo while keeping video lossless.
	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning",
		"-copyts",
		"-ss", fmt.Sprintf("%.6f", s.StartSec),
		"-i", src,
		"-t", fmt.Sprintf("%.6f", s.DurSec),
		"-map", "0:v:0", "-map", "0:a:0",
		"-c", "copy",
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", fmt.Sprintf("%.6f", s.DurSec+10.0),
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmpDir, "seg_%d.m4s"),
		filepath.Join(tmpDir, "playlist.m3u8"),
	}
	if err := h.runFFmpeg(ctx, args, fmt.Sprintf("passthrough seg %d %s", seg, filepath.Base(src))); err != nil {
		return err
	}
	produced := filepath.Join(tmpDir, "seg_0.m4s")
	if !statOK(produced) {
		return fmt.Errorf("ffmpeg produced no segment for seg %d", seg)
	}
	return os.Rename(produced, cachePath)
}
