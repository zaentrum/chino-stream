package play

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/pkgmanifest"
)

// PackagesRoot is where katalog-analyzer writes the per-item CMAF
// trees. Mounted RO into chino-stream pods via the katalog-packages
// PVC; the path here must match the volumeMount in k8s/deployment.yaml.
//
// Layout under PackagesRoot is sharded to keep any single directory's
// child count bounded:
//
//	{category}/{shard2}/{itemId}/{manifest.json, .complete, hls/, …}
//
// where category is one of {movies, shows, music, other} (mapped from
// the katalog item type) and shard2 is the first two hex chars of the
// item uuid. At 10k items per category, ~40 per shard — every fs
// (incl. NFS) reads that fast.
const PackagesRoot = "/var/lib/katalog/packages"

// Known top-level category dirs the analyzer writes to. The stream
// side probes these on read to locate a package for a given item id
// (it doesn't know the item type from the URL alone).
var packageCategories = []string{"movies", "shows", "music", "other"}

// itemRootCache memoises the resolved package directory per item id
// so subsequent requests skip the probe loop. Entries are kept until
// pod restart — the only invalidation we'd ever need is when an item
// gets repackaged into a different category, which doesn't happen in
// practice (category is derived from a stable item type).
var itemRootCache sync.Map // map[string]string (itemId -> abs package dir)

// itemRoot returns the on-disk package directory for the given item
// id, probing each category until one is found. Returns "" when the
// item has no package on any category. Result is cached.
func itemRoot(itemID string) string {
	if itemID == "" {
		return ""
	}
	if v, ok := itemRootCache.Load(itemID); ok {
		return v.(string)
	}
	if len(itemID) < 2 {
		return ""
	}
	shard := strings.ToLower(itemID[:2])
	for _, cat := range packageCategories {
		path := filepath.Join(PackagesRoot, cat, shard, itemID)
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			itemRootCache.Store(itemID, path)
			return path
		}
	}
	return ""
}

// packagedIDsCache memoises the directory walk under PackagesRoot so
// the listing endpoint doesn't restat ~40k dirs on every Zap session
// open. 60-second TTL is short enough to pick up new packages within
// a single user session, long enough that the FS walk amortises to
// zero on busy paths.
//
// The cache is served stale-while-revalidate: a caller never blocks on
// the NFS walk except on the very first cold call (no value yet). Once
// a value exists, an expired TTL hands back the stale slice immediately
// and kicks a SINGLE background refresh (guarded by an atomic in-flight
// flag) that swaps in a fresh slice when it completes. This matters
// because the walk was measured at p90 19s / max 37.8s under NFS
// contention — holding a mutex across it blocked every caller for the
// whole walk on each 60s TTL expiry.
var (
	packagedIDsCacheMu    sync.RWMutex // guards packagedIDsCacheAt + Val
	packagedIDsCacheAt    time.Time
	packagedIDsCacheVal   []string
	packagedIDsRefreshing atomic.Bool // single-flight guard for the bg walk
)

const packagedIDsCacheTTL = 60 * time.Second

// ListCompletedPackageIDs returns the ids of every item with a finished
// .complete sentinel. Stale-while-revalidate: the cached slice is
// returned in microseconds and the slow NFS walk NEVER lands on the
// request path — not even on a cold start. When the value is missing or
// stale, a SINGLE background goroutine (single-flighted via the
// packagedIDsRefreshing CAS) refreshes it and swaps in a fresh slice
// under a short lock; the current value is returned right away. On a
// true cold start (no value yet, e.g. the first ~seconds after a pod
// restart) callers get an empty slice while that one background walk
// runs — the Zap feed degrades gracefully on an empty packaged set, far
// cheaper than a thundering herd of multi-second NFS walks on boot.
//
// Used by the /api/play/packaged-ids endpoint that the Zap pager
// consults to filter its candidate pool to instant-start items —
// packaged items skip ffmpeg entirely and serve in tens of ms,
// avoiding the 1-3s cold start that on-demand transcode imposes.
func ListCompletedPackageIDs() []string {
	packagedIDsCacheMu.RLock()
	val := packagedIDsCacheVal
	age := time.Since(packagedIDsCacheAt)
	packagedIDsCacheMu.RUnlock()

	// Cold (no value) or stale (past TTL): kick a single-flight background
	// refresh and return whatever we have now — never block on the walk.
	if val == nil || age >= packagedIDsCacheTTL {
		if packagedIDsRefreshing.CompareAndSwap(false, true) {
			go refreshCompletedPackageIDs()
		}
	}
	if val == nil {
		return []string{} // cold: serve empty until the first walk lands
	}
	return val
}

