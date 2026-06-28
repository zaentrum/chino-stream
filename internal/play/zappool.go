package play

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Zap warm pool: speculative cache of pre-warmed packaged-content
// candidates the chino-web Zap pager will be steered to.
//
// Why this exists
//   First-frame latency on packaged Zap is dominated by the cold
//   master.m3u8 → variant playlist → init.mp4 → first segment chain,
//   each leg hitting the NFS-backed packages PVC for the first time.
//   Under contended NFS that's 200-2000 ms even with the in-process
//   packagedCache, and the user perceives it as a sluggish channel
//   flip. Picking items + seek points speculatively in advance and
//   priming packagedCache means the leaf fetch chain comes back
//   from RAM in tens of ms regardless of NFS state.
//
// How it works
//   A background worker (started from router.go alongside the
//   existing CacheSweeper) keeps `zapPoolSize` entries in memory.
//   Each entry has a random `seekSec` rolled by the same algorithm
//   useZapMidpoint.ts uses on the client. For each entry the worker
//   primes packagedCache with master.m3u8, every rendition's
//   playlist.m3u8 + init.mp4, plus the segment containing seekSec
//   (and the next one) for each rendition. The serve path
//   (PackagedMaster / PackagedInitSegment / PackagedSegment) is
//   unchanged — it just finds the file already in packagedCache and
//   skips the NFS read.
//
//   GET /api/play/zap-feed returns the head of the pool, bumps the
//   per-entry `playsServed` counter, and evicts entries that reach
//   `zapPoolPlaysPerEntry`. After every served request the worker is
//   pinged (non-blocking channel send) so the refill runs immediately
//   rather than waiting the next ticker fire.
//
//   The pool dies on pod restart. The client (useZapFeed) falls back
//   to today's cold pool-building when /zap-feed responds with an
//   empty slice, so a freshly-booted pod degrades gracefully while
//   the worker primes.

// zapPoolEntry is one pre-warmed Zap candidate. Immutable after the
// worker publishes the pointer into zapPool, except for PlaysServed
// which is mutated only inside the ZapFeed handler under zapPoolMu.
//
// Paths records the packagedCache keys this entry depends on. The
// ZapFeed handler validates them against packagedCache before
// serving — under sustained playback load (long-movie playback can
// stream multiple GB through the 512 MiB cache budget) the LRU
// eviction may have dropped these bytes, in which case advertising
// the entry as "warm" is a lie and we drop it from the pool so the
// worker re-warms a fresh candidate.
type zapPoolEntry struct {
	ItemID      string
	SeekSec     float64
	DurationMs  int64
	Title       string
	Type        string
	Year        int     // 0 when unknown
	Rating      float64 // 0 when unknown
	PosterURL   string
	BackdropURL string
	MidSource   string // always "percent" — fallback entries are skipped at warm time
	WarmedAt    time.Time
	PlaysServed int
	Paths       []string // packagedCache keys: master + per-rendition (playlist + init + warmed segments)
}

const (
	zapPoolSize          = 8
	zapPoolPlaysPerEntry = 2
	zapPoolRefillTick    = 5 * time.Second
	zapRecentlyWarmedTTL = 10 * time.Minute

	// Mirror useZapMidpoint.ts constants verbatim. If you change one
	// side you MUST change the other — telemetry plots midSource and
	// the perceived randomness should agree across server-picked
	// (warm) and client-picked (fallback) entries.
	zapMinSeekSec    = 60.0
	zapTailBufferSec = 90.0
	zapRandLow       = 0.10
	zapRandHigh      = 0.80
)

