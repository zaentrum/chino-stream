package play

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// HLS-on-demand handler. Replaces the previous progressive-MP4 /play
// pipeline with a chunked HLS pipeline:
//
//	/play/{id}/master.m3u8     → variant master playlist (one quality
//	                             rung today; multi-bitrate in the future)
//	/play/{id}/{q}/index.m3u8  → media playlist (#EXT-X-MAP + #EXTINF list)
//	/play/{id}/{q}/init.mp4    → fMP4 initialization segment (ftyp+moov)
//	/play/{id}/{q}/{n}.m4s     → media segment N (fMP4 moof+mdat)
//
// Segment N covers source-time [N*segmentSec, (N+1)*segmentSec). The
// last segment may be shorter — the playlist's trailing #EXTINF reflects
// that. Each segment request runs a short ffmpeg (`-ss N*S -i src -t S
// … -movflags +cmaf+...`) and pipes the fMP4 segment to the client.
// Output is cached on disk under HLSCacheDir, keyed by item+quality+N,
// so subsequent requests for the same segment skip the transcode.
//
// Why HLS: the previous progressive pipeline kept one long-lived HTTP
// connection open for the entire movie. Chromium would close it after
// buffering "enough" → upstream ffmpeg got SIGKILLed → player saw
// premature EOF → recoverFault glitch every ~10 min. With short
// segments the connection lifetime is bounded (~5-8s of TCP per
// segment), so the heuristic never fires.
type HLSHandler struct {
	Catalog         *catalog.Client
	MediaRoot       string
	FFmpegBin       string
	FFprobeBin      string
	TranscodePreset string
	CacheDir        string

	// NVENC opt-in. When true, the transcode path uses CUDA hardware
	// decode + h264_nvenc encode and TranscodePreset is interpreted as
	// libx264's preset (used only when NVENC is unavailable). NVENC
	// presets (p1 ↔ p7) and CQ are passed through NVENCPreset / NVENCCQ.
	// Set from the USE_NVENC env var at startup. Mirroring the pod's
	// nvidia.com/gpu request: when the resource is granted, set this
	// true; otherwise leave it false and libx264 still works on any
	// node.
	UseNVENC    bool
	NVENCPreset string
	NVENCCQ     string

	// segmentLocks dedupes concurrent ffmpeg runs for the same
	// (item, quality, segment) — the first request transcodes and
	// writes to disk, others wait on the same mutex and read the
	// cached file. Without this we'd burn N× the CPU when N players
	// hit the same segment at the same time (e.g., everyone watching
	// the same live-shared moment).
	segmentLocks sync.Map // map[string]*sync.Mutex

	// windowAttempts caps the number of ffmpeg runs per (item, quality,
	// windowIdx) over this process's lifetime. NVENC/hwaccel decode
	// races have been observed to silently truncate a window mid-encode
	// (process exits 0 but writes < windowSize segments). We retry once
	// to clear the race; further partial outputs are most likely
	// deterministic (corrupt source, encoder rejecting the GOP), so we
	// cap and let the player ABR-fall to a lower rung. Keyed
	// `{itemID}/{quality}/{windowIdx}` — same key shape used for
	// segmentLocks so the two are easy to correlate in logs.
	windowAttempts sync.Map // map[string]int
}

// segmentSec is the target HLS segment length. 6s is the canonical HLS
// value: long enough that segment overhead (ffmpeg start, init parsing)
// stays a small fraction of the encoding work, short enough that the
// player can ABR-switch quality / seek with low latency.
const segmentSec = 6

// windowSize is how many segments a single ffmpeg invocation produces.
// Per-segment ffmpeg can't deliver clean boundaries: each fresh run
// reset the encoder, so the resulting segment durations didn't add up
// exactly (video ~6.006 s per segment from 23.976 fps frame timing,
// audio fractional AAC frames). With tfdt forced to integer seconds
// every segment overlapped its neighbour by 6 ms (video) or 37 ms
// (audio) — and MSE either dropped frames or clicked at every seam.
//
// One ffmpeg per window of segments keeps encoder state across the
// internal boundaries, so within a window the segments are byte-for-
// byte adjacent. Only the every-windowSize seam can still glitch (in
// practice the keyframe forced at the window boundary is also a clean
// IDR so the boundary stays clean too).
//
// 10 × 6 s = 60 s per ffmpeg invocation is the LAN-friendly default:
// long enough to amortize ffmpeg startup + colour-space probing, short
// enough that the first segment of a window arrives well inside the
// player's 5 min buffer budget.
const windowSize = 10

// maxWindowAttempts is the upper bound on ffmpeg invocations for a
// single video/audio window across this process's lifetime. First
// attempt is the cold transcode; one retry covers the
// NVENC/hwaccel-decode silent-truncation case we've seen in the wild
// for 4K HEVC sources. Beyond that, the source/encoder pair likely
// can't produce the requested segment at all and we surface 404 so
// the client falls back to a lower quality rung (the auto-fallback
// machinery in chino-androidtv / chino-web handles it gracefully).
const maxWindowAttempts = 2

// copyPipelineVersion is the on-disk schema version for the /copy/
// stream-copy cache. Bump this when the ffmpeg arguments produce
// bytes that won't append cleanly to MSE buffers generated by the
// previous version (codec config change, container flag change,
// audio downmix toggle, etc.). The version is suffixed onto the
// cache key in cachePath, so a bump shifts cold-rebuild responsibility
// to ffmpeg without manual disk cleanup.
const copyPipelineVersion = "v3"

// subPipelineVersion mirrors copyPipelineVersion for the embedded
// subtitle .vtt extracts. Bump on ffmpeg flag changes that affect
// the cue output.
const subPipelineVersion = "v1"

// HLS returns a sub-router with all HLS routes mounted under it. The
// caller wires this under `/api/play/{itemId}/...` so the chino-api
// proxy can forward those URLs straight through.
//
// The legacy quality-keyed routes (/{quality}/...) and the new
// rendition-keyed routes (/{rendId}/...) coexist. The Master handler
// dispatches based on whether the item has a completed package on
// disk and points the player at one set or the other; the player then
// follows whichever URIs the master gave it.
func (h *HLSHandler) Routes(r chi.Router) {
	r.Get("/master.m3u8", h.Master)
	// Fire-and-forget warm endpoint used by the Zap pager for cards
	// the user is likely to swipe to next. Returns 202 immediately;
	// the actual warm runs in a background goroutine off the warm-only
	// ffmpeg pool so it never starves real-request transcodes.
	r.Post("/prewarm", h.Prewarm)
	// Legacy on-demand quality ladder. Constrained so it can't collide
	// with the rendition routes below ("v0", "a0", ...).
	r.Get("/{quality:high|medium|low}/index.m3u8", h.Playlist)
	r.Get("/{quality:high|medium|low}/init.mp4", h.InitSegment)
	r.Get("/{quality:high|medium|low}/{seg:[0-9]+}.m4s", h.Segment)
	// Stream-copy passthrough — for items whose codecs are already
	// browser-compatible. ffmpeg runs but with -c copy, no re-encode.
	r.Get("/copy/index.m3u8", h.PassthroughPlaylist)
	r.Get("/copy/init.mp4", h.PassthroughInit)
	r.Get("/copy/{seg:[0-9]+}.m4s", h.PassthroughSegment)
	// Audio tracks (legacy): separate rendition group in HLS so the
	// player can switch language without restarting video.
	r.Get("/audio/{audioIdx:[0-9]+}/index.m3u8", h.AudioPlaylist)
	r.Get("/audio/{audioIdx:[0-9]+}/init.mp4", h.AudioInitSegment)
	r.Get("/audio/{audioIdx:[0-9]+}/{seg:[0-9]+}.m4s", h.AudioSegment)
	// Packaged CMAF (read from /var/lib/katalog/packages). Rendition
	// IDs are vN for video and aN for audio. Regex on rendId pins the
	// shape so paths can't escape these handlers.
	r.Get("/{rendId:[va][0-9]+}/playlist.m3u8", h.PackagedRenditionPlaylist)
	r.Get("/{rendId:[va][0-9]+}/iframes.m3u8", h.PackagedIframesPlaylist)
	r.Get("/{rendId:[va][0-9]+}/init.mp4", h.PackagedInitSegment)
	r.Get("/{rendId:[va][0-9]+}/seg-{seg:[0-9]+}.m4s", h.PackagedSegment)
	// Trickplay (scrub-preview thumbnails). Player loads the VTT once
	// then pulls one sprite per ~16.7 min window as the user scrubs.
	r.Get("/trickplay/thumbnails.vtt", h.PackagedTrickplayVTT)
	r.Get("/trickplay/sprite-{n:[0-9]+}.jpg", h.PackagedTrickplaySprite)
}

