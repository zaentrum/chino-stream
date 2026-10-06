package play

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
	"github.com/zaentrum/chino-stream/internal/pkgmanifest"
)

// PackagesRoot is the package store of the packages from before the
// library, where the packager wrote them:
//
//	{category}/{shard2}/{itemId}/{manifest.json, .complete, hls/, …}
//	extras/{shard2}/{extraId}/…
//
// An item's package is found through katalog-api (resolve.go), in whichever
// layout it is, and so are the packaged ids; the store is still where an
// extra's package is looked for, until that asks katalog-api too. A
// variable only so tests can point it at packages under testdata.
var PackagesRoot = "/var/lib/katalog/packages"

// packagedFile is the file rel of the package the request r is served from
// — its item's, pinned by the session's v= (resolve.go) — and the retry to
// serve it with should it be missing there: the item resolved once more (a
// version removed after its grace, an answer katalog-api has changed since)
// and the file in the folder answered then; "" when that is the same folder.
// path is "" when the item has no package to serve, err set when katalog-api
// could not say (packagedError).
func (h *HLSHandler) packagedFile(r *http.Request, rel ...string) (path string, retry func() string, err error) {
	itemID := chi.URLParam(r, "itemId")
	pin := r.URL.Query().Get("v")
	p, _, err := h.Packages.itemPackage(r.Context(), itemID, pin, false)
	if err != nil || p == nil {
		if errors.Is(err, catalog.ErrNotFound) {
			err = nil
		}
		return "", nil, err
	}
	return p.path(rel...), func() string {
		h.Packages.forgetComplete(p.dir)
		again, err := h.Packages.itemPackageAgain(r.Context(), itemID, pin, false)
		if err != nil || again == nil || again.dir == p.dir {
			return ""
		}
		return again.path(rel...)
	}, nil
}

// packagedError answers a packaged request katalog-api could not resolve
// (down, nothing cached of the item): 502, which players retry, where a file
// that is not there is a 404.
func packagedError(w http.ResponseWriter, err error) {
	log.Printf("resolve: %v", err)
	http.Error(w, "catalog unavailable", http.StatusBadGateway)
}

// packagedPlayableBy reports whether at least one packaged video
// rendition of mf is HARDWARE-playable on the client: it uses a codec the
// client says it can decode AND its frame height is within the device's
// HW decoder ceiling for that codec family (caps.VideoMaxHeight). A
// package from before renditions.json has a single video rendition
// (HEVC for everything post-2024), so an item the client can't HW-decode
// — wrong codec, OR right codec but the package is 4K and the device's
// HEVC decoder tops out at 1080 — falls through to the on-demand libx264
// transcode ladder (which can downscale) instead of silently dropping
// to the device's software decoder, which on tablets like the SM-T500
// plays 4K HEVC unwatchably slowly. A ladder package plays when one of
// its rungs does; serveLadder then serves the client those rungs.
//
// The height gate is: VideoMaxHeight has no entry for the codec family,
// OR the entry is 0 (no limit), OR rendition.Height <= the entry. A
// rendition that clears the codec check but busts the height ceiling is
// NOT HW-playable; if NO rendition is HW-playable we return false so
// the master/playlist paths fall through to the transcode ladder.
//
// Returns true when the manifest could not be read (mf nil) so we don't
// 404 the player just because we couldn't introspect renditions.
func packagedPlayableBy(mf *pkgmanifest.Manifest, caps Caps) bool {
	if mf == nil || len(mf.Renditions.Video) == 0 {
		return true
	}
	for _, v := range mf.Renditions.Video {
		fam := codecFamily(v.Codec)
		if fam == "" {
			// Unknown codec string — assume playable rather than
			// black-screen the user on a parse miss. No height gate
			// applies because we have no family to look the cap up under.
			return true
		}
		if !videoFamilyOK(fam, caps) {
			continue
		}
		// Codec is supported. Apply the per-family HW height ceiling: an
		// absent entry, a 0 entry, or a rendition no taller than the cap
		// is HW-playable. A rendition taller than the device's HW ceiling
		// for its codec is not — keep scanning in case another rendition
		// (a future fallback rung) fits, otherwise we fall through.
		if maxH := caps.VideoMaxHeight[fam]; maxH > 0 && v.Height > maxH {
			continue
		}
		return true
	}
	return false
}

// codecFamily reduces a master.m3u8-style codec string (avc1.640028,
// hvc1.1.6.L120.B0, hev1.1.6.L120.B0, vp09.00.10.08, av01.0.05M.08)
// to a stable family key matching ParseCaps tokens.
func codecFamily(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case strings.HasPrefix(c, "avc1") || strings.HasPrefix(c, "h264"):
		return "h264"
	case strings.HasPrefix(c, "hvc1") || strings.HasPrefix(c, "hev1") || strings.HasPrefix(c, "hevc") || strings.HasPrefix(c, "h265"):
		return "hevc"
	case strings.HasPrefix(c, "vp09") || strings.HasPrefix(c, "vp9"):
		return "vp9"
	case strings.HasPrefix(c, "av01") || strings.HasPrefix(c, "av1"):
		return "av1"
	}
	return ""
}