var (
	zapPoolMu   sync.Mutex
	zapPool     []*zapPoolEntry
	zapRecently = map[string]time.Time{}
	// Buffered to capacity 1 so multiple wake signals coalesce into
	// one refill pass — the handler fires fire-and-forget after each
	// served entry without blocking.
	zapPoolWake = make(chan struct{}, 1)
	// zapPoolRand is owned exclusively by the worker goroutine
	// (worker is single-flighted by zapPoolLoop). math/rand.Rand is
	// not goroutine-safe but we never share it.
	zapPoolRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// StartZapPoolWorker spawns the background refill loop. Call once
// from main / router wiring. The loop self-restarts on panic so a
// hostile manifest or one bad file can't take the pool down for the
// rest of the pod's lifetime.
func StartZapPoolWorker(ctx context.Context, h *HLSHandler) {
	go zapPoolLoop(ctx, h)
}

func zapPoolLoop(ctx context.Context, h *HLSHandler) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("zap-pool worker panic: %v; restarting in 5s", rec)
			time.Sleep(5 * time.Second)
			go zapPoolLoop(ctx, h)
		}
	}()
	t := time.NewTicker(zapPoolRefillTick)
	defer t.Stop()
	// Kick a refill immediately on boot so the pool starts warming
	// without waiting for the first ticker fire.
	refillZapPool(h)
	for {
		select {
		case <-ctx.Done():
			return
		case <-zapPoolWake:
		case <-t.C:
		}
		refillZapPool(h)
	}
}

// refillZapPool brings the pool back up to zapPoolSize, picking
// random packaged items whose ids are NOT already in the pool and
// NOT in the recently-warmed dedup set. The slow work (NFS reads,
// manifest parse, file warming) happens OUTSIDE zapPoolMu — only the
// snapshot + final append touch the mutex.
func refillZapPool(h *HLSHandler) {
	zapPoolMu.Lock()
	now := time.Now()
	// Purge stale recently-warmed entries opportunistically.
	for id, t := range zapRecently {
		if now.Sub(t) > zapRecentlyWarmedTTL {
			delete(zapRecently, id)
		}
	}
	// Track in-pool ids separately from recently-warmed: the former
	// MUST be excluded (we never duplicate a pool entry), the latter
	// is a freshness preference we can relax when the catalogue is
	// small.
	inPool := make(map[string]struct{}, len(zapPool))
	for _, e := range zapPool {
		inPool[e.ItemID] = struct{}{}
	}
	recent := make(map[string]struct{}, len(zapRecently))
	for id := range zapRecently {
		if _, ok := inPool[id]; !ok {
			recent[id] = struct{}{}
		}
	}
	need := zapPoolSize - len(zapPool)
	zapPoolMu.Unlock()

	if need <= 0 {
		return
	}

	all := ListCompletedPackageIDs()
	if len(all) == 0 {
		return
	}
	// Two-tier candidate set:
	//   - fresh: not in pool AND not recently warmed (the preferred bucket)
	//   - stale: not in pool but recently warmed (used only when fresh
	//            is too small to satisfy `need`)
	// Without this two-tier the pool starves on small catalogues:
	// once `recent` covers most of the package set, the worker's
	// candidate slice empties and the pool can't refill until
	// zapRecentlyWarmedTTL elapses for the oldest entries.
	fresh := make([]string, 0, len(all))
	stale := make([]string, 0)
	for _, id := range all {
		if _, takenPool := inPool[id]; takenPool {
			continue
		}
		if _, recentlyDone := recent[id]; recentlyDone {
			stale = append(stale, id)
			continue
		}
		fresh = append(fresh, id)
	}
	if len(fresh) == 0 && len(stale) == 0 {
		return
	}
	candidates := fresh
	if len(candidates) < need {
		// Shuffle the stale tier before grafting on so the relaxed
		// pick isn't deterministic.
		for i := len(stale) - 1; i > 0; i-- {
			j := zapPoolRand.Intn(i + 1)
			stale[i], stale[j] = stale[j], stale[i]
		}
		candidates = append(candidates, stale...)
	}
	// Fisher-Yates shuffle (worker-owned rand, no lock).
	for i := len(candidates) - 1; i > 0; i-- {
		j := zapPoolRand.Intn(i + 1)
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}

	added := 0
	for _, id := range candidates {
		if added >= need {
			return
		}
		entry := warmOneZapItem(h, id)
		if entry == nil {
			continue
		}
		// Re-check the slot under lock — a concurrent eviction may
		// have repopulated the pool while we were warming. Either
		// way record the warm in `recently` so we don't immediately
		// re-pick this id; the warmed bytes are already in
		// packagedCache (useful) even if the pool entry is dropped.
		zapPoolMu.Lock()
		zapRecently[id] = time.Now()
		if len(zapPool) >= zapPoolSize {
			zapPoolMu.Unlock()
			return
		}
		zapPool = append(zapPool, entry)
		zapPoolMu.Unlock()
		// Pin the entry's warmed bytes so playback can't LRU-evict them
		// out from under the pool (the reason /zap-feed was empty).
		PinPaths(entry.Paths)
		added++
	}
}