// probeCache memoises ffprobe results to skip the ~150-250 ms per-call
// cost when multiple endpoints fire close together for the same item
// (Info → Master → Playlist → Init → Segment all want the same Probe).
// Entries live for probeCacheTTL after their source file's mtime stops
// changing — long enough to amortize a page load, short enough that an
// operator's `mv newfile.mp4 oldfile.mp4` shows up on the next view.
type probeEntry struct {
	probe Probe
	mtime time.Time
	at    time.Time
}

var (
	probeCacheMu sync.RWMutex
	probeCache   = make(map[string]probeEntry)
)

const probeCacheTTL = 5 * time.Minute

// resolveSource is shared with the progressive Play handler — looks up
// the asset path, validates against MediaRoot, returns the absolute
// path on disk and the ffprobe result (so the playlist knows duration).
// ffprobe is in-memory cached per source path; cache invalidation is
// driven by source mtime + TTL.
func (h *HLSHandler) resolveSource(ctx context.Context, itemID, bearer string) (string, *Probe, error) {
	path, err := h.Catalog.PrimaryAssetPath(ctx, itemID, bearer)
	if err != nil {
		return "", nil, err
	}
	clean := filepath.Clean(path)
	if h.MediaRoot != "" && !strings.HasPrefix(clean, filepath.Clean(h.MediaRoot)+string(os.PathSeparator)) {
		// Name both sides. This message was the only symptom of a
		// misconfiguration that made playback impossible for every asset, and
		// it said neither which path nor which root.
		return "", nil, fmt.Errorf(
			"asset path %q is outside the configured media root %q — "+
				"chino-stream cannot open it", clean, filepath.Clean(h.MediaRoot))
	}
	st, err := os.Stat(clean)
	if err != nil {
		return "", nil, err
	}
	if p, ok := lookupProbe(clean, st.ModTime()); ok {
		return clean, p, nil
	}
	probe, err := RunFFprobe(ctx, h.FFprobeBin, clean)
	if err != nil {
		return clean, nil, err
	}
	storeProbe(clean, st.ModTime(), probe)
	return clean, &probe, nil
}

func lookupProbe(path string, mtime time.Time) (*Probe, bool) {
	probeCacheMu.RLock()
	e, ok := probeCache[path]
	probeCacheMu.RUnlock()
	if !ok || !e.mtime.Equal(mtime) || time.Since(e.at) > probeCacheTTL {
		return nil, false
	}
	p := e.probe
	return &p, true
}

func storeProbe(path string, mtime time.Time, probe Probe) {
	probeCacheMu.Lock()
	probeCache[path] = probeEntry{probe: probe, mtime: mtime, at: time.Now()}
	probeCacheMu.Unlock()
}

// warmCopy kicks off the per-segment plan + init.mp4 generation for an
// item that the master decided would route through /copy/. Run in a
// goroutine from Master() so the client's follow-up /copy/index.m3u8
// and /copy/init.mp4 fetches find the work already done. Uses
// context.Background() because the original request will return long
// before the warm finishes.
//
// 3-minute timeout: the keyframe extraction via mp4ff is sub-second
// even for 4 GB remuxes, and the init.mp4 ffmpeg run is ~200 ms, so 3
// min is plenty of headroom. Set high enough that an NFS hiccup on
// the first read of a cold file doesn't kill the warm prematurely.
func (h *HLSHandler) warmCopy(itemID, src string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = withWarmContext(ctx)
	if _, err := h.loadOrBuildPlan(ctx, itemID, src); err != nil {
		log.Printf("warmCopy plan %s: %v", itemID, err)
		return
	}
	initPath := h.cachePath(itemID, "copy-"+copyPipelineVersion, "init")
	if err := h.ensurePassthroughInit(ctx, src, initPath); err != nil {
		log.Printf("warmCopy init %s: %v", itemID, err)
	}
}

// warmTranscode runs ensureVideoWindow + ensureAudioWindow for window 0
// of the transcode ladder so the client's first /high/init.mp4 + seg-0
// request lands on warm cache. Window 0 covers segments 0-9 (~60 s),
// which is more buffer than the player needs to start.
//
// 5-minute timeout: an HEVC → H.264 transcode of a 1080p source at
// `-preset veryfast` runs at roughly 0.5-1× realtime on the cluster's
// CPU pool, so producing 60 s of output can take ~60-120 s. Anything
// shorter risks killing the goroutine mid-window and leaving the cache
// half-populated — the request-context ffmpeg path then has to restart
// from scratch instead of inheriting partial work.
func (h *HLSHandler) warmTranscode(itemID, src string, probe *Probe, ql Quality, maxHeight int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx = withWarmContext(ctx)
	isHDR := probe != nil && probe.IsHDR()
	if err := h.ensureVideoWindow(ctx, itemID, src, ql.Name, ql, isHDR, probe, 0, 0, maxHeight); err != nil {
		log.Printf("warmTranscode video %s: %v", itemID, err)
		return
	}
	if probe != nil && len(probe.AudioTracks) > 0 {
		audioIdx := defaultAudioIndex(probe.AudioTracks)
		if err := h.ensureAudioWindow(ctx, itemID, src, audioIdx, probe, 0, 0); err != nil {
			log.Printf("warmTranscode audio %s: %v", itemID, err)
		}
	}
}