// playlistCache memoises the URI-rewritten m3u8 bodies for master and
// per-rendition playlists. The hot path here used to ReadFile + run
// the rewriteM3U8URIs scanner on every request, but the source files
// are static for the life of the package and the only per-request
// variable is the query string (used by rewriteM3U8URIs). Cache by
// (path, query) keyed on file mtime so repackages still get fresh
// content without a pod restart.
type playlistCacheEntry struct {
	body  []byte
	etag  string
	mtime time.Time
}

var playlistCache sync.Map // map[string]*playlistCacheEntry (cacheKey -> entry)

// servePlaylistCached reads + rewrites + caches an m3u8 file, its URIs
// carrying the request's query, then serves it with an ETag and a short
// max-age so browser revisits short-circuit at the cache layer. retry, when
// set, finds the file anew should it be missing (packagedFile).
func servePlaylistCached(w http.ResponseWriter, r *http.Request, path string, retry func() string) {
	servePlaylistCachedTransform(w, r, path, r.URL.RawQuery, retry, nil)
}

// servePlaylistCachedTransform is servePlaylistCached with the query its
// URIs carry and an optional post-rewrite transform applied to the playlist
// body before it's cached and served. Used for the packaged master: the
// client's share of a ladder, and VIDEO-RANGE on HDR titles whose master
// lacks it. The transform runs once per (path, query) cache miss; cache hits
// serve the already-transformed body. It returns the body served (also on a
// 304), nil when there is none.
func servePlaylistCachedTransform(w http.ResponseWriter, r *http.Request, path, query string, retry func() string, transform func(string) string) []byte {
	st, err := os.Stat(path)
	if err != nil && retry != nil {
		if again := retry(); again != "" {
			path = again
			st, err = os.Stat(path)
		}
	}
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return nil
	}
	// Cache key includes the query so different ?stream=…/?caps=…
	// rewrites stay distinct. Cap at the lifetime of the file mtime
	// — different mtime = different entry, so a repackage shows up.
	cacheKey := path + "?" + query
	if v, ok := playlistCache.Load(cacheKey); ok {
		if e := v.(*playlistCacheEntry); e.mtime.Equal(st.ModTime()) {
			if match := r.Header.Get("If-None-Match"); match != "" && match == e.etag {
				w.WriteHeader(http.StatusNotModified)
				return e.body
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "private, max-age=60")
			w.Header().Set("ETag", e.etag)
			_, _ = w.Write(e.body)
			return e.body
		}
	}
	// Route the raw read through packagedCache so a Zap warm
	// (which writes the m3u8 bytes there via warmPackagedFile)
	// actually skips the NFS read on the first per-query miss.
	// playlistCache is keyed by (path, query) and every distinct
	// stream-token query is a fresh miss; without this fall-through
	// every first-fetch of a freshly-cached playlist would still hit
	// NFS and the warm-pool buys nothing for the playlist leg.
	raw, err := readPackagedBytes(path, st.ModTime())
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return nil
	}
	body := rewriteM3U8URIs(string(raw), query)
	if transform != nil {
		body = transform(body)
	}
	out := []byte(body)
	sum := sha1.Sum(out)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	playlistCache.Store(cacheKey, &playlistCacheEntry{body: out, etag: etag, mtime: st.ModTime()})
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return out
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(out)
	return out
}

// servePackagedMaster serves the master of the package p as the client may
// play it (serveLadder: one codec family at the heights its decoder takes,
// the audio groups it decodes, or the one rung ?q= names), with every
// rendition URI rewritten to carry the inbound query string. Without the
// rewrite the player would resolve `v0/playlist.m3u8` against the master's
// URL and drop `?stream=…`, leaving subsequent rendition fetches
// unauthenticated. A library version's master adds v=<versionId>
// (pinnedQuery), which pins the session to that version: a newer one
// superseding it meanwhile, the session ends on the bytes it started on. A
// master with one video rendition and one audio group is served as
// packaged.
//
// Off the request path it warms the variant the client starts on (the
// served master's first variant and its audio rendition): their media
// playlists and init segments, and with ?t=<sec> the segments from there.
// Not the other rungs or audio groups — a ladder's other renditions are
// bytes no player asked for — and no segments without t, since the player
// may start anywhere (a resume, a Zap card's midpoint).
func (h *HLSHandler) servePackagedMaster(w http.ResponseWriter, r *http.Request, p *pkgDir, mf *pkgmanifest.Manifest) {
	caps := ParseCaps(r.URL.Query().Get("caps"))
	q := r.URL.Query().Get("q")
	// An older shaka didn't emit VIDEO-RANGE, so a packaged HDR HEVC
	// rendition is mis-signalled as SDR and HDR displays never engage HDR
	// mode. The package manifest carries a per-rendition HDR flag from a
	// LOCAL file (no OIDC bearer needed — unlike re-resolving the source via
	// katalog-api, which this stream-token'd path can't do), so stamp
	// VIDEO-RANGE onto the variant lines that lack it when the package has
	// any HDR rendition.
	var videoRange func(string) string
	if manifestHasHDR(mf) {
		videoRange = injectVideoRangeTransform(mf)
	}
	served := servePlaylistCachedTransform(w, r, p.path("hls", "master.m3u8"), pinnedQuery(r.URL.RawQuery, p.versionID), nil, func(body string) string {
		if videoRange != nil {
			body = videoRange(body)
		}
		return serveLadder(body, caps, q).body
	})
	if served == nil {
		return
	}
	start := ladderStart(string(served))
	tSec, segments := -1.0, false
	if v := r.URL.Query().Get("t"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			tSec, segments = n, true
		}
	}
	goWarm(func() { warmStart(p, start, tSec, segments) })
}