// warmOneZapItem reads the package manifest, rolls a random seekSec,
// primes packagedCache with master + per-rendition (playlist + init +
// segments covering seekSec), tracks which paths landed, and returns
// a ready entry. Returns nil when:
//   - the item has no manifest or zero duration
//   - the midpoint algo returns "fallback" (content too short for a
//     mid-stream seek; ZapCard's playUrl gate rejects fallback sources)
//   - the master, the first video rendition's playlist+init, or its
//     containing segment failed to land in packagedCache (corrupt or
//     half-packaged item; serving it would mean cold NFS reads which
//     defeats the entire point of the warm pool)
//
// Each successful warmPackagedFile path is recorded on entry.Paths so
// ZapFeed can validate the bytes haven't been LRU-evicted between
// warm and serve.
func warmOneZapItem(_ *HLSHandler, itemID string) *zapPoolEntry {
	mf, err := ReadPackageManifest(itemID)
	if err != nil || mf == nil {
		return nil
	}
	dur := mf.EffectiveDurationMs()
	if dur <= 0 {
		return nil
	}

	seekSec, midSource := pickZapMidpointServer(dur)
	// "fallback" means content is shorter than zapMinSeekSec +
	// zapTailBufferSec. ZapCard's playUrl gate refuses to render
	// these (returns '' so hls.js never attaches) so warming one
	// would publish a card that the client immediately can't play.
	// Skip server-side; the client cold-pool path may still pick it
	// up if the user really wants short content.
	if midSource != "percent" {
		return nil
	}

	paths := make([]string, 0, 12)

	// Master playlist — required.
	masterPath := packagePath(itemID, "hls", "master.m3u8")
	if !warmPackagedFile(masterPath) {
		return nil
	}
	paths = append(paths, masterPath)

	// Video rendition warmup — at least one must succeed fully
	// (playlist + init + ≥1 segment) or the entry is unplayable.
	videoOK := false
	for _, v := range mf.Renditions.Video {
		if v.ID == "" {
			continue
		}
		playlistPath := packagePath(itemID, "hls", v.ID, "playlist.m3u8")
		initPath := packagePath(itemID, "hls", v.ID, "init.mp4")
		if !warmPackagedFile(playlistPath) || !warmPackagedFile(initPath) {
			continue
		}
		segNames := pickWarmSegments(playlistPath, seekSec)
		warmedSegs := make([]string, 0, len(segNames))
		for _, seg := range segNames {
			p := packagePath(itemID, "hls", v.ID, seg)
			if warmPackagedFile(p) {
				warmedSegs = append(warmedSegs, p)
			}
		}
		if len(warmedSegs) == 0 {
			// Playlist + init landed but no segments — nothing to
			// decode at seekSec. Skip this rendition.
			continue
		}
		paths = append(paths, playlistPath, initPath)
		paths = append(paths, warmedSegs...)
		videoOK = true
	}
	if !videoOK {
		return nil
	}

	// Audio rendition warmup — best effort; audio-only renditions
	// missing don't kill the entry (video carries it), they just
	// degrade silently to whatever NFS serves.
	for _, a := range mf.Renditions.Audio {
		if a.ID == "" {
			continue
		}
		playlistPath := packagePath(itemID, "hls", a.ID, "playlist.m3u8")
		initPath := packagePath(itemID, "hls", a.ID, "init.mp4")
		if !warmPackagedFile(playlistPath) {
			continue
		}
		if !warmPackagedFile(initPath) {
			continue
		}
		paths = append(paths, playlistPath, initPath)
		for _, seg := range pickWarmSegments(playlistPath, seekSec) {
			p := packagePath(itemID, "hls", a.ID, seg)
			if warmPackagedFile(p) {
				paths = append(paths, p)
			}
		}
	}

	year := 0
	if mf.Year != nil {
		year = *mf.Year
	}
	return &zapPoolEntry{
		ItemID:     itemID,
		SeekSec:    seekSec,
		DurationMs: dur,
		Title:      mf.Title,
		Type:       mf.Type,
		Year:       year,
		MidSource:  midSource,
		WarmedAt:   time.Now(),
		Paths:      paths,
	}
	// Enrichment for poster/backdrop/rating: chino-api owns those
	// fields and they aren't accessible from chino-stream without
	// the requester's bearer. The client (useZapFeed) already has
	// them from its existing /items listings, so leaving empty is
	// acceptable — ZapCard's lazy-load of /items/{id} fills the
	// detail surface on focus.
}