// refreshCompletedPackageIDs runs the NFS walk off the request path and
// swaps the result into the cache under a short write lock. The shared
// slice is SWAPPED (never mutated in place) so a concurrent reader
// holding the old slice never races with the writer. Panic-safe and
// always clears the in-flight guard so a failed walk can be retried on
// the next stale read.
func refreshCompletedPackageIDs() {
	defer packagedIDsRefreshing.Store(false)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("packaged-ids background refresh panic: %v", rec)
		}
	}()
	ids := walkCompletedPackageIDs()
	packagedIDsCacheMu.Lock()
	packagedIDsCacheVal = ids
	packagedIDsCacheAt = time.Now()
	packagedIDsCacheMu.Unlock()
}

// walkCompletedPackageIDs is the actual directory walk under
// PackagesRoot, factored out of ListCompletedPackageIDs so both the
// synchronous cold-start path and the background refresh share one
// implementation. The walk is sharded (4 categories × 256 shards ×
// ~40 items) so even a fully-populated catalogue is sub-second on
// local disk, ~1-3s on warm NFS, and up to tens of seconds when the
// NFS client is contended. Returns a freshly-allocated slice.
func walkCompletedPackageIDs() []string {
	ids := make([]string, 0, 256)
	for _, cat := range packageCategories {
		catDir := filepath.Join(PackagesRoot, cat)
		shards, err := os.ReadDir(catDir)
		if err != nil {
			continue
		}
		for _, sh := range shards {
			if !sh.IsDir() {
				continue
			}
			shDir := filepath.Join(catDir, sh.Name())
			items, err := os.ReadDir(shDir)
			if err != nil {
				continue
			}
			for _, it := range items {
				if !it.IsDir() {
					continue
				}
				if _, err := os.Stat(filepath.Join(shDir, it.Name(), ".complete")); err == nil {
					ids = append(ids, it.Name())
				}
			}
		}
	}
	return ids
}

// HasCompletedPackage reports whether the given item has a finished
// CMAF package on disk. Cheap on a cache hit (single stat); cold-path
// is at most len(packageCategories) stats. Used by the master / init
// / segment handlers to dispatch between "serve static files from
// /media/packages" and "fall through to the legacy on-demand
// transcode".
func HasCompletedPackage(itemID string) bool {
	root := itemRoot(itemID)
	if root == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, ".complete"))
	return err == nil && !st.IsDir()
}

// packagePath joins {item-root}/rel safely. The chi URL params are
// constrained by route regex (^v[0-9]+$|^a[0-9]+$ for rendId, digits
// for seg) so a hostile rendId can't traverse out of the item
// directory, but we still filepath.Clean before stat as a belt-and-
// braces measure.
func packagePath(itemID string, rel ...string) string {
	root := itemRoot(itemID)
	if root == "" {
		return ""
	}
	parts := append([]string{root}, rel...)
	return filepath.Clean(filepath.Join(parts...))
}