// pinnedQuery is the query the master of version versionID writes onto its
// URIs: the request's, any v= it had replaced by v=<versionID>, so every
// playlist and segment request of the session names the version it plays
// (resolve.go). The other parameters stay as they came, byte for byte. A
// package from before the library has no version: the query as it came.
func pinnedQuery(raw, versionID string) string {
	if versionID == "" {
		return raw
	}
	var out []string
	for _, kv := range strings.Split(raw, "&") {
		if kv != "" && kv != "v" && !strings.HasPrefix(kv, "v=") {
			out = append(out, kv)
		}
	}
	return strings.Join(append(out, "v="+url.QueryEscape(versionID)), "&")
}

// goWarm runs a cache warm off the request path. Tests run it in place.
var goWarm = func(f func()) { go f() }

// manifestHasHDR reports whether any packaged video rendition is HDR.
func manifestHasHDR(mf *pkgmanifest.Manifest) bool {
	if mf == nil {
		return false
	}
	for _, v := range mf.Renditions.Video {
		if v.HDR {
			return true
		}
	}
	return false
}

// injectVideoRangeTransform returns a playlist transform that appends a
// VIDEO-RANGE attribute to each #EXT-X-STREAM-INF line, keyed by the rendition
// the variant points at: PQ for an HDR rendition, SDR otherwise. The package
// manifest records HDR only as a bool, so HDR maps to PQ (HDR10 — the common
// case); a HLG title would be a rare mislabel, and the display still reads the
// true transfer curve from the stream's own colr/SEI. Lines that already carry
// VIDEO-RANGE are left untouched.
func injectVideoRangeTransform(mf *pkgmanifest.Manifest) func(string) string {
	hdr := make(map[string]bool, len(mf.Renditions.Video))
	for _, v := range mf.Renditions.Video {
		hdr[v.ID] = v.HDR
	}
	return func(body string) string {
		lines := strings.Split(body, "\n")
		for i := range lines {
			if !strings.HasPrefix(lines[i], "#EXT-X-STREAM-INF:") || strings.Contains(lines[i], "VIDEO-RANGE=") {
				continue
			}
			// The variant URI is the next non-empty, non-comment line; its
			// first path segment is the rendition id (e.g. "v0/playlist.m3u8").
			rendID := ""
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t == "" || strings.HasPrefix(t, "#") {
					continue
				}
				if k := strings.IndexByte(t, '/'); k >= 0 {
					t = t[:k]
				}
				rendID = t
				break
			}
			vr := "SDR"
			if hdr[rendID] {
				vr = "PQ"
			}
			line, cr := lines[i], ""
			if strings.HasSuffix(line, "\r") {
				line, cr = strings.TrimSuffix(line, "\r"), "\r"
			}
			lines[i] = line + ",VIDEO-RANGE=" + vr + cr
		}
		return strings.Join(lines, "\n")
	}
}

// PackagedRenditionPlaylist serves the per-rendition playlist.m3u8
// (video, audio or WebVTT), again rewriting segment URIs to carry the
// query string. Path is /api/play/{itemId}/{rendId}/playlist.m3u8.
func (h *HLSHandler) PackagedRenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	path, retry, err := h.packagedFile(r, "hls", chi.URLParam(r, "rendId"), "playlist.m3u8")
	if err != nil {
		packagedError(w, err)
		return
	}
	servePlaylistCached(w, r, path, retry)
}

// PackagedIframesPlaylist serves shaka's I-frame trick-play playlist.
// Same shape as the regular rendition playlist; the player loads it
// when the user scrubs.
func (h *HLSHandler) PackagedIframesPlaylist(w http.ResponseWriter, r *http.Request) {
	path, retry, err := h.packagedFile(r, "hls", chi.URLParam(r, "rendId"), "iframes.m3u8")
	if err != nil {
		packagedError(w, err)
		return
	}
	serveIframesPlaylist(w, r, path, retry)
}

