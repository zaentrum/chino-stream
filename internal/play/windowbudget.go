package play

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// maxWindowAttempts is how many failed ffmpeg runs a video/audio window
// gets within windowRetryCooldown. One retry covers the NVENC/hwaccel-decode
// silent-truncation case seen in the wild for 4K HEVC sources (ffmpeg exits 0
// with part of the window missing). A second failure in a row is most likely
// deterministic (corrupt source, an encoder rejecting the GOP, an encoder the
// build lacks), so the window is given up on: requests answer 404 at once and
// the client falls back to a lower quality rung (the auto-fallback machinery
// in chino-androidtv / chino-web handles it gracefully).
const maxWindowAttempts = 2

// windowRetryCooldown is how long a window stays given up on after its last
// failed run. Then it gets a fresh budget: a transient cause (NFS hiccup, a
// fixed image, a source replaced on disk) heals by itself, while a window that
// keeps failing costs at most maxWindowAttempts runs per cooldown instead of
// one per player retry.
const windowRetryCooldown = 2 * time.Minute

// errWindowUnavailable is returned for a window that is given up on.
var errWindowUnavailable = errors.New("window unavailable")

// windowFailures counts the failed ffmpeg runs per window, keyed
// `{itemID}/{quality}/{windowIdx}` — the segmentLocks key shape, so the two
// are easy to correlate in logs. A window that produces its segment is
// forgotten; a record whose last failure is windowRetryCooldown old is void.
//
// The count used to live as long as the process: two failed runs — or two
// requests the client abandoned mid-run, or an image whose ffmpeg lacked the
// encoder — kept a window dead ("cap reached … seg 0 unavailable") until the
// pod restarted.
type windowFailures struct {
	mu  sync.Mutex
	m   map[string]windowFailure
	now func() time.Time // nil: time.Now (tests stub it)
}

type windowFailure struct {
	count int
	last  time.Time
	cause string // the last failure, for the give-up error
}

func (f *windowFailures) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// check returns key's record (zero if none) and how long the window stays
// given up on (0 = it may run now). A record past its cooldown no longer
// blocks — the next failure starts a fresh count (fail), and prune drops it
// — but is still returned, so the caller wipes what the failed runs left.
func (f *windowFailures) check(key string) (windowFailure, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.m[key]
	if rec.count < maxWindowAttempts {
		return rec, 0
	}
	return rec, max(0, windowRetryCooldown-f.clock().Sub(rec.last))
}

// fail records a failed run of key and returns how many it has had.
func (f *windowFailures) fail(key, cause string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock()
	rec := f.m[key]
	if now.Sub(rec.last) >= windowRetryCooldown {
		rec = windowFailure{}
	}
	rec.count++
	rec.last = now
	rec.cause = cause
	if f.m == nil {
		f.m = map[string]windowFailure{}
	}
	f.m[key] = rec
	return rec.count
}

// clear forgets key: its window produced the segment asked for.
func (f *windowFailures) clear(key string) {
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
}

// prune drops void records, so windows nobody asks for again do not pile up.
func (f *windowFailures) prune() {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock()
	for key, rec := range f.m {
		if now.Sub(rec.last) >= windowRetryCooldown {
			delete(f.m, key)
		}
	}
}

// produceWindow runs transcode (one ffmpeg run of a whole window) until done
// reports the asked-for segment on disk, within the window's failure budget.
// kind ("vwin"/"awin") and key name the window in logs; seg is the segment
// asked for. It runs as the window's production (window.go), one at a time
// per window; ctx is the production's, done when nobody wants the window.
//
//   - A run that fails while ctx is done (every client waiting for the
//     window went away, a warm hit its timeout, the slot wait was abandoned)
//     is not charged: it says nothing about whether the window can be
//     produced.
//   - Any other failed run is charged and returned; the client may retry.
//   - A run that exits 0 without the segment (the silent truncation) is
//     charged and retried at once, after invalidate wipes the partial output.
//   - A window out of budget returns errWindowUnavailable, naming the last
//     cause, without running ffmpeg.
func (h *HLSHandler) produceWindow(ctx context.Context, kind, key string, seg int, done func() bool, transcode func() (stderrTail string, err error), invalidate func() error) error {
	for {
		rec, wait := h.windowFails.check(key)
		if wait > 0 {
			return fmt.Errorf("%w: %s %s gave up on seg %d after %d failed runs, next try in %s; last: %s",
				errWindowUnavailable, kind, key, seg, rec.count, wait.Round(time.Second), rec.cause)
		}
		if rec.count > 0 {
			// A run of this window failed or came up short before: wipe
			// its partial install so this run writes into a clean slate.
			// init.bin stays (it is shared across windows).
			if err := invalidate(); err != nil {
				return err
			}
		}
		tail, err := transcode()
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			h.windowFails.fail(key, err.Error())
			return err
		}
		if done() {
			h.windowFails.clear(key)
			return nil
		}
		cause := fmt.Sprintf("ffmpeg exited 0 without seg %d", seg)
		if tail != "" {
			cause += "; stderr: " + tail
		}
		n := h.windowFails.fail(key, cause)
		log.Printf("hls %s %s: partial transcode (%s), run %d of %d", kind, key, cause, n, maxWindowAttempts)
	}
}

// windowError answers a request whose window could not be produced: 404 once
// the window is given up on, so the player falls back to a lower rung at
// once (players retry a 5xx, not a 4xx); 502 for a failed run it may retry.
func windowError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, errWindowUnavailable) {
		http.Error(w, what+" unavailable", http.StatusNotFound)
		return
	}
	http.Error(w, what+" failed", http.StatusBadGateway)
}