// pickZapMidpointServer mirrors useZapMidpoint.ts pickZapMidpoint
// without the segment-aware branch (chino-stream has no analyzer
// intro/credits markers; chino-api owns those). The constants must
// stay in lockstep with the TS file — see the zapMinSeekSec /
// zapRandLow comments above.
//
// Always returns source="percent" for valid runtimes and "fallback"
// for content too short to safely pick a midpoint.
func pickZapMidpointServer(durationMs int64) (seekSec float64, source string) {
	durSec := float64(durationMs) / 1000.0
	if durSec <= zapMinSeekSec+zapTailBufferSec {
		s := zapMinSeekSec
		if half := math.Floor(durSec / 2); half < s {
			s = half
		}
		if s < 0 {
			s = 0
		}
		return s, "fallback"
	}
	lower := zapMinSeekSec
	upper := durSec - zapTailBufferSec
	rr := zapPoolRand.Float64()
	ratio := zapRandLow + rr*(zapRandHigh-zapRandLow)
	seek := math.Round(durSec * ratio)
	if seek < lower {
		seek = lower
	}
	if seek > upper {
		seek = upper
	}
	return seek, "percent"
}

// poolEntryStillResident returns true when EVERY path the entry
// depends on is still in packagedCache. A single missing path
// implies the LRU dropped at least one of the entry's bytes; the
// remaining serve would fall back to an NFS read on the hot path,
// which is exactly the latency the warm pool exists to prevent.
// Empty Paths (only possible from a defensively-constructed entry,
// shouldn't happen post-warmOneZapItem) is treated as not-resident
// so we don't serve a metadata-only entry that warms nothing.
func poolEntryStillResident(e *zapPoolEntry) bool {
	if len(e.Paths) == 0 {
		return false
	}
	for _, p := range e.Paths {
		if _, ok := packagedCache.Load(p); !ok {
			return false
		}
	}
	return true
}