// serveIframesPlaylist serves the I-frame playlist at path, its URIs
// carrying the request's query; retry, when set, finds it anew should it be
// missing.
func serveIframesPlaylist(w http.ResponseWriter, r *http.Request, path string, retry func() string) {
	body, err := os.ReadFile(path)
	if err != nil && retry != nil {
		if again := retry(); again != "" {
			body, err = os.ReadFile(again)
		}
	}
	if err != nil {
		http.Error(w, "iframes not found", http.StatusNotFound)
		return
	}
	out := rewriteM3U8URIs(string(body), r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// PackagedInitSegment serves a rendition's CMAF init.mp4 from the
// packages PVC. Reads into memory once per (path, mtime) and serves
// subsequent Range requests from the buffer — see servePackagedStatic
// for the rationale (NFS Range-read contention).
func (h *HLSHandler) PackagedInitSegment(w http.ResponseWriter, r *http.Request) {
	h.servePackaged(w, r, "video/mp4", "init not found", "hls", chi.URLParam(r, "rendId"), "init.mp4")
}

// PackagedSegment serves one CMAF media segment from the packages
// PVC. Path: /api/play/{itemId}/{rendId}/seg-{seg}.m4s. The {seg}
// value is matched as 5-digit zero-padded in the route so shaka's
// seg-00001.m4s naming flows through unchanged.
func (h *HLSHandler) PackagedSegment(w http.ResponseWriter, r *http.Request) {
	h.servePackaged(w, r, "video/iso.segment", "segment not found",
		"hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".m4s")
}

// PackagedSubtitleSegment serves one WebVTT segment of a packaged
// subtitle rendition. Path: /api/play/{itemId}/{rendId}/seg-{seg}.vtt,
// rendId sN, the name shaka writes (seg-00001.vtt).
func (h *HLSHandler) PackagedSubtitleSegment(w http.ResponseWriter, r *http.Request) {
	h.servePackaged(w, r, "text/vtt; charset=utf-8", "subtitle segment not found",
		"hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".vtt")
}

// servePackaged serves the file rel of the request's package (packagedFile)
// as servePackagedStatic does.
func (h *HLSHandler) servePackaged(w http.ResponseWriter, r *http.Request, contentType, notFoundMsg string, rel ...string) {
	path, retry, err := h.packagedFile(r, rel...)
	if err != nil {
		packagedError(w, err)
		return
	}
	servePackagedStaticRetry(w, r, path, retry, contentType, notFoundMsg)
}

// packagedFileCache is an in-memory LRU-ish cache for packaged
// static files. Keyed by path; the value is the full file contents
// plus mtime + last-touch time.
//
// Why we have it: http.ServeFile uses Range-aware streaming with a
// live file descriptor across the whole response. On the packages
// PVC (NFS), three parallel Range requests for the same segment
// (typical hls.js behaviour during scrubs) take 6-56 seconds each
// because the NFS client serialises reads on the inode. Slurping
// the file once into a []byte and serving subsequent ranges from
// memory drops latency from tens of seconds to sub-ms.
//
// Size budget: chino-stream pod limit is 4 GiB; cap the cache at
// ~512 MiB so transcode buffers + Go heap + page cache still fit.
// Evictions are LRU-ish: when total bytes exceed the cap, walk the
// map and drop entries with oldest lastTouch until under budget.
const packagedCacheBudgetBytes = 512 * 1024 * 1024

type packagedCacheEntry struct {
	bytes     []byte
	mtime     time.Time
	lastTouch atomic.Int64 // unix nanos
}

var (
	packagedCacheMu      sync.Mutex
	packagedCacheLoading sync.Map // map[string]*sync.Mutex — per-path read serialisation
	packagedCache        sync.Map // map[string]*packagedCacheEntry
	packagedCacheBytes   atomic.Int64

	// zapPinnedPaths holds packagedCache keys the eviction sweep must NOT
	// drop — the bytes the live Zap warm pool depends on. Without this the
	// pool's pre-warmed bytes (untouched between warm and serve) are the
	// oldest-touched and get LRU-evicted by any sustained playback, leaving
	// /zap-feed perpetually empty (warmed:0) and clients stuck on their
	// movies-only cold fallback. The pool is ≤8 entries each warming only a
	// master + one rendition's playlist+init+~2 segments, so pinned bytes
	// stay a small fraction of the 512 MiB budget. The zap pool worker
	// PinPaths on add and UnpinPaths on drop (zappool.go).
	zapPinnedPaths sync.Map // map[string]struct{}
)

func packagedLoadLock(path string) *sync.Mutex {
	mu, _ := packagedCacheLoading.LoadOrStore(path, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// servePackagedStatic serves a file from the packages PVC with an
// in-memory cache that survives Range requests + concurrent
// duplicates. Returns 404 with `notFoundMsg` if the file doesn't
// exist; 500 if the read fails.
func servePackagedStatic(w http.ResponseWriter, r *http.Request, path, contentType, notFoundMsg string) {
	servePackagedStaticRetry(w, r, path, nil, contentType, notFoundMsg)
}

// servePackagedStaticRetry is servePackagedStatic with retry, which finds
// the file anew should it be missing at path (packagedFile).
func servePackagedStaticRetry(w http.ResponseWriter, r *http.Request, path string, retry func() string, contentType, notFoundMsg string) {
	st, err := os.Stat(path)
	if err != nil && retry != nil {
		if again := retry(); again != "" {
			path = again
			st, err = os.Stat(path)
		}
	}
	if err != nil {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return
	}
	mtime := st.ModTime()

	// Cache hit (matching mtime) → serve from memory.
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
			http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
			return
		}
	}

	// Cache miss. Serialise concurrent loaders of the same path so
	// only one goroutine pays the NFS read cost; the others wait
	// on the mutex and then hit the cache.
	mu := packagedLoadLock(path)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
			http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
			return
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "read failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entry := storePackaged(path, data, mtime)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
}

// evictPackagedCache drops the oldest-touched entries until the
// total cached byte count is back under budget. Single-threaded via
// packagedCacheMu so we don't fight ourselves under load spikes.
func evictPackagedCache() {
	packagedCacheMu.Lock()
	defer packagedCacheMu.Unlock()
	if packagedCacheBytes.Load() <= packagedCacheBudgetBytes {
		return
	}
	type kv struct {
		path  string
		entry *packagedCacheEntry
	}
	all := make([]kv, 0, 256)
	packagedCache.Range(func(k, v any) bool {
		all = append(all, kv{k.(string), v.(*packagedCacheEntry)})
		return true
	})
	sort.Slice(all, func(i, j int) bool {
		return all[i].entry.lastTouch.Load() < all[j].entry.lastTouch.Load()
	})
	for _, kv := range all {
		if packagedCacheBytes.Load() <= packagedCacheBudgetBytes*8/10 {
			break // drop to 80% so we don't immediately re-evict
		}
		// Never evict bytes the live Zap warm pool depends on — keeping
		// /zap-feed populated under playback load. Pinned set is small
		// (≤8 pool entries), so skipping them can't starve the cache.
		if _, pinned := zapPinnedPaths.Load(kv.path); pinned {
			continue
		}
		if packagedCache.CompareAndDelete(kv.path, kv.entry) {
			packagedCacheBytes.Add(-int64(len(kv.entry.bytes)))
		}
	}
}

// PinPaths / UnpinPaths protect (resp. release) packagedCache keys from
// the eviction sweep. Used by the Zap pool worker to keep its pre-warmed
// bytes resident for the life of a pool entry.
func PinPaths(paths []string) {
	for _, p := range paths {
		zapPinnedPaths.Store(p, struct{}{})
	}
}

func UnpinPaths(paths []string) {
	for _, p := range paths {
		zapPinnedPaths.Delete(p)
	}
}

// warmPackaged primes the caches for the package p OFF the request
// path so the player's follow-up master / playlist / init / segment
// fetches all land hot. It is fire-and-forget, panic-safe, and bounded:
// the master plus the variant the client starts on, nothing else.
//
// What it warms:
//
//	(a) master.m3u8, into packagedCache — the real servePlaylistCached
//	    read is then hot (the rewrite+cache step itself is cheap; the
//	    NFS read was the slow part).
//	(b) the start of the master the client is served for caps and q
//	    (serveLadder): its first variant's video rendition and that
//	    variant's audio rendition — media playlist, init.mp4 (a 924-byte
//	    init.mp4 was seen stalling 18.4s cold under NFS contention) and
//	    the segments at tSec (pickWarmSegments; the first ones when
//	    tSec<=0). A ladder's other rungs and audio groups are not
//	    warmed: a client starts on one variant, and warming every
//	    rendition of a ladder filled the cache with bytes no player
//	    asked for.
//
// A client that decodes none of the package's rungs is not warmed at
// all (packagedPlayableBy): it falls through to the transcode.
func (h *HLSHandler) warmPackaged(p *pkgDir, mf *pkgmanifest.Manifest, caps Caps, q string, tSec float64) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("warmPackaged %s panic: %v", p.dir, rec)
		}
	}()
	if !packagedPlayableBy(mf, caps) {
		return
	}
	warmPackagedFile(p.path("hls", "master.m3u8"))
	master, err := readPackagedMaster(p)
	if err != nil {
		return
	}
	warmStart(p, serveLadder(master, caps, q), tSec, true)
}

// warmStart warms the variant a client starts on: s.video and s.audio,
// each its media playlist and init.mp4, and with segments the segments
// at tSec (the first ones when tSec<=0). It returns the paths that are
// in packagedCache afterwards and whether the video rendition is
// playable from there: its playlist, its init and, with segments, at
// least one segment landed.
func warmStart(p *pkgDir, s servedLadder, tSec float64, segments bool) (paths []string, videoOK bool) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("warmStart %s panic: %v", p.dir, rec)
		}
	}()
	paths, videoOK = warmRendition(p, s.video, tSec, segments)
	if !videoOK {
		return nil, false
	}
	if s.audio != "" {
		audio, _ := warmRendition(p, s.audio, tSec, segments)
		paths = append(paths, audio...)
	}
	return paths, true
}

