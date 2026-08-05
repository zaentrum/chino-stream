package play

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
	"github.com/zaentrum/chino-stream/internal/metrics"
)

const passthroughBuf = 64 * 1024

type Handler struct {
	Catalog         *catalog.Client
	MediaRoot       string
	FFmpegBin       string
	FFprobeBin      string
	TranscodePreset string
	// UseNVENC is reflected in /play/info so the player's Playback info
	// dialog can show the user whether their transcode is happening on
	// GPU (h264_nvenc) or CPU (libx264). Set from the same env var as
	// HLSHandler.UseNVENC.
	UseNVENC bool
	// CacheDir is the on-disk root for subtitle .vtt extracts. Shared
	// with HLSHandler.CacheDir so existing sweeper / disk budget logic
	// covers subtitle output too. Empty disables the disk cache and
	// reverts to the old streaming behaviour.
	CacheDir string

	// subLocks dedupes concurrent ffmpeg runs for the same
	// (item, track, startSec) so the player's 26-track mount only
	// fires one extraction even if every <track> requests its url at
	// the same time.
	subLocks sync.Map // map[string]*sync.Mutex
}

// bearerFrom strips "Bearer " from the Authorization header. Used to forward
// the user's OIDC token to katalog-api so it can apply per-tenant access
// rules. Empty when the auth middleware ran in disabled mode.
func bearerFrom(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// Play dispatches a play request: looks up the file, probes its codecs,
// and either streams it directly (with byte-range support) or pipes it
// through ffmpeg. Range requests are honored ONLY in passthrough mode —
// transcoded streams are forward-only because seeking in the source
// requires restarting ffmpeg with an `-ss` offset (left for a later pass).
func (h *Handler) Play(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	if itemID == "" {
		http.Error(w, "missing itemId", http.StatusBadRequest)
		return
	}

	path, err := h.Catalog.PrimaryAssetPath(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, "no playback asset for item", http.StatusNotFound)
			return
		}
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Defense in depth: refuse paths that escape the configured media root.
	clean := filepath.Clean(path)
	if h.MediaRoot != "" && !strings.HasPrefix(clean, filepath.Clean(h.MediaRoot)+string(os.PathSeparator)) {
		log.Printf("play: refusing out-of-root path %q (root=%q)", clean, h.MediaRoot)
		// A server-side misconfiguration, not a client error: the asset is
		// registered at a path the stream service was not told to serve from.
		// Returning 403 made it read as a permissions problem and sent people
		// looking in entirely the wrong place.
		http.Error(w, "this file is stored outside the configured media root — "+
			"check MEDIA_ROOT on chino-stream", http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(clean); err != nil {
		http.Error(w, "file missing on filesystem", http.StatusNotFound)
		return
	}

	probeStart := time.Now()
	probe, err := RunFFprobe(r.Context(), h.FFprobeBin, clean)
	metrics.FFprobeDuration.Observe(time.Since(probeStart).Seconds())
	if err != nil {
		log.Printf("play: ffprobe failed for %s: %v — falling back to passthrough", clean, err)
		metrics.PlayRequests.WithLabelValues("passthrough_fallback").Inc()
		h.passthrough(w, r, clean)
		return
	}
	log.Printf("play %s: video=%s audio=%s container=%s %dx%d",
		itemID, probe.VideoCodec, probe.AudioCodec, probe.Container, probe.Width, probe.Height)

	mode, _ := probe.Decide()
	// `?t=<sec>` (resume position) and `?force_transcode=1` (client-
	// initiated stall recovery on an unstable connection) both force a
	// transcode pipeline even when the codecs would otherwise allow
	// passthrough or remux. Passthrough can't seek by wall-clock time
	// (it serves bytes from offset 0 and relies on HTTP Range from the
	// browser), so once the client asks to resume at second N or
	// explicitly requests a transcoded fallback we must build the
	// ffmpeg pipeline; otherwise the user restarts from the file's
	// beginning every time the network hiccups.
	startSec := 0
	if v := r.URL.Query().Get("t"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			startSec = n
		}
	}
	if startSec > 0 || r.URL.Query().Get("force_transcode") == "1" {
		mode = "transcode"
	}
	w.Header().Set("X-Stream-Mode", mode)
	metrics.PlayRequests.WithLabelValues(mode).Inc()
	switch mode {
	case "transcode", "remux":
		// q controls the rung of the quality ladder for transcode mode;
		// remux ignores it (stream-copy at source bitrate either way).
		// audio selects an alternate audio stream (per-kind index) — used
		// by the player's language switcher.
		q := ResolveQuality(r.URL.Query().Get("q"))
		audioIdx := 0
		if v := r.URL.Query().Get("audio"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(probe.AudioTracks) {
				audioIdx = n
			}
		}
		w.Header().Set("X-Stream-Quality", q.Name)
		w.Header().Set("X-Stream-Start", strconv.Itoa(startSec))
		w.Header().Set("X-Stream-Audio", strconv.Itoa(audioIdx))
		h.transcode(w, r, clean, mode, itemID, q, startSec, audioIdx)
	default:
		h.passthrough(w, r, clean)
	}
}

// sidecarSubtitleMIME maps the catalog's `format` column to the wire
// MIME the clients consume. Bitmap formats keep their native
// container (PGS .sup, VobSub .idx/.sub, DVB .dvb) and the renderer
// runs on the client; for VobSub we additionally need the .sub
// sibling — see siblingPaths.
var sidecarSubtitleMIME = map[string]string{
	"":       "text/vtt; charset=utf-8", // legacy rows pre-format column
	"webvtt": "text/vtt; charset=utf-8",
	"vtt":    "text/vtt; charset=utf-8",
	"srt":    "application/x-subrip; charset=utf-8",
	// Bluray PGS bitstream. MIME matches AndroidX Media3's
	// MimeTypes.APPLICATION_PGS so the same constant works on
	// Android, libpgs-js on web, and the Swift overlay on iOS.
	"pgs": "application/pgs",
	// VobSub is the .idx (palette + index) — the .sub bitmap
	// sibling is served at the same URL with the .sub extension.
	"vobsub": "application/x-vobsub",
	"dvb":    "application/dvb-subtitles",
}

// SidecarSubtitle serves a packaged sidecar subtitle straight off
// the packages PVC. The route accepts an optional extension —
// /api/play/subs/{subID}, /api/play/subs/{subID}.vtt,
// /api/play/subs/{subID}.sup, etc — but the catalog row's `format`
// column is the source of truth for MIME. Extensions are useful
// for clients that infer renderer from URL (Media3's MimeType,
// hls.js track tags) and for cache keys.
//
// Range, ETag, If-Modified-Since all flow through http.ServeFile.
// Sibling files (VobSub .sub next to the .idx) are resolved
// in-handler when the request's extension picks the sibling.
//
// The id is resolved via katalog-api /api/v1/subtitles/{id}/asset.
// The only Postgres round-trip is the catalog lookup; the file
// itself never round-trips.
func (h *Handler) SidecarSubtitle(w http.ResponseWriter, r *http.Request) {
	subID := chi.URLParam(r, "subID")
	if subID == "" {
		http.Error(w, "missing subtitle id", http.StatusBadRequest)
		return
	}
	a, err := h.Catalog.SubtitleAssetByID(r.Context(), subID, bearerFrom(r))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, "unknown subtitle id", http.StatusNotFound)
			return
		}
		http.Error(w, "catalog: "+err.Error(), http.StatusBadGateway)
		return
	}
	clean := filepath.Clean(a.Path)
	// VobSub sibling: a request for /{id}.sub maps to the .sub file
	// next to the catalog row's .idx path. Anything else uses the
	// stored path directly.
	reqExt := strings.ToLower(filepath.Ext(r.URL.Path))
	if a.Format == "vobsub" && reqExt == ".sub" {
		clean = strings.TrimSuffix(clean, filepath.Ext(clean)) + ".sub"
	}
	// Allow under either the packages root or the legacy media root —
	// older sidecars live next to the source file.
	if !strings.HasPrefix(clean, "/var/lib/katalog/packages/") &&
		!(h.MediaRoot != "" && strings.HasPrefix(clean, filepath.Clean(h.MediaRoot)+string(os.PathSeparator))) {
		http.Error(w, "subtitle path outside allowed roots", http.StatusForbidden)
		return
	}
	st, err := os.Stat(clean)
	if err != nil {
		http.Error(w, "subtitle file missing", http.StatusNotFound)
		return
	}
	mime, known := sidecarSubtitleMIME[a.Format]
	if !known {
		http.Error(w, "unsupported subtitle format "+a.Format, http.StatusBadGateway)
		return
	}
	// The .sub bitmap half of a VobSub pair is binary, not text.
	if a.Format == "vobsub" && reqExt == ".sub" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	// Subtitle bytes are immutable per package run; cache aggressively.
	// Range requests on the bitmap formats matter for big PGS files
	// (5–50 MB) where iOS / Android stream from disk rather than
	// pre-parsing in memory.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	http.ServeFile(w, r, clean)
}