// PackagedPlayableBy reports whether at least one packaged video
// rendition is HARDWARE-playable on the client: it uses a codec the
// client says it can decode AND its frame height is within the device's
// HW decoder ceiling for that codec family (caps.VideoMaxHeight). The
// current packager only emits a single video rendition (HEVC for
// everything post-2024), so an item the client can't HW-decode — wrong
// codec, OR right codec but the package is 4K and the device's HEVC
// decoder tops out at 1080 — falls through to the on-demand libx264
// transcode ladder (which can downscale) instead of silently dropping
// to the device's software decoder, which on tablets like the SM-T500
// plays 4K HEVC unwatchably slowly.
//
// The height gate is: VideoMaxHeight has no entry for the codec family,
// OR the entry is 0 (no limit), OR rendition.Height <= the entry. A
// rendition that clears the codec check but busts the height ceiling is
// NOT HW-playable; if NO rendition is HW-playable we return false so
// the master/playlist paths fall through to the transcode ladder.
//
// Returns true when the manifest is unreadable so we don't 404 the
// player just because we couldn't introspect renditions.
func PackagedPlayableBy(itemID string, caps Caps) bool {
	mf, err := ReadPackageManifest(itemID)
	if err != nil || mf == nil {
		return true
	}
	if len(mf.Renditions.Video) == 0 {
		return true
	}
	for _, v := range mf.Renditions.Video {
		fam := codecFamily(v.Codec)
		codecOK := false
		switch fam {
		case "h264":
			codecOK = caps.Video["h264"] || caps.Video["avc1"]
		case "hevc":
			codecOK = caps.Video["hevc"] || caps.Video["hvc1"] || caps.Video["h265"]
		case "vp9":
			codecOK = caps.Video["vp9"]
		case "av1":
			codecOK = caps.Video["av1"]
		default:
			// Unknown codec string — assume playable rather than
			// black-screen the user on a parse miss. No height gate
			// applies because we have no family to look the cap up under.
			return true
		}
		if !codecOK {
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

// servePlaylistCached reads + rewrites + caches an m3u8 file, then
// serves it with an ETag and a short max-age so browser revisits
// short-circuit at the cache layer.
func servePlaylistCached(w http.ResponseWriter, r *http.Request, path string) {
	servePlaylistCachedTransform(w, r, path, nil)
}

// servePlaylistCachedTransform is servePlaylistCached with an optional
// post-rewrite transform applied to the playlist body before it's cached and
// served. Used to stamp VIDEO-RANGE onto the packaged master for HDR titles
// (shaka omits it). The transform runs once per (path, query) cache miss; cache
// hits serve the already-transformed body.
func servePlaylistCachedTransform(w http.ResponseWriter, r *http.Request, path string, transform func(string) string) {
	st, err := os.Stat(path)
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}
	// Cache key includes the query so different ?stream=…/?caps=…
	// rewrites stay distinct. Cap at the lifetime of the file mtime
	// — different mtime = different entry, so a repackage shows up.
	cacheKey := path + "?" + r.URL.RawQuery
	if v, ok := playlistCache.Load(cacheKey); ok {
		if e := v.(*playlistCacheEntry); e.mtime.Equal(st.ModTime()) {
			if match := r.Header.Get("If-None-Match"); match != "" && match == e.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "private, max-age=60")
			w.Header().Set("ETag", e.etag)
			_, _ = w.Write(e.body)
			return
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
		return
	}
	body := rewriteM3U8URIs(string(raw), r.URL.RawQuery)
	if transform != nil {
		body = transform(body)
	}
	out := []byte(body)
	sum := sha1.Sum(out)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	playlistCache.Store(cacheKey, &playlistCacheEntry{body: out, etag: etag, mtime: st.ModTime()})
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(out)
}

// PackagedMaster serves the shaka-generated master.m3u8 with every
// rendition URI rewritten to carry the inbound query string. Without
// the rewrite the player would resolve `v0/playlist.m3u8` against the
// master's URL and drop `?stream=…`, leaving subsequent rendition
// fetches unauthenticated.
func (h *HLSHandler) PackagedMaster(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	masterPath := packagePath(itemID, "hls", "master.m3u8")
	// Shaka doesn't emit VIDEO-RANGE, so a packaged HDR HEVC rendition is
	// mis-signalled as SDR and HDR displays never engage HDR mode. The package
	// manifest carries a per-rendition HDR flag from a LOCAL file (no OIDC
	// bearer needed — unlike re-resolving the source via katalog-api, which
	// this stream-token'd path can't do), so stamp VIDEO-RANGE onto the variant
	// lines when the package has any HDR rendition. Pure-SDR packages serve
	// verbatim (byte-identical to before this change).
	if mf, err := ReadPackageManifest(itemID); err == nil && manifestHasHDR(mf) {
		servePlaylistCachedTransform(w, r, masterPath, injectVideoRangeTransform(mf))
		return
	}
	servePlaylistCached(w, r, masterPath)
}

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
// (video or audio), again rewriting segment URIs to carry the query
// string. Path is /api/play/{itemId}/{rendId}/playlist.m3u8.
func (h *HLSHandler) PackagedRenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	servePlaylistCached(w, r, packagePath(itemID, "hls", rendID, "playlist.m3u8"))
}

// PackagedIframesPlaylist serves shaka's I-frame trick-play playlist.
// Same shape as the regular rendition playlist; the player loads it
// when the user scrubs.
func (h *HLSHandler) PackagedIframesPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	path := packagePath(itemID, "hls", rendID, "iframes.m3u8")
	body, err := os.ReadFile(path)
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
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	path := packagePath(itemID, "hls", rendID, "init.mp4")
	servePackagedStatic(w, r, path, "video/mp4", "init not found")
}