// Master returns the master playlist. Two source-of-truth modes:
//
//   - Packaged: the analyzer has already written a CMAF tree under
//     /var/lib/katalog/packages/{id}/ and dropped a .complete sentinel.
//     We serve shaka's master.m3u8 unchanged (only the URI query
//     strings are rewritten so the player keeps the inbound ?stream=
//     token on each rendition fetch).
//
//   - Legacy on-demand: no completed package → emit a SINGLE video
//     variant pointing at the rung selected by `?q=` (default high).
//     The old multi-variant ladder caused hls.js to settle on the
//     medium rung even on fast LANs because ffmpeg's realtime output
//     bottlenecked its bandwidth estimate. Quality switches are now
//     driven by the client changing the `?q=` query and reloading the
//     master — a hard src change in hls.js — which the existing
//     pendingSeekRef path already handles for resume position.
//
// Items move from legacy to packaged when an operator POSTs the admin
// packaging endpoint; existing in-flight viewers keep using legacy on
// the URLs they already fetched, new viewers get packaged.
func (h *HLSHandler) Master(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	// Caps come from ?caps= on the client (chino-web's MediaCapabilities
	// probe). We need them BEFORE the packaged check too — if the
	// packaged master only advertises HEVC and the client can't decode
	// hvc, we'd ship bytes the device renders as a black screen and
	// the player would loop until the user gave up. Skip packaged in
	// that case and fall through to the on-demand transcode path,
	// which produces libx264 from the source file.
	caps := ParseCaps(r.URL.Query().Get("caps"))
	if HasCompletedPackage(itemID) && PackagedPlayableBy(itemID, caps) {
		h.PackagedMaster(w, r)
		return
	}
	clean, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Dispatch matrix:
	//
	//   probe + caps → mode → pipeline
	//   ─────────────────────────────────────────────────────────────
	//   video OK, audio OK + ≤2ch  passthrough  → /copy/   -c copy
	//   video OK, audio NOT OK     remux        → /{q}/    -c:v copy + audio re-encode
	//                                              (transcode.go's mode=="remux" branch)
	//   video NOT OK               transcode    → /{q}/    full libx264/NVENC ladder
	//
	// Client signals what it can decode via ?caps=avc,hvc,aacmc,... so
	// the first pick is right the first time (no circuit-breaker round
	// trip). Missing/empty caps falls back to DefaultCaps (the safe set
	// every modern browser handles).
	qParam := r.URL.Query().Get("q")
	ql := ResolveQuality(qParam)
	mode, _ := probe.DecideWith(caps)
	// useCopy is reserved for TRUE stream-copy of both tracks. The
	// remux case (video copy, audio re-encode) shares the transcode
	// pipeline because that's where the audio-only re-encode branch
	// lives. Forcing q != high also takes the transcode path (the
	// quality switcher only makes sense when we're actually encoding).
	useCopy := (qParam == "" || qParam == "high") && mode == "passthrough"
	isHEVCSource := probe.VideoCodec == "hevc" || probe.VideoCodec == "h265"
	q := r.URL.RawQuery
	if q != "" {
		q = "?" + q
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")
	// Audio renditions — one per audio track in the source. The first
	// (or the one with the default-disposition flag) gets DEFAULT=YES
	// so hls.js picks it automatically; users switch via the audio
	// menu, which maps onto hls.audioTrack.
	audioGroup := "aud"
	defaultIdx := defaultAudioIndex(probe.AudioTracks)
	for _, t := range probe.AudioTracks {
		name := audioRenditionName(t)
		def := "NO"
		if t.Index == defaultIdx {
			def = "YES"
		}
		lang := t.Language
		if lang == "" {
			lang = "und"
		}
		// Copy variant has one audio rendition built into each segment
		// (multi-audio passthrough would need per-track CMAF, deferred).
		// Skip the EXT-X-MEDIA audio group in copy mode so hls.js
		// reads audio from the video segments themselves.
		if useCopy {
			continue
		}
		sb.WriteString(fmt.Sprintf(
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=%q,NAME=%q,LANGUAGE=%q,DEFAULT=%s,AUTOSELECT=YES,URI=\"audio/%d/index.m3u8%s\"\n",
			audioGroup, name, lang, def, t.Index, q,
		))
	}
	if useCopy {
		// Stream-copy passthrough variant. CODECS string switches between
		// avc1 (H.264 source) and hvc1 (HEVC source); we use permissive
		// profile/level values since the actual config comes from the
		// source's avcC/hvcC verbatim and browsers tolerate looser
		// advertised levels than what they can decode.
		bw := nominalBitrate("high")
		res := nominalResolution("high")
		videoCodec := "avc1.640028"
		if isHEVCSource {
			// Main profile, Level 4.0 — covers Intouchables-class 1080p
			// remuxes and most BD HEVC content. 4K Main10 sources
			// (hvc1.2.4.L153.B0) still play because the avcC/hvcC is
			// authoritative; the master string is just gating.
			videoCodec = "hvc1.1.6.L120.B0"
		}
		sb.WriteString(fmt.Sprintf(
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s,CODECS=\"%s,mp4a.40.2\",VIDEO-RANGE=%s\n",
			bw, res, videoCodec, videoRange(probe),
		))
		sb.WriteString(fmt.Sprintf("copy/index.m3u8%s\n", q))
	} else {
		// Transcode variant — single rung matching ?q=.
		bw := nominalBitrate(ql.Name)
		res := nominalResolution(ql.Name)
		audioAttr := ""
		if len(probe.AudioTracks) > 0 {
			audioAttr = fmt.Sprintf(",AUDIO=%q", audioGroup)
		}
		sb.WriteString(fmt.Sprintf(
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s,CODECS=\"avc1.640028,mp4a.40.2\",VIDEO-RANGE=SDR%s\n",
			bw, res, audioAttr,
		))
		sb.WriteString(fmt.Sprintf("%s/index.m3u8%s\n", ql.Name, q))
	}
	// Pre-warm: kick off the first window's transcode / first-segment plan
	// in the background so the playlist + init.mp4 + seg-0 fetches that
	// follow this master.m3u8 race the work that's already in flight. Cuts
	// ~400 ms off cold start on a libx264 transcode path.
	if useCopy {
		go h.warmCopy(itemID, clean)
	} else {
		go h.warmTranscode(itemID, clean, probe, ql, caps.MaxVideoHeight())
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(sb.String()))
}

// PackagedIDs lists every item that has a finished CMAF package on
// disk. The Zap pager calls this once per session to filter its
// candidate pool to instant-start items — packaged items skip ffmpeg
// entirely and serve in tens of ms.
//
// The directory walk is cached for 60 s inside ListCompletedPackageIDs
// so back-to-back calls don't hammer NFS. No auth-related state is
// exposed (just opaque ids the caller already has in their catalog
// listing), but we still gate behind the existing verifier middleware
// because the broader play group does.
func (h *HLSHandler) PackagedIDs(w http.ResponseWriter, _ *http.Request) {
	ids := ListCompletedPackageIDs()
	w.Header().Set("Content-Type", "application/json")
	// 30-second client cache so a tab refreshing the Zap pool inside
	// the same minute reuses the response.
	w.Header().Set("Cache-Control", "private, max-age=30")
	var sb strings.Builder
	sb.WriteString(`{"ids":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		sb.WriteString(id)
		sb.WriteByte('"')
	}
	sb.WriteString(`]}`)
	_, _ = w.Write([]byte(sb.String()))
}

// Prewarm primes the chino-stream caches for a card the user is
// likely to swipe to next, without actually serving any media. The
// Zap pager fires a fire-and-forget POST to this endpoint for the
// distance=1 card so that when the user does swipe, ffprobe is
// cached, the source file is in the OS page cache, the NVENC context
// is primed, and (for transcode items) window 0 is already on disk.
//
// Behaviour mirrors Master() up to the playlist emit step, then
// branches into the same background warmCopy / warmTranscode the
// real Master kicks off. Differences:
//
//   - HTTP 202 returned immediately; no playlist body
//   - Packaged items warm the NFS client + OS page cache + the
//     in-memory packagedCache for the renditions the client can play,
//     off the request path (warmPackaged). The early-return that used
//     to claim "nothing to warm" was wrong: cold packaged reads stall
//     for seconds under NFS contention (a 924-byte init.mp4 was seen
//     to stall 18.4s; video segments p90 1.35s, max 17.7s), and Zap
//     channel-surf lands on a COLD new item on every swipe.
//   - The transcode warm goroutines acquire from warmFFmpegSlots, not
//     the main pool, so they can NEVER starve a real-request transcode
//
// Optional ?t=<sec> picks the segments to warm (the seek/resume hint):
// when present we warm the segment containing tSec plus the next one;
// otherwise the first two segments of each rendition.
//
// Authn rides on the same verifier middleware as Master; the bearer
// can come from Authorization or ?stream=token.
func (h *HLSHandler) Prewarm(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	caps := ParseCaps(r.URL.Query().Get("caps"))
	tSec := 0.0
	if v := r.URL.Query().Get("t"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			tSec = n
		}
	}
	if HasCompletedPackage(itemID) && PackagedPlayableBy(itemID, caps) {
		// Fire-and-forget background warm: prime the NFS client, OS
		// page cache and packagedCache so the real playlist/init/seg
		// fetches that follow are hot. 202 with a body so client
		// telemetry can distinguish "packaged-warming" from "warming"
		// (transcode) and the old "packaged" no-op.
		go h.warmPackaged(itemID, caps, tSec)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("packaged-warming"))
		return
	}
	clean, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	qParam := r.URL.Query().Get("q")
	ql := ResolveQuality(qParam)
	mode, _ := probe.DecideWith(caps)
	useCopy := (qParam == "" || qParam == "high") && mode == "passthrough"
	if useCopy {
		go h.warmCopy(itemID, clean)
	} else {
		go h.warmTranscode(itemID, clean, probe, ql, caps.MaxVideoHeight())
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("warming"))
}

// defaultAudioIndex picks the source audio track that should be
// auto-selected. Prefers a track with disposition=default; otherwise
// the first available track.
func defaultAudioIndex(tracks []TrackInfo) int {
	for _, t := range tracks {
		if t.Default {
			return t.Index
		}
	}
	if len(tracks) > 0 {
		return tracks[0].Index
	}
	return 0
}

func audioRenditionName(t TrackInfo) string {
	if t.Title != "" {
		return t.Title
	}
	if t.Language != "" {
		return langDisplay(t.Language)
	}
	return fmt.Sprintf("Track %d", t.Index)
}

// langDisplay maps an ISO-639 code to a human-readable name. Falls
// back to the code itself.
func langDisplay(code string) string {
	switch strings.ToLower(code) {
	case "eng", "en":
		return "English"
	case "deu", "ger", "de":
		return "German"
	case "fra", "fre", "fr":
		return "French"
	case "spa", "es":
		return "Spanish"
	case "ita", "it":
		return "Italian"
	case "jpn", "ja":
		return "Japanese"
	case "zho", "chi", "zh":
		return "Chinese"
	case "und":
		return "Unknown"
	}
	return code
}

// Playlist returns the media playlist for one quality variant. Lists
// the init segment via #EXT-X-MAP and every media segment with its
// #EXTINF duration. Always a VOD playlist (#EXT-X-PLAYLIST-TYPE:VOD)
// with #EXT-X-ENDLIST so the player knows the full length up front.
func (h *HLSHandler) Playlist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	quality := chi.URLParam(r, "quality")
	if _, ok := QualityLadder[quality]; !ok {
		http.Error(w, "unknown quality", http.StatusBadRequest)
		return
	}
	_, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	durSec := float64(probe.DurationMs) / 1000.0
	if durSec <= 0 {
		http.Error(w, "source has no duration", http.StatusBadGateway)
		return
	}
	q := r.URL.RawQuery
	if q != "" {
		q = "?" + q
	}
	segCount := int(durSec / float64(segmentSec))
	tail := durSec - float64(segCount*segmentSec)
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")
	sb.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", segmentSec))
	sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"init.mp4%s\"\n", q))
	for n := 0; n < segCount; n++ {
		sb.WriteString(fmt.Sprintf("#EXTINF:%d.000,\n%d.m4s%s\n", segmentSec, n, q))
	}
	if tail > 0.5 {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%d.m4s%s\n", tail, segCount, q))
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(sb.String()))
}

// InitSegment returns the fMP4 init bytes (ftyp + moov, no samples).
// Produced by the same ffmpeg invocation that emits window 0's segments,
// so the avcC/esds in init and the samples in the segments come from
// one encoder instance — byte-for-byte compatible by construction.
func (h *HLSHandler) InitSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	quality := chi.URLParam(r, "quality")
	ql, ok := QualityLadder[quality]
	if !ok {
		http.Error(w, "unknown quality", http.StatusBadRequest)
		return
	}
	src, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The HW height cap rides on the same ?caps= the master carried, so
	// every leg (init / segment) re-derives the identical cache namespace
	// and serves the bytes ensureVideoWindow wrote under it.
	maxHeight := ParseCaps(r.URL.Query().Get("caps")).MaxVideoHeight()
	isHDR := probe != nil && probe.IsHDR()
	if err := h.ensureVideoWindow(r.Context(), itemID, src, quality, ql, isHDR, probe, 0, 0, maxHeight); err != nil {
		log.Printf("hls init %s/%s: %v", itemID, quality, err)
		http.Error(w, "init segment failed", http.StatusBadGateway)
		return
	}
	h.serveCached(w, r, h.cachePath(itemID, videoCacheQuality(quality, maxHeight), "init"), "video/mp4")
}

// Segment serves one HLS media segment. On a cache miss, the entire
// containing window of segments is produced in a single ffmpeg run —
// keeping encoder state across internal boundaries so the segments
// within the window are seamlessly adjacent.
func (h *HLSHandler) Segment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	quality := chi.URLParam(r, "quality")
	segStr := chi.URLParam(r, "seg")
	ql, ok := QualityLadder[quality]
	if !ok {
		http.Error(w, "unknown quality", http.StatusBadRequest)
		return
	}
	seg, err := strconv.Atoi(segStr)
	if err != nil || seg < 0 {
		http.Error(w, "bad segment number", http.StatusBadRequest)
		return
	}
	src, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if probe != nil && probe.DurationMs > 0 {
		total := float64(probe.DurationMs) / 1000.0
		if float64(seg*segmentSec) >= total {
			http.Error(w, "segment past end", http.StatusNotFound)
			return
		}
	}
	maxHeight := ParseCaps(r.URL.Query().Get("caps")).MaxVideoHeight()
	isHDR := probe != nil && probe.IsHDR()
	windowIdx := seg / windowSize
	if err := h.ensureVideoWindow(r.Context(), itemID, src, quality, ql, isHDR, probe, windowIdx, seg, maxHeight); err != nil {
		log.Printf("hls seg %s/%s/%d (win %d): %v", itemID, quality, seg, windowIdx, err)
		http.Error(w, "segment failed", http.StatusBadGateway)
		return
	}
	cachePath := h.cachePath(itemID, videoCacheQuality(quality, maxHeight), strconv.Itoa(seg))
	if _, err := os.Stat(cachePath); err != nil {
		// ensureVideoWindow ran (possibly with one retry) and the
		// segment still isn't on disk. Either past source EOF or the
		// encoder can't produce it deterministically — let the client
		// ABR-fall to a lower rung rather than blocking the player.
		http.Error(w, "segment unavailable", http.StatusNotFound)
		return
	}
	h.serveCached(w, r, cachePath, "video/iso.segment")
}

// ensureVideoWindow makes sure the requested segment (and init.mp4)
// for windowIdx are present in the cache. One ffmpeg invocation
// produces the whole window — see windowSize — keeping encoder state
// across internal boundaries so segments are seamless.
//
// requestedSeg is the absolute segment number the caller wants; the
// fast-path check is keyed on it (not just on the first segment of
// the window) because the NVENC/hwaccel path has been seen to exit 0
// with a partially-populated window. If ffmpeg ran and the specific
// segment didn't land, we retry once with a fresh tmpdir (cap:
// maxWindowAttempts) — that clears the silent-truncation race for
// most 4K HEVC sources. Beyond the cap, return without error and let
// the caller 404 so the client can fall back to a lower rung.
//
// Concurrent requests for any segment in the same window dedupe on
// the per-window mutex, so we never burn N× CPU on the same transcode.
func (h *HLSHandler) ensureVideoWindow(ctx context.Context, itemID, src, quality string, ql Quality, isHDR bool, probe *Probe, windowIdx, requestedSeg, maxHeight int) error {
	// capQuality is the on-disk cache namespace. When a HW height cap is
	// in play the encoder output differs (downscaled to the ceiling), so
	// it must NOT share the uncapped rung's cache — otherwise a capped
	// device and an uncapped one collide on the same {item}/{quality}
	// segments. The player URL path stays "{quality}"; only the cache dir
	// is namespaced (each handler re-derives the same cap from ?caps=).
	capQuality := videoCacheQuality(quality, maxHeight)
	initPath := h.cachePath(itemID, capQuality, "init")
	requestedSegPath := h.cachePath(itemID, capQuality, strconv.Itoa(requestedSeg))
	if statOK(initPath) && statOK(requestedSegPath) {
		return nil
	}
	mu := h.lockFor(fmt.Sprintf("vwin/%s/%s/%d", itemID, capQuality, windowIdx))
	mu.Lock()
	defer mu.Unlock()
	if statOK(initPath) && statOK(requestedSegPath) {
		return nil
	}

	windowKey := fmt.Sprintf("%s/%s/%d", itemID, capQuality, windowIdx)
	for {
		// Bail before kicking off another ffmpeg run if we've already
		// burned our budget. This is the "give up gracefully" case —
		// caller sees the requestedSeg still missing and 404s.
		prev, _ := h.windowAttempts.Load(windowKey)
		attempts, _ := prev.(int)
		if attempts >= maxWindowAttempts {
			log.Printf("hls vwin %s: cap reached (%d attempts), seg %d unavailable",
				windowKey, attempts, requestedSeg)
			return nil
		}
		// On a retry, wipe the partial install so the next runFFmpeg
		// can write into a clean slate. We never delete init.bin (init
		// is shared across windows and was patched on first install).
		if attempts > 0 {
			log.Printf("hls vwin %s: partial transcode (seg %d missing), retry %d",
				windowKey, requestedSeg, attempts+1)
			if err := h.invalidateWindowSegments(itemID, capQuality, windowIdx); err != nil {
				return err
			}
		}
		h.windowAttempts.Store(windowKey, attempts+1)
		if err := h.transcodeVideoWindow(ctx, itemID, src, quality, ql, isHDR, probe, windowIdx, maxHeight); err != nil {
			return err
		}
		if statOK(requestedSegPath) {
			return nil
		}
	}
}

// videoCacheQuality is the cache-dir namespace for a transcode rung.
// Without a HW height cap it's just the rung name (unchanged on-disk
// layout, so legacy caches stay valid). With a cap it appends "-h<cap>"
// so capped (downscaled) output never aliases uncapped output for the
// same item+rung.
func videoCacheQuality(quality string, maxHeight int) string {
	if maxHeight <= 0 {
		return quality
	}
	return quality + "-h" + strconv.Itoa(maxHeight)
}

// transcodeVideoWindow runs ffmpeg once for windowIdx and installs the
// produced segments into the cache. Split out from ensureVideoWindow
// so the partial-install retry loop can re-invoke it without
// duplicating the arg-building code.
func (h *HLSHandler) transcodeVideoWindow(ctx context.Context, itemID, src, quality string, ql Quality, isHDR bool, probe *Probe, windowIdx, maxHeight int) error {
	capQuality := videoCacheQuality(quality, maxHeight)
	initPath := h.cachePath(itemID, capQuality, "init")
	windowStartSec := windowIdx * windowSize * segmentSec
	windowDurSec := windowSize * segmentSec
	if probe != nil && probe.DurationMs > 0 {
		total := int(probe.DurationMs / 1000)
		// +1 because ffmpeg's -t is exclusive and we'd rather over-shoot
		// by a frame than truncate the final segment to nothing.
		if windowStartSec+windowDurSec > total {
			windowDurSec = total - windowStartSec + 1
		}
	}
	if windowDurSec < 1 {
		return fmt.Errorf("window %d past source end", windowIdx)
	}

	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(initPath), fmt.Sprintf("vwin%d-", windowIdx))
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Source dims drive the rung's scale filter for sources larger than
	// the rung's nominal target. Without this, a 4K source fed through
	// the "high" rung would try to encode at native 3840×1606 in
	// libx264/NVENC with -level 4.0 (max 1920×1088) and ffmpeg bails
	// with "InitializeEncoder failed: Invalid Level".
	srcW, srcH := 0, 0
	if probe != nil {
		srcW, srcH = probe.Width, probe.Height
	}
	// HDR->SDR tone-mapping only exists on the libx264 path (see
	// videoEncoderArgs); the NVENC path skips it and produces a desaturated,
	// too-dark picture. Route HDR to libx264 so it tone-maps — but ONLY at
	// ≤1080p, where libx264 keeps up with the 60s-window deadline in real time.
	// A 4K HDR source stays on NVENC (still desaturated) because CPU
	// tone-mapping 4K under real-time serving pressure would stall playback at
	// every window seam; the GPU tonemap_cuda path (follow-up) fixes 4K.
	// useNvenc gates BOTH the input hwaccel flags AND the encoder args so
	// CUDA-decoded frames are never handed to the software filter/encoder
	// chain (which fails with a "no path between colorspaces"/"Impossible to
	// convert" error).
	useNvenc := h.UseNVENC && !(isHDR && srcH > 0 && srcH <= 1080)

	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning",
		"-fflags", "+genpts+igndts+discardcorrupt",
		"-err_detect", "ignore_err",
	}
	if useNvenc {
		// CUDA hwaccel decode keeps frames on the GPU; the matching
		// scale_cuda + h264_nvenc in videoEncoderArgs reads them straight
		// from device memory. Without -hwaccel_output_format cuda the
		// decode lands in system memory and scale_cuda fails with a
		// "no path" error.
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args,
		"-ss", strconv.Itoa(windowStartSec),
		"-i", src,
		"-t", strconv.Itoa(windowDurSec),
		"-map", "0:v:0",
	)
	args = append(args, videoEncoderArgs(ql, h.TranscodePreset, isHDR, useNvenc, h.NVENCPreset, h.NVENCCQ, srcW, srcH, maxHeight)...)
	args = append(args,
		// Shift output PTS so segments declare absolute timeline.
		"-output_ts_offset", strconv.Itoa(windowStartSec),
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", strconv.Itoa(segmentSec),
		"-hls_init_time", "0",
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmpDir, "seg_%d.m4s"),
		filepath.Join(tmpDir, "playlist.m3u8"),
	)
	if err := h.runFFmpeg(ctx, args, fmt.Sprintf("vwin %d %s/%s", windowIdx, capQuality, filepath.Base(src))); err != nil {
		return err
	}
	return h.installWindow(tmpDir, itemID, capQuality, windowIdx)
}

// invalidateWindowSegments removes every cached segment for one
// window, leaving init.bin untouched. Called before a retry so the
// next install can land without colliding with stale partial output.
func (h *HLSHandler) invalidateWindowSegments(itemID, quality string, windowIdx int) error {
	base := windowIdx * windowSize
	for i := 0; i < windowSize; i++ {
		p := h.cachePath(itemID, quality, strconv.Itoa(base+i))
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ensureAudioWindow is the audio counterpart of ensureVideoWindow.
// Same windowing strategy and the same fast-path-keyed-on-the-
// requested-segment + retry behaviour — audio transcodes are usually
// deterministic (AAC + LC + plain ffmpeg path, no NVENC) so the retry
// is rarely needed in practice, but symmetry keeps the recovery
// behaviour predictable across both pipelines.
func (h *HLSHandler) ensureAudioWindow(ctx context.Context, itemID, src string, audioIdx int, probe *Probe, windowIdx, requestedSeg int) error {
	audioKey := "audio-" + strconv.Itoa(audioIdx)
	initPath := h.cachePath(itemID, audioKey, "init")
	requestedSegPath := h.cachePath(itemID, audioKey, strconv.Itoa(requestedSeg))
	if statOK(initPath) && statOK(requestedSegPath) {
		return nil
	}
	mu := h.lockFor(fmt.Sprintf("awin/%s/%d/%d", itemID, audioIdx, windowIdx))
	mu.Lock()
	defer mu.Unlock()
	if statOK(initPath) && statOK(requestedSegPath) {
		return nil
	}

	windowKey := fmt.Sprintf("%s/%s/%d", itemID, audioKey, windowIdx)
	for {
		prev, _ := h.windowAttempts.Load(windowKey)
		attempts, _ := prev.(int)
		if attempts >= maxWindowAttempts {
			log.Printf("hls awin %s: cap reached (%d attempts), seg %d unavailable",
				windowKey, attempts, requestedSeg)
			return nil
		}
		if attempts > 0 {
			log.Printf("hls awin %s: partial transcode (seg %d missing), retry %d",
				windowKey, requestedSeg, attempts+1)
			if err := h.invalidateWindowSegments(itemID, audioKey, windowIdx); err != nil {
				return err
			}
		}
		h.windowAttempts.Store(windowKey, attempts+1)
		if err := h.transcodeAudioWindow(ctx, itemID, src, audioIdx, audioKey, probe, windowIdx); err != nil {
			return err
		}
		if statOK(requestedSegPath) {
			return nil
		}
	}
}

func (h *HLSHandler) transcodeAudioWindow(ctx context.Context, itemID, src string, audioIdx int, audioKey string, probe *Probe, windowIdx int) error {
	initPath := h.cachePath(itemID, audioKey, "init")
	windowStartSec := windowIdx * windowSize * segmentSec
	windowDurSec := windowSize * segmentSec
	if probe != nil && probe.DurationMs > 0 {
		total := int(probe.DurationMs / 1000)
		if windowStartSec+windowDurSec > total {
			windowDurSec = total - windowStartSec + 1
		}
	}
	if windowDurSec < 1 {
		return fmt.Errorf("audio window %d past source end", windowIdx)
	}

	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(initPath), fmt.Sprintf("awin%d-", windowIdx))
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning",
		"-fflags", "+genpts+igndts+discardcorrupt",
		"-err_detect", "ignore_err",
		"-ss", strconv.Itoa(windowStartSec),
		"-i", src,
		"-t", strconv.Itoa(windowDurSec),
		"-vn",
		"-map", fmt.Sprintf("0:a:%d", audioIdx),
	}
	args = append(args, audioEncoderArgs()...)
	args = append(args,
		"-output_ts_offset", strconv.Itoa(windowStartSec),
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", strconv.Itoa(segmentSec),
		"-hls_init_time", "0",
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmpDir, "seg_%d.m4s"),
		filepath.Join(tmpDir, "playlist.m3u8"),
	)
	if err := h.runFFmpeg(ctx, args, fmt.Sprintf("awin %d/%d %s", audioIdx, windowIdx, filepath.Base(src))); err != nil {
		return err
	}
	return h.installWindow(tmpDir, itemID, audioKey, windowIdx)
}

// installWindow patches each per-window segment's tfdt to land in the
// right place in the global timeline, then renames the outputs into
// the permanent cache layout.
//
// Why patching is necessary: ffmpeg's `-f hls -hls_segment_type fmp4`
// muxer writes per-segment tfdt baseMediaDecodeTime starting from 0
// for each muxer invocation, regardless of `-output_ts_offset`. So
// window 0 produces tfdts 0, 6×ts, …, 54×ts (correct), but window 1
// produces tfdts 0, 6×ts, … (wrong — should start at 60×ts). Without
// this patch, MSE would render every window as starting at t=0,
// overwriting the previous window in the source buffer.
//
// Within each window the deltas across segments are preserved exactly
// (encoder state was continuous), so we only need to add a single
// window-wide bias.
func (h *HLSHandler) installWindow(tmpDir, itemID, qualityOrAudio string, windowIdx int) error {
	cacheRoot := filepath.Dir(h.cachePath(itemID, qualityOrAudio, "init"))
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return err
	}
	initSrc := filepath.Join(tmpDir, "init.mp4")
	initBytes, err := os.ReadFile(initSrc)
	if err != nil {
		return fmt.Errorf("read window init: %w", err)
	}
	timescales := parseInitTimescales(initBytes)
	bias := uint64(windowIdx * windowSize * segmentSec) // seconds; multiplied by per-track timescale at patch time

	initDst := filepath.Join(cacheRoot, "init.bin")
	if !statOK(initDst) {
		if err := os.Rename(initSrc, initDst); err != nil {
			return fmt.Errorf("install init: %w", err)
		}
	}
	for i := 0; i < windowSize; i++ {
		segSrc := filepath.Join(tmpDir, fmt.Sprintf("seg_%d.m4s", i))
		if !statOK(segSrc) {
			// End of file: no more segments in this window.
			break
		}
		if bias > 0 {
			if err := addTfdtBiasInPlace(segSrc, bias, timescales); err != nil {
				return fmt.Errorf("patch tfdt seg %d: %w", i, err)
			}
		}
		absSeg := windowIdx*windowSize + i
		segDst := filepath.Join(cacheRoot, strconv.Itoa(absSeg)+".bin")
		if err := os.Rename(segSrc, segDst); err != nil {
			return fmt.Errorf("install seg %d: %w", absSeg, err)
		}
	}
	return nil
}

// addTfdtBiasInPlace adds (biasSec × per-track-timescale) to every
// tfdt baseMediaDecodeTime in the file. Used to shift a whole window's
// worth of segments to its correct position in the global timeline.
// Operates in place — the file's byte length doesn't change because
// we only mutate the existing 8-byte bmdt fields.
func addTfdtBiasInPlace(path string, biasSec uint64, timescales map[uint32]uint32) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	patchMoofTfdtBias(buf, biasSec, timescales)
	return os.WriteFile(path, buf, 0o644)
}

func patchMoofTfdtBias(buf []byte, biasSec uint64, timescales map[uint32]uint32) {
	i := 0
	for i+8 <= len(buf) {
		size := int(uint32(buf[i])<<24 | uint32(buf[i+1])<<16 | uint32(buf[i+2])<<8 | uint32(buf[i+3]))
		boxType := string(buf[i+4 : i+8])
		if size < 8 || i+size > len(buf) {
			return
		}
		if boxType == "moof" {
			patchTrafTfdtBias(buf[i+8:i+size], biasSec, timescales)
		}
		i += size
	}
}

func patchTrafTfdtBias(moof []byte, biasSec uint64, timescales map[uint32]uint32) {
	i := 0
	for i+8 <= len(moof) {
		size := int(uint32(moof[i])<<24 | uint32(moof[i+1])<<16 | uint32(moof[i+2])<<8 | uint32(moof[i+3]))
		boxType := string(moof[i+4 : i+8])
		if size < 8 || i+size > len(moof) {
			return
		}
		if boxType == "traf" {
			addTfdtBiasToTraf(moof[i+8:i+size], biasSec, timescales)
		}
		i += size
	}
}

func addTfdtBiasToTraf(traf []byte, biasSec uint64, timescales map[uint32]uint32) {
	var trackID uint32
	tfdtOffset := -1
	tfdtVersion := uint8(0)
	i := 0
	for i+8 <= len(traf) {
		size := int(uint32(traf[i])<<24 | uint32(traf[i+1])<<16 | uint32(traf[i+2])<<8 | uint32(traf[i+3]))
		boxType := string(traf[i+4 : i+8])
		if size < 8 || i+size > len(traf) {
			return
		}
		switch boxType {
		case "tfhd":
			if i+8+8 <= len(traf) {
				trackID = uint32(traf[i+12])<<24 | uint32(traf[i+13])<<16 | uint32(traf[i+14])<<8 | uint32(traf[i+15])
			}
		case "tfdt":
			tfdtOffset = i
			tfdtVersion = traf[i+8]
		}
		i += size
	}
	if tfdtOffset < 0 || trackID == 0 {
		return
	}
	ts, ok := timescales[trackID]
	if !ok || ts == 0 {
		return
	}
	add := biasSec * uint64(ts)
	if tfdtVersion == 1 {
		cur := uint64(traf[tfdtOffset+12])<<56 |
			uint64(traf[tfdtOffset+13])<<48 |
			uint64(traf[tfdtOffset+14])<<40 |
			uint64(traf[tfdtOffset+15])<<32 |
			uint64(traf[tfdtOffset+16])<<24 |
			uint64(traf[tfdtOffset+17])<<16 |
			uint64(traf[tfdtOffset+18])<<8 |
			uint64(traf[tfdtOffset+19])
		bmdt := cur + add
		traf[tfdtOffset+12] = byte(bmdt >> 56)
		traf[tfdtOffset+13] = byte(bmdt >> 48)
		traf[tfdtOffset+14] = byte(bmdt >> 40)
		traf[tfdtOffset+15] = byte(bmdt >> 32)
		traf[tfdtOffset+16] = byte(bmdt >> 24)
		traf[tfdtOffset+17] = byte(bmdt >> 16)
		traf[tfdtOffset+18] = byte(bmdt >> 8)
		traf[tfdtOffset+19] = byte(bmdt)
	} else {
		cur := uint32(traf[tfdtOffset+12])<<24 |
			uint32(traf[tfdtOffset+13])<<16 |
			uint32(traf[tfdtOffset+14])<<8 |
			uint32(traf[tfdtOffset+15])
		bmdt := cur + uint32(add)
		traf[tfdtOffset+12] = byte(bmdt >> 24)
		traf[tfdtOffset+13] = byte(bmdt >> 16)
		traf[tfdtOffset+14] = byte(bmdt >> 8)
		traf[tfdtOffset+15] = byte(bmdt)
	}
}

// parseInitTimescales walks an init.mp4 in memory and pulls each
// track's (track_ID, timescale) from its mdhd box. Used by the
// window installer so it can patch tfdt's bmdt with the right
// per-track timescale.
func parseInitTimescales(init []byte) map[uint32]uint32 {
	out := map[uint32]uint32{}
	walkChildren(init, func(typ string, body []byte) {
		if typ != "moov" {
			return
		}
		walkChildren(body, func(typ2 string, trak []byte) {
			if typ2 != "trak" {
				return
			}
			var trackID uint32
			var ts uint32
			walkChildren(trak, func(typ3 string, sub []byte) {
				switch typ3 {
				case "tkhd":
					if len(sub) < 24 {
						return
					}
					v := sub[0]
					off := 4 + 8
					if v == 1 {
						off = 4 + 16
					}
					if len(sub) >= off+4 {
						trackID = uint32(sub[off])<<24 | uint32(sub[off+1])<<16 | uint32(sub[off+2])<<8 | uint32(sub[off+3])
					}
				case "mdia":
					walkChildren(sub, func(typ4 string, mdia []byte) {
						if typ4 != "mdhd" || len(mdia) < 20 {
							return
						}
						v := mdia[0]
						off := 4 + 8
						if v == 1 {
							off = 4 + 16
						}
						if len(mdia) >= off+4 {
							ts = uint32(mdia[off])<<24 | uint32(mdia[off+1])<<16 | uint32(mdia[off+2])<<8 | uint32(mdia[off+3])
						}
					})
				}
			})
			if trackID != 0 && ts != 0 {
				out[trackID] = ts
			}
		})
	})
	return out
}

func walkChildren(buf []byte, fn func(typ string, body []byte)) {
	i := 0
	for i+8 <= len(buf) {
		size := int(uint32(buf[i])<<24 | uint32(buf[i+1])<<16 | uint32(buf[i+2])<<8 | uint32(buf[i+3]))
		if size < 8 || i+size > len(buf) {
			return
		}
		typ := string(buf[i+4 : i+8])
		fn(typ, buf[i+8:i+size])
		i += size
	}
}

// ffmpegSlots is the global concurrency cap for ffmpeg invocations.
// Each libx264 1080p run holds ~400-700 MB; with the 2 GiB pod limit
// (and 4 GiB after the deployment bump) running more than a handful
// in parallel OOM-kills the container. A user scrubbing rapidly fans
// out 8+ window requests in seconds — without a cap the pod dies and
// every in-flight player session 502s.
//
// Capacity defaults to FFMPEG_MAX_CONCURRENT (or 3 if unset). Each
// runFFmpeg / runFFmpegWithStdout call acquires before Start and
// releases after Wait. Acquisition is context-aware so a cancelled
// HTTP request doesn't get stuck waiting forever on a full pool.
var ffmpegSlots chan struct{}

// warmFFmpegSlots is a SEPARATE concurrency cap dedicated to background
// warm work (pre-warmed window 0 from Master, plus the explicit /prewarm
// endpoint used by the Zap pager). Without this, a warm goroutine and
// a real request would queue on the same semaphore — under load the
// warm starves the real request, so the user waits longer than they
// would have if the warm had never run.
//
// Capacity defaults to FFMPEG_WARM_MAX_CONCURRENT (or 2 if unset).
// Smaller than the main pool on purpose: warms are best-effort, and
// keeping the warm pool small means the main pool is never starved
// for the real-request slots that drive user-visible latency.
var warmFFmpegSlots chan struct{}

// ctxKey is a private context-key type so we don't collide with anyone
// else's context values.
type ctxKey int

const ctxKeyWarm ctxKey = 1

// withWarmContext marks a context as belonging to a warm goroutine.
// runFFmpeg checks for this marker and routes the acquisition to
// warmFFmpegSlots instead of ffmpegSlots. Warm callers MUST set this
// — otherwise their ffmpeg invocation queues on the real-request pool
// and the whole point of the split (real requests never wait behind
// best-effort warms) is defeated.
func withWarmContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyWarm, true)
}

func isWarmContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyWarm).(bool)
	return v
}

func init() {
	n := 3
	if v := os.Getenv("FFMPEG_MAX_CONCURRENT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	ffmpegSlots = make(chan struct{}, n)

	wn := 2
	if v := os.Getenv("FFMPEG_WARM_MAX_CONCURRENT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			wn = parsed
		}
	}
	warmFFmpegSlots = make(chan struct{}, wn)
}

// acquireFFmpegSlot blocks until a slot is available or ctx is done.
// Returns a release function the caller MUST call (defer release()).
//
// Routes warm goroutines (ctx marked via withWarmContext) to the
// smaller warmFFmpegSlots pool so real-request transcodes never wait
// behind best-effort warms.
func acquireFFmpegSlot(ctx context.Context) (release func(), err error) {
	pool := ffmpegSlots
	if isWarmContext(ctx) {
		pool = warmFFmpegSlots
	}
	select {
	case pool <- struct{}{}:
		return func() { <-pool }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// runFFmpeg executes ffmpeg with the given args, draining stderr to
// the log. Used by the windowed transcoders. Gated by ffmpegSlots
// so concurrent window requests can't OOM-kill the pod.
func (h *HLSHandler) runFFmpeg(ctx context.Context, args []string, label string) error {
	release, err := acquireFFmpegSlot(ctx)
	if err != nil {
		return fmt.Errorf("ffmpeg slot wait: %w", err)
	}
	defer release()
	cmd := exec.CommandContext(ctx, h.FFmpegBin, args...)
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	go drainTo(stderr, label)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg wait: %w", err)
	}
	return nil
}

func statOK(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// AudioPlaylist returns the audio-only media playlist for one source
// audio track. Same shape as the video Playlist: VOD, fixed
// segmentSec segments, #EXT-X-MAP to init.mp4.
func (h *HLSHandler) AudioPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	audioStr := chi.URLParam(r, "audioIdx")
	audioIdx, err := strconv.Atoi(audioStr)
	if err != nil || audioIdx < 0 {
		http.Error(w, "bad audio index", http.StatusBadRequest)
		return
	}
	_, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	durSec := float64(probe.DurationMs) / 1000.0
	if durSec <= 0 {
		http.Error(w, "source has no duration", http.StatusBadGateway)
		return
	}
	q := r.URL.RawQuery
	if q != "" {
		q = "?" + q
	}
	segCount := int(durSec / float64(segmentSec))
	tail := durSec - float64(segCount*segmentSec)
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")
	sb.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", segmentSec))
	sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"init.mp4%s\"\n", q))
	for n := 0; n < segCount; n++ {
		sb.WriteString(fmt.Sprintf("#EXTINF:%d.000,\n%d.m4s%s\n", segmentSec, n, q))
	}
	if tail > 0.5 {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%d.m4s%s\n", tail, segCount, q))
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(sb.String()))
}

// AudioInitSegment serves the audio init.mp4 for one source audio
// track. Produced by the same window 0 invocation that emits the audio
// segments, mirroring the video init/segment alignment guarantee.
func (h *HLSHandler) AudioInitSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	audioStr := chi.URLParam(r, "audioIdx")
	audioIdx, err := strconv.Atoi(audioStr)
	if err != nil || audioIdx < 0 {
		http.Error(w, "bad audio index", http.StatusBadRequest)
		return
	}
	src, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.ensureAudioWindow(r.Context(), itemID, src, audioIdx, probe, 0, 0); err != nil {
		log.Printf("hls audio init %s/%d: %v", itemID, audioIdx, err)
		http.Error(w, "audio init failed", http.StatusBadGateway)
		return
	}
	h.serveCached(w, r, h.cachePath(itemID, "audio-"+audioStr, "init"), "video/mp4")
}

// AudioSegment serves one audio segment. Windowed transcode: a cache
// miss runs one ffmpeg for the whole containing window so the AAC
// frame boundaries inside the window line up without overlap.
func (h *HLSHandler) AudioSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	audioStr := chi.URLParam(r, "audioIdx")
	segStr := chi.URLParam(r, "seg")
	audioIdx, err := strconv.Atoi(audioStr)
	if err != nil || audioIdx < 0 {
		http.Error(w, "bad audio index", http.StatusBadRequest)
		return
	}
	seg, err := strconv.Atoi(segStr)
	if err != nil || seg < 0 {
		http.Error(w, "bad segment number", http.StatusBadRequest)
		return
	}
	src, probe, err := h.resolveSource(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if probe != nil && probe.DurationMs > 0 {
		total := float64(probe.DurationMs) / 1000.0
		if float64(seg*segmentSec) >= total {
			http.Error(w, "segment past end", http.StatusNotFound)
			return
		}
	}
	windowIdx := seg / windowSize
	if err := h.ensureAudioWindow(r.Context(), itemID, src, audioIdx, probe, windowIdx, seg); err != nil {
		log.Printf("hls audio seg %s/%d/%d (win %d): %v", itemID, audioIdx, seg, windowIdx, err)
		http.Error(w, "audio segment failed", http.StatusBadGateway)
		return
	}
	cachePath := h.cachePath(itemID, "audio-"+audioStr, strconv.Itoa(seg))
	if _, err := os.Stat(cachePath); err != nil {
		http.Error(w, "segment unavailable", http.StatusNotFound)
		return
	}
	h.serveCached(w, r, cachePath, "video/iso.segment")
}

// scaleTargetHeight extracts the target height from a "scale=-2:<h>"
// expression (the only shape used in QualityLadder). Returns 0 when the
// expression isn't of that form so the caller leaves the rung untouched.
func scaleTargetHeight(scale string) int {
	const pfx = "scale=-2:"
	if !strings.HasPrefix(scale, pfx) {
		return 0
	}
	if h, err := strconv.Atoi(scale[len(pfx):]); err == nil {
		return h
	}
	return 0
}

// videoEncoderArgs is the shared video encoding config — every segment
// + the init must produce identical codec parameters or MSE silently
// drops the append (no error, just an empty buffered range). Pinning
// profile, level, and color space is what lets us advertise a single
// codec string in the master playlist (avc1.640028 = H.264 High @
// Level 4.0 in BT.709 SDR) that browsers actually recognise.
//
// Two encoder paths:
//   - libx264 (CPU). Default when nvenc is not available. Slow but
//     portable. The filter chain explicitly tonemaps HDR via zscale +
//     hable; SDR sources keep the chain simple to avoid "no path
//     between colorspaces" errors on rips without explicit matrix
//     tags.
//   - h264_nvenc (GPU). Reads CUDA-decoded frames straight off the
//     device (caller must add `-hwaccel cuda -hwaccel_output_format
//     cuda` on the input side), scales on-device via scale_cuda, and
//     encodes via NVENC. ~10× faster on 1080p HEVC than libx264. HDR
//     tonemapping is intentionally skipped here — libplacebo on CUDA
//     is touchy with the bundled ffmpeg build and the source pool
//     skews heavily SDR; if a HDR source lands here it'll just look
//     desaturated, which beats failing to play.
func videoEncoderArgs(ql Quality, preset string, isHDR, useNvenc bool, nvencPreset, nvencCQ string, srcW, srcH, maxHeight int) []string {
	// than that — 4K, 1440p, ultrawide 21:9 — must be downscaled into
	// a 1920×1080 box or the encoder rejects them with "Invalid Level".
	// On the explicit ladder rungs (medium/low) ql.Scale already
	// targets ≤720p; the "high" rung's empty Scale meant passthrough
	// size, which only works for ≤1080p.
	//
	// maxHeight is the device HW decoder ceiling threaded from the
	// request's ?caps= (0 = no cap). The fit box height is the SMALLER
	// of the 1080 level limit and maxHeight, so a device that can only
	// HW-decode 1080 never gets a taller H.264 frame than it can play —
	// otherwise the source-resolution "high" rung re-serves the same 4K
	// frame the device couldn't HW-decode in the first place (the reason
	// it fell through from packaged to transcode). The width companion
	// (16:9 of the box height) keeps us inside Level 4.0's pixel budget.
	capH := 1080
	if maxHeight > 0 && maxHeight < capH {
		capH = maxHeight
	}
	capW := capH * 16 / 9 // 16:9 companion of the box height
	//
	// Aspect-preserving "fit within capW×capH":
	//   - width-limited (srcW/srcH > 16/9): w=capW, h=srcH*capW/srcW
	//   - height-limited:                   h=capH, w=srcW*capH/srcH
	// Round both to even pixels — h264 requires even dims, scale_cuda
	// errors on odd numbers.
	needsDownscale := srcW > capW || srcH > capH
	fitW, fitH := 0, 0
	if needsDownscale && srcW > 0 && srcH > 0 {
		if srcW*capH > srcH*capW {
			// Width-limited (e.g. 21:9 ultrawide). 3840×1606 → capW×….
			fitW = capW
			fitH = (srcH*capW/srcW + 1) &^ 1
		} else {
			fitH = capH
			fitW = (srcW*capH/srcH + 1) &^ 1
		}
	}
	// On the explicit ladder rungs (medium/low) ql.Scale already targets
	// a fixed height (720/480). If the device HW ceiling is even lower
	// than the rung's height, tighten the rung's scale to the ceiling so
	// the output never exceeds what the device can HW-decode.
	rungScale := ql.Scale
	if rungScale != "" && maxHeight > 0 {
		if rungH := scaleTargetHeight(rungScale); rungH > maxHeight {
			rungScale = "scale=-2:" + strconv.Itoa(maxHeight)
		}
	}
	if useNvenc {
		// nv12 is NVENC's native 8-bit pixel format. Forcing the
		// post-scale frame into nv12 also downconverts 10-bit HEVC
		// input (yuv420p10le) to 8-bit so h264_nvenc can encode it
		// — h264 profile=high doesn't support 10-bit. yuv420p hits
		// "Impossible to convert" because scale_cuda outputs CUDA
		// frames and libavfilter auto-inserts a SW scaler that
		// can't read them.
		var vf string
		switch {
		case rungScale != "":
			// rungScale is "scale=-2:720"; rewrite to scale_cuda=-2:720.
			h := rungScale[len("scale=-2:"):]
			vf = "scale_cuda=-2:" + h + ":format=nv12"
		case needsDownscale:
			vf = fmt.Sprintf("scale_cuda=%d:%d:format=nv12", fitW, fitH)
		default:
			// Source ≤1080p — no resize, just the format conversion.
			// scale_cuda needs w/h params; iw:ih = "input width
			// and height" = passthrough.
			vf = "scale_cuda=iw:ih:format=nv12"
		}
		return []string{
			"-vf", vf,
			"-c:v", "h264_nvenc",
			"-preset", nvencPreset,
			"-rc:v", "vbr",
			"-cq", nvencCQ,
			"-profile:v", "high",
			"-level:v", "4.0",
			"-b_ref_mode", "middle",
			"-spatial_aq", "1",
			"-rc-lookahead", "20",
			// -forced-idr 1 makes h264_nvenc honour the
			// -force_key_frames expression below. Without it NVENC
			// inserts IDRs on its own ~10 s schedule (GOP/keyint
			// driven), the HLS muxer splits at NVENC's IDR cadence
			// instead of the requested segmentSec, and a 60 s
			// window yields 6 ≈10.4 s segments rather than 10 × 6 s
			// — so any segment index ≥ 6 of a window 404s because
			// the file simply doesn't exist on disk. (Matches the
			// "force_key_frames + output_ts_offset interaction"
			// known issue called out in commit bb81aa9.)
			"-forced-idr", "1",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSec),
		}
	}
	vf := "format=yuv420p"
	if isHDR {
		vf = "zscale=transfer=linear:npl=100,format=gbrpf32le,tonemap=tonemap=hable:desat=0,zscale=primaries=709:transfer=709:matrix=709:range=tv,format=yuv420p"
	}
	switch {
	case rungScale != "":
		vf = rungScale + "," + vf
	case needsDownscale:
		vf = fmt.Sprintf("scale=%d:%d,", fitW, fitH) + vf
	}
	return []string{
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", preset,
		"-tune", "zerolatency",
		"-crf", ql.CRF,
		"-profile:v", "high",
		"-level:v", "4.0",
		"-pix_fmt", "yuv420p",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-colorspace", "bt709",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSec),
	}
}