// EmbeddedSubtitle extracts an embedded subtitle stream to WebVTT and
// serves it, caching the output on disk so concurrent and repeat
// requests share a single ffmpeg invocation per (item, track, offset).
// Before the cache landed, a file with 26 embedded tracks would spawn
// 26 parallel ffmpeg processes the moment any client mounted them and
// OOM the pod; one per unique key keeps the pipeline bounded.
//
// Cache key includes startSec because the output cue timings shift
// by the same offset (the player asks for `?t=<sec>` after a quality
// switch / forced transcode where the new pipeline starts at that
// source-time). startSec=0 is the dominant case during normal
// playback and stays cached the longest under the sweeper.
func (h *Handler) EmbeddedSubtitle(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	idxStr := chi.URLParam(r, "streamIndex")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		http.Error(w, "bad stream index", http.StatusBadRequest)
		return
	}
	path, err := h.Catalog.PrimaryAssetPath(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, "no playback asset for item", http.StatusNotFound)
			return
		}
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	clean := filepath.Clean(path)
	if h.MediaRoot != "" && !strings.HasPrefix(clean, filepath.Clean(h.MediaRoot)+string(os.PathSeparator)) {
		// A server-side misconfiguration, not a client error: the asset is
		// registered at a path the stream service was not told to serve from.
		// Returning 403 made it read as a permissions problem and sent people
		// looking in entirely the wrong place.
		http.Error(w, "this file is stored outside the configured media root — "+
			"check MEDIA_ROOT on chino-stream", http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(clean); err != nil {
		http.Error(w, "file missing on filesystem", http.StatusNotFound)
		return
	}
	startSec := 0
	if v := r.URL.Query().Get("t"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			startSec = n
		}
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	if startSec > 0 {
		w.Header().Set("Cache-Control", "public, max-age=300")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	// Fall back to the streaming path if no CacheDir is configured —
	// keeps the handler functional in tests / dev where the operator
	// hasn't wired up an HLSCacheDir.
	if h.CacheDir == "" {
		w.WriteHeader(http.StatusOK)
		cmd := BuildEmbeddedSubtitleFFmpeg(r.Context(), h.FFmpegBin, clean, idx, startSec)
		if err := PipeFFmpegTo(cmd, w, itemID+"#sub:"+idxStr); err != nil {
			log.Printf("subtitle extract %s#sub:%s: %v", itemID, idxStr, err)
		}
		return
	}

	cachePath := h.subCachePath(itemID, idx, startSec)
	if err := h.ensureSubtitleCached(r.Context(), clean, idx, startSec, itemID, cachePath); err != nil {
		log.Printf("subtitle extract %s#sub:%d: %v", itemID, idx, err)
		http.Error(w, "subtitle extract failed", http.StatusBadGateway)
		return
	}
	f, err := os.Open(cachePath)
	if err != nil {
		http.Error(w, "subtitle cache read", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// subCachePath mirrors HLSHandler.cachePath's sharded layout so the
// existing cache sweeper picks up these files. Keys include the
// pipeline version so a future change to ffmpeg subtitle flags
// invalidates old entries automatically.
func (h *Handler) subCachePath(itemID string, idx, startSec int) string {
	hash := sha1.Sum([]byte(itemID))
	id := hex.EncodeToString(hash[:8])
	leaf := fmt.Sprintf("%d_%d.vtt", idx, startSec)
	return filepath.Join(h.CacheDir, id, "sub-"+subPipelineVersion, leaf)
}

func (h *Handler) subLockFor(key string) *sync.Mutex {
	mu, _ := h.subLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// ensureSubtitleCached writes the extracted VTT to cachePath if it's
// missing. Concurrent callers for the same (item, idx, startSec) all
// block on a single mutex; the loser of the lock race re-checks the
// file on entry and serves from disk without re-running ffmpeg.
func (h *Handler) ensureSubtitleCached(ctx context.Context, src string, idx, startSec int, itemID, cachePath string) error {
	if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
		return nil
	}
	mu := h.subLockFor(cachePath)
	mu.Lock()
	defer mu.Unlock()
	if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), "sub-*.vtt.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cmd := BuildEmbeddedSubtitleFFmpeg(ctx, h.FFmpegBin, src, idx, startSec)
	label := fmt.Sprintf("%s#sub:%d", itemID, idx)
	if err := PipeFFmpegTo(cmd, tmp, label); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Info returns the codec probe + dispatch decision as JSON so the player
// can show the user why (or why not) the file is being transcoded.
//
// Three modes are reported:
//   - "packaged"   → the analyzer has built a pre-segmented CMAF tree
//     under /var/lib/katalog/packages/{id}/. The player
//     serves video + audio as static byte-range fetches
//     from disk. No request-time ffmpeg.
//   - "transcode"  → legacy on-demand path. Source codec isn't browser-
//     compatible, so chino-stream runs ffmpeg per
//     window to produce HLS segments on the fly.
//   - "remux" / "passthrough" → legacy CMAF/MP4 served byte-range.
//
// The packaged check is cheap (a single stat per cached itemRoot) and
// runs first so a packaged item doesn't kick off an ffprobe just to be
// told something it doesn't apply to.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	if itemID == "" {
		http.Error(w, "missing itemId", http.StatusBadRequest)
		return
	}
	path, err := h.Catalog.PrimaryAssetPath(r.Context(), itemID, bearerFrom(r))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, "no playback asset for item", http.StatusNotFound)
			return
		}
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	clean := filepath.Clean(path)
	if h.MediaRoot != "" && !strings.HasPrefix(clean, filepath.Clean(h.MediaRoot)+string(os.PathSeparator)) {
		// A server-side misconfiguration, not a client error: the asset is
		// registered at a path the stream service was not told to serve from.
		// Returning 403 made it read as a permissions problem and sent people
		// looking in entirely the wrong place.
		http.Error(w, "this file is stored outside the configured media root — "+
			"check MEDIA_ROOT on chino-stream", http.StatusInternalServerError)
		return
	}

	// Packaged mode dispatches before any source-side probing — but
	// only when the client can actually decode the packaged codec.
	// Otherwise we'd advertise mode=packaged to a phone that can't
	// play HEVC, the player would happily mount the static stream,
	// and the user would stare at a black frame at 00:00.
	infoCaps := ParseCaps(r.URL.Query().Get("caps"))
	if HasCompletedPackage(itemID) && PackagedPlayableBy(itemID, infoCaps) {
		mf, err := ReadPackageManifest(itemID)
		if err == nil && mf != nil {
			writePackagedInfo(w, mf)
			return
		}
		// Manifest unreadable — fall through to the source-side probe
		// so the player at least gets *some* info, even if the
		// pipeline label is wrong.
		log.Printf("packaged manifest read failed for %s: %v", itemID, err)
	}

	if _, err := os.Stat(clean); err != nil {
		http.Error(w, "file missing on filesystem", http.StatusNotFound)
		return
	}
	probe, err := RunFFprobe(r.Context(), h.FFprobeBin, clean)
	if err != nil {
		http.Error(w, "ffprobe: "+err.Error(), http.StatusBadGateway)
		return
	}
	// /play/info is the first call the client makes — use its caps
	// hint (same format as master.m3u8's ?caps=) so the mode + reason
	// returned match what the player will actually get when it asks
	// for master.m3u8 a moment later. Without this, Info would always
	// say "passthrough" for h264 sources even when the player is
	// going to be routed to remux because of multichannel audio.
	mode, reason := probe.DecideWith(infoCaps)
	// Surface the available quality rungs so the client can render a
	// picker. Remux / passthrough modes ignore the q parameter, so
	// there's no ladder for them.
	var ladder []map[string]string
	if mode == "transcode" {
		for _, name := range []string{"high", "medium", "low"} {
			ql := QualityLadder[name]
			ladder = append(ladder, map[string]string{"name": ql.Name, "label": ql.Label})
		}
	}
	// Encoder hint for the Playback info dialog. Only meaningful for
	// the transcode path; for passthrough/remux the encoder is the
	// source itself (no re-encode happens).
	encoder := "libx264"
	if h.UseNVENC {
		encoder = "h264_nvenc"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"filename":        filepath.Base(clean),
		"container":       probe.Container,
		"video_codec":     probe.VideoCodec,
		"audio_codec":     probe.AudioCodec,
		"width":           probe.Width,
		"height":          probe.Height,
		"duration_ms":     probe.DurationMs,
		"mode":            mode,
		"reason":          reason,
		"qualities":       ladder,
		"default_quality": "high",
		"audio_tracks":    probe.AudioTracks,
		"subtitle_tracks": probe.SubtitleTracks,
		"encoder":         encoder,
	})
}