// PackagedSegment serves one CMAF media segment from the packages
// PVC. Path: /api/play/{itemId}/{rendId}/seg-{seg}.m4s. The {seg}
// value is matched as 5-digit zero-padded in the route so shaka's
// seg-00001.m4s naming flows through unchanged.
func (h *HLSHandler) PackagedSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	seg := chi.URLParam(r, "seg")
	path := packagePath(itemID, "hls", rendID, "seg-"+seg+".m4s")
	servePackagedStatic(w, r, path, "video/iso.segment", "segment not found")
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
	st, err := os.Stat(path)
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
	entry := &packagedCacheEntry{bytes: data, mtime: mtime}
	entry.lastTouch.Store(time.Now().UnixNano())
	if v, loaded := packagedCache.LoadOrStore(path, entry); loaded {
		entry = v.(*packagedCacheEntry)
	} else {
		newTotal := packagedCacheBytes.Add(int64(len(data)))
		if newTotal > packagedCacheBudgetBytes {
			go evictPackagedCache()
		}
	}
	entry.lastTouch.Store(time.Now().UnixNano())

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

// warmPackaged primes the caches for a packaged item OFF the request
// path so the player's follow-up master / playlist / init / segment
// fetches all land hot. It is fire-and-forget (called via `go`),
// panic-safe, and bounded (~2 segments per playable rendition).
//
// What it warms:
//
//	(a) master.m3u8 + each playable video+audio rendition playlist.m3u8
//	    via os.ReadFile — warms the NFS client + OS page cache so the
//	    real servePlaylistCached read is hot (the rewrite+cache step
//	    itself is cheap; the NFS read was the slow part).
//	(b) init.mp4 for those renditions into packagedCache, so the real
//	    PackagedInitSegment serves from memory (a 924-byte init.mp4 was
//	    seen stalling 18.4s cold under NFS contention).
//	(c) the media segment that contains tSec (+ the next one) for each
//	    rendition; or the first 2 segments when tSec<=0. Segment choice
//	    is driven by parsing the rendition playlist's #EXTINF durations
//	    and seg-*.m4s URIs.
//
// Renditions are filtered to those the client can actually play via
// ReadPackageManifest + PackagedPlayableBy(caps): no point warming an
// HEVC rendition for a device that will fall through to transcode.
func (h *HLSHandler) warmPackaged(itemID string, caps Caps, tSec float64) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("warmPackaged %s panic: %v", itemID, rec)
		}
	}()
	if !PackagedPlayableBy(itemID, caps) {
		return
	}
	mf, err := ReadPackageManifest(itemID)
	if err != nil || mf == nil {
		// No manifest to drive rendition selection — still warm the
		// master so at least the first fetch is hot.
		warmPackagedFile(packagePath(itemID, "hls", "master.m3u8"))
		return
	}

	// (a) master playlist.
	warmPackagedFile(packagePath(itemID, "hls", "master.m3u8"))

	// Build the rendition id list: every video rendition (manifest-level
	// PackagedPlayableBy already gated the whole item) + every audio
	// rendition. Audio is codec-agnostic here (always stereo AAC in
	// packaged mode), so all audio renditions are playable.
	rendIDs := make([]string, 0, len(mf.Renditions.Video)+len(mf.Renditions.Audio))
	for _, v := range mf.Renditions.Video {
		if v.ID != "" {
			rendIDs = append(rendIDs, v.ID)
		}
	}
	for _, a := range mf.Renditions.Audio {
		if a.ID != "" {
			rendIDs = append(rendIDs, a.ID)
		}
	}

	for _, rid := range rendIDs {
		playlistPath := packagePath(itemID, "hls", rid, "playlist.m3u8")
		// (a) rendition playlist — warm NFS+page cache.
		warmPackagedFile(playlistPath)
		// (b) init segment — warm into packagedCache.
		warmPackagedFile(packagePath(itemID, "hls", rid, "init.mp4"))
		// (c) segment(s) covering tSec (or the first two).
		segNames := pickWarmSegments(playlistPath, tSec)
		for _, seg := range segNames {
			warmPackagedFile(packagePath(itemID, "hls", rid, seg))
		}
	}
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
	entry := &packagedCacheEntry{bytes: data, mtime: mtime}
	entry.lastTouch.Store(time.Now().UnixNano())
	if v, loaded := packagedCache.LoadOrStore(path, entry); loaded {
		v.(*packagedCacheEntry).lastTouch.Store(time.Now().UnixNano())
		return v.(*packagedCacheEntry).bytes, nil
	}
	newTotal := packagedCacheBytes.Add(int64(len(data)))
	if newTotal > packagedCacheBudgetBytes {
		go evictPackagedCache()
	}
	return data, nil
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
	itemID := chi.URLParam(r, "itemId")
	path := packagePath(itemID, "trickplay", "thumbnails.vtt")
	servePackagedStatic(w, r, path, "text/vtt; charset=utf-8", "trickplay vtt not found")
}