// warmRendition warms one packaged rendition (see warmStart). ok: its
// playlist and init landed, and with segments at least one segment.
func warmRendition(p *pkgDir, rendID string, tSec float64, segments bool) (paths []string, ok bool) {
	if rendID == "" {
		return nil, false
	}
	playlistPath := p.path("hls", rendID, "playlist.m3u8")
	initPath := p.path("hls", rendID, "init.mp4")
	if !warmPackagedFile(playlistPath) || !warmPackagedFile(initPath) {
		return nil, false
	}
	paths = []string{playlistPath, initPath}
	if !segments {
		return paths, true
	}
	for _, seg := range pickWarmSegments(playlistPath, tSec) {
		if path := p.path("hls", rendID, seg); warmPackagedFile(path) {
			paths = append(paths, path)
		}
	}
	return paths, len(paths) > 2
}

// pickWarmSegments parses a rendition playlist.m3u8 and returns the
// seg-*.m4s file name(s) to warm: when tSec>0 the segment whose
// cumulative [start,end) contains tSec plus the immediately following
// one; otherwise the first two segments. Bounded to at most 2 entries.
// Returns nil if the playlist can't be read/parsed (warmPackagedFile
// then simply has fewer files to warm).
//
// Parsing model: shaka emits a VOD playlist of alternating
// `#EXTINF:<dur>,` tag lines and bare URI lines (`seg-00001.m4s`). We
// accumulate EXTINF durations to track the start time of each segment
// and capture the URI that follows each EXTINF. Tag lines other than
// EXTINF (#EXT-X-MAP, #EXT-X-ENDLIST, …) and blank lines are skipped.
func pickWarmSegments(playlistPath string, tSec float64) []string {
	raw, err := os.ReadFile(playlistPath)
	if err != nil {
		return nil
	}
	type segEntry struct {
		uri   string
		start float64
	}
	var segs []segEntry
	var cum float64
	pendingDur := -1.0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXTINF:"):
			// "#EXTINF:5.994," → parse the duration up to the comma.
			v := strings.TrimPrefix(line, "#EXTINF:")
			if c := strings.IndexByte(v, ','); c >= 0 {
				v = v[:c]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				pendingDur = d
			}
		case strings.HasPrefix(line, "#"):
			// Other tag — ignore.
			continue
		default:
			// URI line. Strip any query string (shouldn't be present
			// on disk, but be defensive). Only count it as a segment
			// when a preceding EXTINF gave it a duration.
			uri := line
			if q := strings.IndexByte(uri, '?'); q >= 0 {
				uri = uri[:q]
			}
			dur := pendingDur
			pendingDur = -1.0
			if dur < 0 {
				continue
			}
			segs = append(segs, segEntry{uri: uri, start: cum})
			cum += dur
		}
	}
	if len(segs) == 0 {
		return nil
	}

	startIdx := 0
	if tSec > 0 {
		// Find the segment whose [start, start+dur) contains tSec. We
		// only stored start times, so the containing segment is the
		// last one whose start <= tSec.
		startIdx = 0
		for i, s := range segs {
			if s.start <= tSec {
				startIdx = i
			} else {
				break
			}
		}
	}

	// Warm a 4-segment window starting at the picked index. At the
	// ~6 s/segment shaka emits, that's ~24 s of media — comfortably
	// above ZapCard's hls.js maxBufferLength=20 setting so the player
	// can satisfy its initial buffer entirely from packagedCache
	// without a third / fourth NFS-bound segment fetch. Stops short
	// at the playlist's tail.
	const warmSegments = 4
	out := make([]string, 0, warmSegments)
	for i := 0; i < warmSegments && startIdx+i < len(segs); i++ {
		out = append(out, segs[startIdx+i].uri)
	}
	return out
}