// passthrough streams the file directly with full byte-range support
// (Spring's old behaviour, ported to Go).
func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "open: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	size := stat.Size()
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")

	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		n, _ := io.Copy(w, f)
		metrics.BytesServed.WithLabelValues("passthrough").Add(float64(n))
		return
	}
	start, end, ok := parseRange(rng, size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		http.Error(w, "seek: "+err.Error(), http.StatusInternalServerError)
		return
	}
	length := end - start + 1
	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	n, _ := io.CopyN(w, f, length)
	metrics.BytesServed.WithLabelValues("passthrough").Add(float64(n))
}

// transcode runs ffmpeg with the chosen mode and streams the fragmented
// MP4 output. No Range support — Content-Length is unknown until ffmpeg
// finishes, so we use chunked transfer. `q` and `startSec` come from
// the client; the client uses them to step down quality on stalls and to
// resume from the current playback position after a switch.
func (h *Handler) transcode(w http.ResponseWriter, r *http.Request, path, mode, itemID string, q Quality, startSec, audioIdx int) {
	if r.Header.Get("Range") != "" {
		// Browsers issue a Range probe on the very first request. For
		// transcoded streams we can't seek arbitrarily, so reply 200 OK
		// and let the player play forward. Returning 416 would cause the
		// browser to abort.
		w.Header().Set("X-Stream-Range", "ignored-during-transcode")
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cmd := BuildFFmpeg(r.Context(), h.FFmpegBin, h.TranscodePreset, path, mode, q, startSec, audioIdx)
	log.Printf("play %s: %s q=%s start=%ds audio=%d", itemID, mode, q.Name, startSec, audioIdx)

	metrics.TranscodesActive.Inc()
	defer metrics.TranscodesActive.Dec()
	sessionStart := time.Now()
	cw := &countingWriter{w: w, mode: mode}
	if err := PipeFFmpegTo(cmd, cw, itemID); err != nil {
		// At this point headers are already flushed — best we can do is log.
		// ClientAbortedError is normal (browser scrubbed/closed).
		log.Printf("play %s: %s pipe ended: %v", itemID, mode, err)
	}
	metrics.TranscodeDuration.WithLabelValues(mode).Observe(time.Since(sessionStart).Seconds())
}

// countingWriter wraps an http.ResponseWriter to bump the per-mode bytes
// counter as ffmpeg writes its chunked output. Stays a writer-only adapter
// (no need to surface http.Flusher etc. — ffmpeg writes fast enough that
// the underlying writer's buffering is fine).
type countingWriter struct {
	w    interface{ Write([]byte) (int, error) }
	mode string
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		metrics.BytesServed.WithLabelValues(c.mode).Add(float64(n))
	}
	return n, err
}

// parseRange handles `bytes=start-end`, `bytes=start-`, `bytes=-suffix`.
// Returns inclusive start..end (both bounds within file).
func parseRange(h string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	s, e := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if s == "" {
		n, err := strconv.ParseInt(e, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = size - n
		if start < 0 {
			start = 0
		}
		end = size - 1
	} else {
		ss, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = ss
		if e == "" {
			end = size - 1
		} else {
			ee, err := strconv.ParseInt(e, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			end = ee
		}
	}
	if start < 0 || end >= size || start > end {
		return 0, 0, false
	}
	_ = passthroughBuf
	return start, end, true
}