// audioEncoderArgs is shared by every audio init + segment of every
// rendition. Pinned to AAC-LC 48 kHz stereo 128 kbps — matches the
// "mp4a.40.2" codec advertised in the master playlist. Stereo
// downmix is unconditional: 5.1 / 7.1 sources are mixed to 2.0 so
// the same pipeline applies regardless of source channel layout.
func audioEncoderArgs() []string {
	return []string{
		"-c:a", "libfdk_aac",
		"-profile:a", "aac_low",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", "128k",
	}
}

// videoRange maps a source's transfer function to the HLS EXT-X-STREAM-INF
// VIDEO-RANGE attribute (RFC 8216bis §4.4.4.2): smpte2084 -> PQ (HDR10),
// arib-std-b67 -> HLG; everything else (including a nil probe) -> SDR. The
// on-the-fly master omitted this, so a passed-through HDR HEVC variant was
// mis-signalled as SDR and HDR displays never engaged HDR mode. A transcoded
// variant is always tagged SDR by the caller (its output is BT.709 H.264).
func videoRange(p *Probe) string {
	if p == nil {
		return "SDR"
	}
	switch strings.ToLower(p.ColorTransfer) {
	case "smpte2084":
		return "PQ"
	case "arib-std-b67":
		return "HLG"
	default:
		return "SDR"
	}
}