// readPackagedBytes returns the raw on-disk bytes for `path`, going
// through packagedCache so a prior warm (or a previous serve) skips
// the NFS read. The mtime arg is the value the caller already stat'd;
// passing it in keeps a single Stat at the call site rather than
// double-stat'ing here. Cache entries with a stale mtime are ignored
// (a repackage's fresh bytes win).
//
// This is the lookup the playlist serve path uses to avoid the NFS
// ReadFile leg on a per-query cache miss in playlistCache. Segment
// serving uses servePackagedStatic directly which already manages
// the same cache plus the http write — keeping that path inline
// avoids the extra alloc + copy here.
func readPackagedBytes(path string, mtime time.Time) ([]byte, error) {
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			return entry.bytes, nil
		}
	}
	mu := packagedLoadLock(path)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			return entry.bytes, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return storePackaged(path, data, mtime).bytes, nil
}

// storePackaged puts path's bytes, read when the file had mtime, in
// packagedCache in place of whatever it held for path. An entry of another
// mtime is the file as it was: a title packaged again writes the same paths,
// and keeping that entry served the earlier package's master, playlists and
// segments until the entry was evicted, which a path the Zap pool pins never
// is. The caller holds path's load lock (packagedLoadLock).
func storePackaged(path string, data []byte, mtime time.Time) *packagedCacheEntry {
	entry := &packagedCacheEntry{bytes: data, mtime: mtime}
	entry.lastTouch.Store(time.Now().UnixNano())
	delta := int64(len(data))
	if old, loaded := packagedCache.Swap(path, entry); loaded {
		delta -= int64(len(old.(*packagedCacheEntry).bytes))
	}
	if packagedCacheBytes.Add(delta) > packagedCacheBudgetBytes {
		go evictPackagedCache()
	}
	return entry
}

// warmPackagedFile loads a packaged static file into packagedCache
// via readPackagedBytes. Returns true when the file is resident in
// packagedCache after the call (either freshly loaded or already
// there with matching mtime), false when the file is missing / the
// NFS read errored / the input was empty. Safe to call concurrently;
// the per-path load lock inside readPackagedBytes dedupes with the
// real serve path so a warm and a real request never double-read.
// The bool lets the Zap warm-pool worker count how many of the legs
// it needed actually landed, so a corrupt / half-packaged item
// doesn't get published as a "warmed" pool entry that then serves
// nothing but NFS misses.
func warmPackagedFile(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if _, err := readPackagedBytes(path, st.ModTime()); err != nil {
		return false
	}
	return true
}

// PackagedTrickplayVTT serves the WebVTT cue file that points scrub-
// preview thumbnails to their position inside the sprite sheets.
// Path: /api/play/{itemId}/trickplay/thumbnails.vtt.
func (h *HLSHandler) PackagedTrickplayVTT(w http.ResponseWriter, r *http.Request) {
	h.servePackaged(w, r, "text/vtt; charset=utf-8", "trickplay vtt not found", "trickplay", "thumbnails.vtt")
}

// PackagedTrickplaySprite serves one sprite-sheet JPG. The VTT cues
// reference these by relative name (sprite-NNNN.jpg) so the player's
// resolved URL lands here.
func (h *HLSHandler) PackagedTrickplaySprite(w http.ResponseWriter, r *http.Request) {
	h.servePackaged(w, r, "image/jpeg", "trickplay sprite not found", "trickplay", "sprite-"+chi.URLParam(r, "n")+".jpg")
}