// PackagedTrickplaySprite serves one sprite-sheet JPG. The VTT cues
// reference these by relative name (sprite-NNNN.jpg) so the player's
// resolved URL lands here.
func (h *HLSHandler) PackagedTrickplaySprite(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	n := chi.URLParam(r, "n")
	path := packagePath(itemID, "trickplay", "sprite-"+n+".jpg")
	servePackagedStatic(w, r, path, "image/jpeg", "trickplay sprite not found")
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

// manifestCache memoises parsed manifest.json blobs per itemId,
// invalidated by file mtime. The Master / Info / PackagedPlayableBy
// hot path used to ReadFile + Unmarshal on EVERY request — at
// 5-30 ms per call this dominated packaged.m3u8 latency for Zap
// (CDP probe 2026-06-02). Cached lookups are sub-microsecond.
type manifestCacheEntry struct {
	mf    *pkgmanifest.Manifest
	mtime time.Time
}

var manifestCache sync.Map // map[string]*manifestCacheEntry (itemId -> entry)

// ReadPackageManifest parses the manifest.json sitting next to the
// .complete sentinel. Returns nil + an error on read or parse failure
// — the Info handler then logs and falls through to the source-side
// probe so the player at least gets *some* info.
//
// The parsed manifest is cached in-process keyed on the file's mtime,
// so an operator who repackages an item picks up the new manifest on
// the next request without a pod restart.
func ReadPackageManifest(itemID string) (*pkgmanifest.Manifest, error) {
	root := itemRoot(itemID)
	if root == "" {
		return nil, os.ErrNotExist
	}
	mfPath := filepath.Join(root, "manifest.json")
	st, err := os.Stat(mfPath)
	if err != nil {
		return nil, err
	}
	if v, ok := manifestCache.Load(itemID); ok {
		if e := v.(*manifestCacheEntry); e.mtime.Equal(st.ModTime()) {
			return e.mf, nil
		}
	}
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		return nil, err
	}
	var m pkgmanifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	manifestCache.Store(itemID, &manifestCacheEntry{mf: &m, mtime: st.ModTime()})
	return &m, nil
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
// codec).
func writePackagedInfo(w http.ResponseWriter, mf *pkgmanifest.Manifest) {
	video := pkgmanifest.VideoRendition{}
	if len(mf.Renditions.Video) > 0 {
		video = mf.Renditions.Video[0]
	}
	audioTracks := make([]map[string]any, 0, len(mf.Renditions.Audio))
	for i, a := range mf.Renditions.Audio {
		audioTracks = append(audioTracks, map[string]any{
			"index":    i,
			"codec":    a.Codec, // always mp4a in packaged mode
			"language": a.Language,
			"title":    a.Title,
			"default":  a.Default,
			"channels": a.Channels, // always 2 in packaged mode
		})
	}
	// v2 manifests put the title at the top level; v1 manifests had
	// only Source.Path. Prefer Title when present; fall back to the
	// source-path basename for legacy packages.
	filename := mf.Title
	if filename == "" && mf.Source != nil {
		filename = filepath.Base(mf.Source.Path)
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
		"qualities":       nil,
		"default_quality": video.ID,
		"audio_tracks":    audioTracks,
		"subtitle_tracks": mf.Subtitles,
	})
}