// ensureCached writes the output of `produce` to cachePath atomically
// (via a `.tmp` rename) and dedupes concurrent producers for the same
// path. If the file already exists and is non-empty, returns
// immediately. Otherwise, the FIRST caller to take the per-path mutex
// runs the producer; subsequent callers wait, then find the cached
// file ready.
func (h *HLSHandler) ensureCached(ctx context.Context, cachePath string, produce func(io.Writer) error) error {
	if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	mu := h.lockFor(cachePath)
	mu.Lock()
	defer mu.Unlock()
	// Re-check after acquiring lock — another caller may have written.
	if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
		return nil
	}
	tmp := cachePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := produce(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (h *HLSHandler) lockFor(key string) *sync.Mutex {
	mu, _ := h.segmentLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (h *HLSHandler) cachePath(itemID, quality, leaf string) string {
	// SHA-1 the item id so filesystem-unsafe characters can't escape.
	hash := sha1.Sum([]byte(itemID))
	id := hex.EncodeToString(hash[:8])
	return filepath.Join(h.CacheDir, id, quality, leaf+".bin")
}

func (h *HLSHandler) serveCached(w http.ResponseWriter, r *http.Request, path, ctype string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cache read", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	// Aggressive caching — segments are immutable by content.
	w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
	w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	_, _ = io.Copy(w, f)
}

// drainTo logs ffmpeg stderr per-line.
func drainTo(rc io.Reader, label string) {
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			log.Printf("ffmpeg %s: %s", label, strings.TrimRight(string(buf[:n]), "\n"))
		}
		if err != nil {
			return
		}
	}
}

// StartCacheSweeper deletes cached HLS files older than maxAge every
// `every` interval. Cheap time-based eviction — fine for the current
// scale; a per-pod LRU is overkill at one user.
func (h *HLSHandler) StartCacheSweeper(ctx context.Context, every, maxAge time.Duration) {
	t := time.NewTicker(every)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.sweep(maxAge)
			}
		}
	}()
}

func (h *HLSHandler) sweep(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.Walk(h.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
		return nil
	})
}

// nominalBitrate / nominalResolution are advertised hints for ABR.
func nominalBitrate(q string) int {
	switch q {
	case "high":
		return 6_500_000
	case "medium":
		return 3_000_000
	default:
		return 1_400_000
	}
}

func nominalResolution(q string) string {
	switch q {
	case "high":
		return "1920x1080"
	case "medium":
		return "1280x720"
	default:
		return "854x480"
	}
}