// rewriteM3U8URIs walks every line of an m3u8 and appends ?query (or
// merges with an existing ?…) to every line that is a URI. Lines
// starting with '#' are tag lines, except we also have to mutate the
// URI="…" attribute inside #EXT-X-MEDIA tags and the URI="…" inside
// #EXT-X-I-FRAME-STREAM-INF.
//
// We keep the input line endings as-is; shaka emits unix '\n' which is
// what every modern player expects.
func rewriteM3U8URIs(body, query string) string {
	if query == "" {
		return body
	}
	var sb strings.Builder
	sb.Grow(len(body) + 128)
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			sb.WriteByte('\n')
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(line, "#EXT-X-MAP:"):
			sb.WriteString(rewriteTagURIAttr(line, query))
			sb.WriteByte('\n')
		case strings.HasPrefix(line, "#"):
			sb.WriteString(line)
			sb.WriteByte('\n')
		default:
			sb.WriteString(appendQuery(line, query))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// rewriteTagURIAttr finds URI="…" inside an HLS tag line and appends
// ?query to the captured value, preserving everything else. Returns
// the line unchanged when no URI attribute is present.
func rewriteTagURIAttr(line, query string) string {
	const marker = `URI="`
	i := strings.Index(line, marker)
	if i < 0 {
		return line
	}
	start := i + len(marker)
	end := strings.IndexByte(line[start:], '"')
	if end < 0 {
		return line
	}
	end += start
	uri := line[start:end]
	return line[:start] + appendQuery(uri, query) + line[end:]
}

// appendQuery returns uri with ?query (or &query) appended.
func appendQuery(uri, query string) string {
	if strings.ContainsRune(uri, '?') {
		return uri + "&" + query
	}
	return uri + "?" + query
}

// manifestCache memoises parsed package records per path, invalidated by
// file mtime. The Master / Info / packagedPlayableBy hot path used to
// ReadFile + Unmarshal on EVERY request — at 5-30 ms per call this
// dominated packaged.m3u8 latency for Zap (CDP probe 2026-06-02). Cached
// lookups are sub-microsecond.
type manifestCacheEntry struct {
	mf    *pkgmanifest.Manifest
	mtime time.Time
}

// manifestCache is keyed by the record's path: a version's package.json, a
// package's or an extra's manifest.json.
var manifestCache sync.Map // map[string]*manifestCacheEntry (path -> entry)

// readPkgManifest is the Manifest of the package p: its package.json in the
// library (pkgmanifest.FromPackageRecord), its manifest.json before it.
// Returns nil + an error on read or parse failure — the Info handler then
// logs and falls through to the source-side probe so the player at least
// gets *some* info.
//
// The parsed manifest is cached in-process keyed on the file's mtime,
// so an operator who repackages an item picks up the new manifest on
// the next request without a pod restart.
func readPkgManifest(p *pkgDir) (*pkgmanifest.Manifest, error) {
	if p == nil {
		return nil, os.ErrNotExist
	}
	decode := decodeManifest
	if p.library() {
		decode = pkgmanifest.FromPackageRecord
	}
	return readManifest(p.path(p.record), decode)
}

// decodeManifest parses a manifest.json.
func decodeManifest(raw []byte) (*pkgmanifest.Manifest, error) {
	var m pkgmanifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// readManifest parses the record at path with decode, cached in
// manifestCache until the file's mtime changes.
func readManifest(path string, decode func([]byte) (*pkgmanifest.Manifest, error)) (*pkgmanifest.Manifest, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if v, ok := manifestCache.Load(path); ok {
		if e := v.(*manifestCacheEntry); e.mtime.Equal(st.ModTime()) {
			return e.mf, nil
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := decode(raw)
	if err != nil {
		return nil, err
	}
	manifestCache.Store(path, &manifestCacheEntry{mf: m, mtime: st.ModTime()})
	return m, nil
}

// libraryTitle is the title (and release year, 0 when unknown) of the item
// whose library version folder is dir: the primary title of its
// metadata.json, which katalog-manager projects whenever the catalog
// changes, else the title its item.json was created with. A version's
// package.json names no item; the item's own records do. "" when neither
// can be read.
func libraryTitle(dir string) (string, int) {
	item := filepath.Dir(filepath.Dir(dir)) // <item>/versions/<versionId>
	if title, year := recordTitle(filepath.Join(item, "metadata.json")); title != "" {
		return title, year
	}
	return recordTitle(filepath.Join(item, "item.json"))
}

// recordTitles caches recordTitle per path, by mtime.
var recordTitles sync.Map // map[string]recordTitleEntry

type recordTitleEntry struct {
	title string
	year  int
	mtime time.Time
}

// recordTitle reads the title of an item record at path: a metadata.json's
// titles.primary and the year of its releaseDate, or an item.json's title.
func recordTitle(path string) (string, int) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0
	}
	if v, ok := recordTitles.Load(path); ok {
		if e := v.(recordTitleEntry); e.mtime.Equal(st.ModTime()) {
			return e.title, e.year
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0
	}
	var rec struct {
		Title  string `json:"title"`
		Titles struct {
			Primary string `json:"primary"`
		} `json:"titles"`
		ReleaseDate string `json:"releaseDate"`
	}
	if json.Unmarshal(raw, &rec) != nil {
		return "", 0
	}
	e := recordTitleEntry{title: rec.Titles.Primary, mtime: st.ModTime()}
	if e.title == "" {
		e.title = rec.Title
	}
	if len(rec.ReleaseDate) >= 4 {
		e.year, _ = strconv.Atoi(rec.ReleaseDate[:4])
	}
	recordTitles.Store(path, e)
	return e.title, e.year
}

// writePackagedInfo emits the /play/info JSON shape for a packaged
// item. The fields match what chino-web's PlayerPage panel expects so
// it stops claiming a transcode is happening when none is.
//
// mode="packaged" + reason="…" is the load-bearing change: the player
// branches on mode and stops drawing the "Transcode required" badge
// for that value. Audio tracks are taken from the manifest's actual
// renditions (post-downmix, stereo AAC), not from a fresh probe of
// the source file (which would still report the original surround
// codec), each by its name (packagedAudioTracks), never by the source's
// title.
//
// The video fields describe the rung the client starts on: the first
// variant of the master it is served for caps and q. qualities is the
// choice it may offer (packagedQualities) — null for a package with one
// rendition, as before — and default_quality is "auto": the client's
// ladder, adaptive. Clients put the name they pick in ?q=.
func writePackagedInfo(w http.ResponseWriter, p *pkgDir, mf *pkgmanifest.Manifest, caps Caps, q string) {
	video := pkgmanifest.VideoRendition{}
	if len(mf.Renditions.Video) > 0 {
		video = mf.Renditions.Video[0]
	}
	var qualities []map[string]any
	if master, err := readPackagedMaster(p); err == nil {
		start := serveLadder(master, caps, q).video
		for _, v := range mf.Renditions.Video {
			if v.ID == start {
				video = v
			}
		}
		qualities = packagedQualities(master, caps)
	}
	audioTracks := packagedAudioTracks(mf.Renditions.Audio)
	// v2 manifests put the title at the top level; v1 manifests had
	// only Source.Path. Prefer Title when present; fall back to the
	// source-path basename for legacy packages. A library version's
	// record names no title: the item's records do.
	filename := mf.Title
	if filename == "" && mf.Source != nil {
		filename = filepath.Base(mf.Source.Path)
	}
	if filename == "" && p.library() {
		filename, _ = libraryTitle(p.dir)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"filename":        filename,
		"container":       "cmaf",
		"video_codec":     video.Codec,
		"audio_codec":     "aac",
		"width":           video.Width,
		"height":          video.Height,
		"duration_ms":     mf.EffectiveDurationMs(),
		"mode":            "packaged",
		"reason":          "pre-segmented CMAF on disk; served as static byte-range fetches with no request-time ffmpeg",
		"qualities":       qualities,
		"default_quality": "auto",
		"audio_tracks":    audioTracks,
		"subtitle_tracks": packagedSubtitleTracks(mf.Subtitles),
	})
}

// packagedAudioTracks are /play/info's audio_tracks of a package, one per
// stereo rendition: its name as the packager wrote it ("English", "No
// dialogue", "English · Commentary"), else - a manifest from before names -
// trackDisplayName of its language and title, unique as the master's NAMEs
// are. title is the same name, for a client that reads title: the source's
// title as it is, free text that is often its codec ("AC3 5.1 @ 640 Kbps"),
// is never a label.
func packagedAudioTracks(renditions []pkgmanifest.AudioRendition) []map[string]any {
	names := make([]string, len(renditions))
	for i, a := range renditions {
		if names[i] = strings.TrimSpace(a.Name); names[i] == "" {
			names[i] = trackDisplayName(a.Language, a.Title)
		}
	}
	names = uniqueNames(names)
	out := make([]map[string]any, 0, len(renditions))
	for i, a := range renditions {
		out = append(out, map[string]any{
			"index":    i,
			"codec":    a.Codec, // always mp4a in packaged mode
			"language": a.Language,
			"name":     names[i],
			"title":    names[i],
			"default":  a.Default,
			"channels": a.Channels, // always 2 in packaged mode
		})
	}
	return out
}

// packagedSubtitleTracks are /play/info's subtitle_tracks of a package, its
// sidecars as the manifest lists them - the source's own tracks, then the
// subtitle files from next to it (external), whose sN count on past them -
// each with its name: the packager's, else subtitleDisplayName of its
// language, title and forced flag. title is the same name, never the
// source's title as it is.
func packagedSubtitleTracks(subs []pkgmanifest.Subtitle) []map[string]any {
	out := make([]map[string]any, 0, len(subs))
	for _, s := range subs {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			name = subtitleDisplayName(s.Language, s.Title, s.Forced)
		}
		e := map[string]any{"id": s.ID, "path": s.Path, "language": s.Language, "name": name, "title": name, "format": s.Format}
		if s.Default {
			e["default"] = true
		}
		if s.Forced {
			e["forced"] = true
		}
		if s.HLS != "" {
			e["hls"] = s.HLS
		}
		if s.External {
			e["external"] = true
		}
		out = append(out, e)
	}
	return out
}

// readPackagedMaster is the package's hls/master.m3u8 as packaged, through
// packagedCache.
func readPackagedMaster(p *pkgDir) (string, error) {
	path := p.path("hls", "master.m3u8")
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	b, err := readPackagedBytes(path, st.ModTime())
	return string(b), err
}