// ZapFeed is the HTTP handler for GET /api/play/zap-feed.
// Returns up to `limit` pool entries (default 1, max zapPoolSize),
// bumps their PlaysServed counter, evicts ones that reach
// zapPoolPlaysPerEntry, and signals the worker to refill.
//
// The serve path holds zapPoolMu only for the duration of the slice
// mutation (sub-microsecond). The actual write to the response
// happens AFTER releasing the mutex so a slow client / large payload
// can't block the worker.
func (h *HLSHandler) ZapFeed(w http.ResponseWriter, r *http.Request) {
	limit := 1
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= zapPoolSize {
			limit = n
		}
	}

	zapPoolMu.Lock()
	// First sweep: drop entries whose warmed bytes have been
	// LRU-evicted from packagedCache. Sustained long-movie playback
	// can stream multiple GB through the 512 MiB cache and the warm
	// pool's bytes (untouched between warm and serve) are exactly
	// the oldest-touched — first to be evicted. Without this sweep
	// ZapFeed would advertise "warmed" entries whose first-frame
	// fetch is back on the cold NFS path, silently defeating the
	// feature.
	live := zapPool[:0]
	stale := 0
	for _, e := range zapPool {
		if poolEntryStillResident(e) {
			live = append(live, e)
		} else {
			// Evicted despite pinning (e.g. pin lost across a re-warm) —
			// release any lingering pins so the set can't leak.
			UnpinPaths(e.Paths)
			stale++
		}
	}
	zapPool = live
	// Bump playsServed for up to `limit` head entries.
	out := make([]*zapPoolEntry, 0, limit)
	for i := 0; i < len(zapPool) && i < limit; i++ {
		zapPool[i].PlaysServed++
		out = append(out, zapPool[i])
	}
	// Compact: drop entries that hit the play cap.
	kept := zapPool[:0]
	removed := stale
	for _, e := range zapPool {
		if e.PlaysServed >= zapPoolPlaysPerEntry {
			// Served its quota — release its pin so the bytes become
			// evictable again and the worker re-warms a fresh entry.
			UnpinPaths(e.Paths)
			removed++
			continue
		}
		kept = append(kept, e)
	}
	zapPool = kept
	warmed := len(zapPool)
	zapPoolMu.Unlock()

	if removed > 0 || warmed < zapPoolSize {
		// Non-blocking send — channel has capacity 1, signals coalesce.
		select {
		case zapPoolWake <- struct{}{}:
		default:
		}
	}

	writeZapFeedJSON(w, out, warmed)
}

// writeZapFeedJSON emits the zap-feed payload. Hand-rolled writer
// keeps the latency under a millisecond — `json.Encoder` over a
// small fixed shape is wasteful, but we still go through
// encoding/json for the title string to escape quotes / backslashes
// safely.
func writeZapFeedJSON(w http.ResponseWriter, entries []*zapPoolEntry, warmed int) {
	var sb strings.Builder
	sb.Grow(256 + 256*len(entries))
	sb.WriteString(`{"items":[`)
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":`)
		sb.Write(jsonString(e.ItemID))
		sb.WriteString(`,"title":`)
		sb.Write(jsonString(e.Title))
		sb.WriteString(`,"type":`)
		sb.Write(jsonString(e.Type))
		sb.WriteString(`,"year":`)
		sb.WriteString(strconv.Itoa(e.Year))
		sb.WriteString(`,"rating":`)
		sb.WriteString(strconv.FormatFloat(e.Rating, 'f', 1, 64))
		sb.WriteString(`,"duration_ms":`)
		sb.WriteString(strconv.FormatInt(e.DurationMs, 10))
		sb.WriteString(`,"poster_url":`)
		sb.Write(jsonString(e.PosterURL))
		sb.WriteString(`,"backdrop_url":`)
		sb.Write(jsonString(e.BackdropURL))
		sb.WriteString(`,"seek_sec":`)
		sb.WriteString(strconv.FormatFloat(e.SeekSec, 'f', 1, 64))
		sb.WriteString(`,"mid_source":`)
		sb.Write(jsonString(e.MidSource))
		sb.WriteByte('}')
	}
	sb.WriteString(`],"pool_size":`)
	sb.WriteString(strconv.Itoa(zapPoolSize))
	sb.WriteString(`,"warmed":`)
	sb.WriteString(strconv.Itoa(warmed))
	sb.WriteByte('}')

	w.Header().Set("Content-Type", "application/json")
	// Each request mutates pool state (playsServed) so any cache
	// layer must NOT replay it across users.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(sb.String()))
}

func jsonString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}
