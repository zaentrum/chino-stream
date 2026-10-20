package play

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"

	"github.com/zaentrum/chino-stream/internal/metrics"
)

// The live worker (ADR-0013, "live rungs beside the package, as a supervised
// safety net", phase 1): every live encode leaves chino-stream for the
// live-worker service, a supervised pool of warm worker processes. Its
// contract is the worker's own api and client packages.
//
//   - A video rung's segment of an original or of a package, and a segment of
//     an original's audio, is one worker job, on the 6 s grid the on-the-fly
//     playlists have always had (live.go).
//   - A package's audio and the stream copy are remuxed in Go, with no encoder
//     (remuxaudio.go, remuxcopy.go).
//   - An original's embedded subtitle is a worker job; a package's subtitle
//     is shifted in Go (webvtt.go).
//   - The progressive transcode answers 404; its byte ranges of the original
//     stay. The header probe (ffprobe) stays.
//
// LIVE_WORKER_MODE turns it on, one instance at a time:
//
//   - off (the default): today's ffmpeg pipelines, byte for byte. A session
//     an earlier "on" sent to the worker goes on there (the lw pin below).
//   - shadow: ffmpeg serves as with off, and the worker is asked for the same
//     segments at prefetch priority; what comes back is checked and dropped
//     (liveshadow.go).
//   - on: new on-the-fly sessions go to the live pipeline.
//
// A session keeps its pipeline: the on-the-fly master of a session sent to
// the live pipeline writes lw=1 into its URIs, as it writes v= for a package
// version, so every request of the session says which pipeline it began on and
// a segment never meets the other pipeline's init, however the flag turns or
// whichever instance answers. Without LIVE_WORKER_URL there is no live
// pipeline at all, and lw means nothing.

// LiveWorkerMode is LIVE_WORKER_MODE.
type LiveWorkerMode string

// The modes.
const (
	LiveWorkerOff    LiveWorkerMode = "off"
	LiveWorkerShadow LiveWorkerMode = "shadow"
	LiveWorkerOn     LiveWorkerMode = "on"
)

// ParseLiveWorkerMode reads LIVE_WORKER_MODE: off (also empty), shadow or on.
// Anything else is an error, and off.
func ParseLiveWorkerMode(s string) (LiveWorkerMode, error) {
	switch m := LiveWorkerMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", LiveWorkerOff:
		return LiveWorkerOff, nil
	case LiveWorkerShadow, LiveWorkerOn:
		return m, nil
	}
	return LiveWorkerOff, fmt.Errorf("LIVE_WORKER_MODE %q: want off, shadow or on", s)
}

// The deadlines of the calls (phase-1.md, "How chino-stream calls it"): the
// worker's deadlineMs, which bounds its queueing, its retries on other worker
// processes and the job together. The HTTP call gives up liveGrace after it.
const (
	livePlayheadDeadline = 12 * time.Second  // a segment a player waits for: room for a retry after a crash
	liveInitDeadline     = 10 * time.Second  // a rung's init
	livePrefetchDeadline = 30 * time.Second  // a segment ahead of the play head
	liveSubtitleDeadline = 120 * time.Second // a subtitle stream, read through the whole file
	liveGrace            = 2 * time.Second
	// liveUnreachableRetry is how long after a refused or reset connection (the
	// service restarting) a call is made once more.
	liveUnreachableRetry = 200 * time.Millisecond
	// liveRetryRoom is the least time a call made once more must have left.
	liveRetryRoom = time.Second
	// liveReadyTTL is how long the worker's readiness is believed.
	liveReadyTTL = 5 * time.Second
)

// LiveWorker is chino-stream's side of the live worker. nil is no live
// worker: LIVE_WORKER_URL is not set, and nothing is ever sent to one.
type LiveWorker struct {
	mode   LiveWorkerMode
	client *client.Client

	// The deadlines, retry pauses and readiness TTL; fields so tests can
	// shorten them.
	playhead, init, prefetch, subtitle time.Duration
	grace, unreachableRetry, retryRoom time.Duration
	readyTTL                           time.Duration

	ready   liveReadiness
	flights liveFlights
	shadow  *shadowState
}

// NewLiveWorker is the live worker at base (http://host:port), called with
// the bearer token when it is not empty, in mode.
func NewLiveWorker(base, token string, mode LiveWorkerMode) *LiveWorker {
	return &LiveWorker{
		mode:             mode,
		client:           client.New(base, token, nil),
		playhead:         livePlayheadDeadline,
		init:             liveInitDeadline,
		prefetch:         livePrefetchDeadline,
		subtitle:         liveSubtitleDeadline,
		grace:            liveGrace,
		unreachableRetry: liveUnreachableRetry,
		retryRoom:        liveRetryRoom,
		readyTTL:         liveReadyTTL,
		shadow:           newShadowState(),
	}
}

// Mode is the mode; off for no live worker.
func (lw *LiveWorker) Mode() LiveWorkerMode {
	if lw == nil {
		return LiveWorkerOff
	}
	return lw.mode
}

// On says whether new sessions go to the live pipeline.
func (lw *LiveWorker) On() bool { return lw.Mode() == LiveWorkerOn }

// shadowing says whether ffmpeg's segments are checked against the worker's.
func (lw *LiveWorker) shadowing() bool { return lw.Mode() == LiveWorkerShadow }

// deadline is the deadline of a call of kind at priority.
func (lw *LiveWorker) deadline(kind, priority string) time.Duration {
	switch {
	case kind == api.KindSubtitle:
		return lw.subtitle
	case priority == api.PriorityPrefetch:
		return lw.prefetch
	case kind == api.KindInit:
		return lw.init
	}
	return lw.playhead
}

// errCodecMismatch is a fragment whose parameter sets are not those of the
// rung's init a player holds: it would not decode with it.
var errCodecMismatch = errors.New("live worker: the fragment's codec configuration is not the rung's init's")

// resultOf is the result label of a call's error.
func resultOf(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errCodecMismatch):
		return "codec"
	}
	if c := client.ClassOf(err); c != "" {
		return c
	}
	if ctx.Err() != nil {
		return "canceled"
	}
	return "unreachable"
}

// output asks the worker for a segment or an init (kind): req with its
// priority set, its deadline the kind's. A playhead call the worker had no
// slot for (503) is made once more after Retry-After when the deadline
// allows, and one whose connection was refused or reset once more after
// liveUnreachableRetry; nothing else is retried here (the supervisor has
// tried other worker processes already).
func (lw *LiveWorker) output(ctx context.Context, kind string, req api.Request) (*client.Output, error) {
	if req.Priority == "" {
		req.Priority = api.PriorityPlayhead
	}
	track := req.Track
	if track == "" {
		track = api.TrackVideo
	}
	var out *client.Output
	err := lw.retrying(ctx, kind, track, req.Priority, lw.deadline(kind, req.Priority), func(cctx context.Context, remaining time.Duration) error {
		req.DeadlineMs = max(1, int(remaining.Milliseconds()))
		var err error
		if kind == api.KindInit {
			out, err = lw.client.Init(cctx, req)
		} else {
			out, err = lw.client.Segment(cctx, req)
		}
		return err
	})
	return out, err
}

// subtitleVTT asks the worker for a text subtitle stream as WebVTT.
func (lw *LiveWorker) subtitleVTT(ctx context.Context, req api.SubtitleRequest) ([]byte, error) {
	var vtt []byte
	err := lw.retrying(ctx, api.KindSubtitle, "subtitle", api.PriorityPlayhead, lw.subtitle, func(cctx context.Context, remaining time.Duration) error {
		req.DeadlineMs = max(1, int(remaining.Milliseconds()))
		var err error
		vtt, err = lw.client.Subtitle(cctx, req)
		return err
	})
	return vtt, err
}

// retrying makes the call once, and once more as output says, within
// budget; each attempt is counted and timed.
func (lw *LiveWorker) retrying(ctx context.Context, kind, track, priority string, budget time.Duration,
	call func(ctx context.Context, remaining time.Duration) error) error {
	end := time.Now().Add(budget)
	busyRetried, unreachableRetried := false, false
	for {
		remaining := time.Until(end)
		cctx, cancel := context.WithDeadline(ctx, end.Add(lw.grace))
		metrics.LiveWorkerInflight.WithLabelValues(priority).Inc()
		t0 := time.Now()
		err := call(cctx, remaining)
		metrics.LiveWorkerInflight.WithLabelValues(priority).Dec()
		cancel()
		result := resultOf(ctx, err)
		metrics.LiveWorkerRequests.WithLabelValues(kind, track, priority, result).Inc()
		metrics.LiveWorkerDuration.WithLabelValues(kind, track, priority, result).Observe(time.Since(t0).Seconds())
		if err == nil || ctx.Err() != nil {
			return err
		}
		var pause time.Duration
		switch result {
		case "unreachable":
			if unreachableRetried || priority == api.PriorityPrefetch {
				return err
			}
			unreachableRetried, pause = true, lw.unreachableRetry
		case api.ClassBusy:
			if busyRetried || priority != api.PriorityPlayhead {
				return err
			}
			busyRetried, pause = true, retryAfter(err)
		default:
			return err
		}
		if time.Until(end)-pause < lw.retryRoom {
			return err
		}
		metrics.LiveWorkerRetries.WithLabelValues(kind, result).Inc()
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// retryAfter is when a busy worker says to call again (a second without a
// word of its own).
func retryAfter(err error) time.Duration {
	var e *client.Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return time.Second
}

// liveStatus is what a player is answered for a failed call (phase-1.md):
// a segment the source cannot make 404, as a window given up on was (players
// fall back at once on a 4xx instead of retrying); busy 503 with Retry-After;
// a deadline 504; anything else 502, as a failed ffmpeg run was.
func liveStatus(err error) (status int, retry time.Duration) {
	var e *client.Error
	switch {
	case errors.As(err, &e):
		switch e.Class() {
		case api.ClassSource, api.ClassCircuit:
			return http.StatusNotFound, 0
		case api.ClassBusy:
			return http.StatusServiceUnavailable, retryAfter(err)
		case api.ClassDeadline:
			return http.StatusGatewayTimeout, 0
		}
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, 0
	}
	return http.StatusBadGateway, 0
}

// liveError answers a request whose live encode failed (liveStatus).
func liveError(w http.ResponseWriter, err error, what string) {
	status, retry := liveStatus(err)
	switch status {
	case http.StatusNotFound:
		metrics.LiveWorkerFallbacks.WithLabelValues("refused").Inc()
		http.Error(w, what+" unavailable", status)
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int((retry+time.Second-1)/time.Second))))
		http.Error(w, what+" busy", status)
	case http.StatusGatewayTimeout:
		http.Error(w, what+" timed out", status)
	default:
		http.Error(w, what+" failed", status)
	}
}

// liveReadiness caches the worker's /readyz.
type liveReadiness struct {
	mu       sync.Mutex
	at       time.Time
	ok       bool
	checking chan struct{}
}

// Ready says whether the worker answers /readyz with 200, as it did within
// the last readyTTL: a new session is sent to it only then (else to ffmpeg,
// counted as a fallback). One check at a time; the others wait for it.
func (lw *LiveWorker) Ready(ctx context.Context) bool {
	r := &lw.ready
	r.mu.Lock()
	if !r.at.IsZero() && time.Since(r.at) < lw.readyTTL {
		ok := r.ok
		r.mu.Unlock()
		return ok
	}
	done := r.checking
	if done == nil {
		done = make(chan struct{})
		r.checking = done
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), lw.grace)
			defer cancel()
			status, err := lw.client.GetJSON(cctx, "/readyz", nil)
			ok := err == nil && status == http.StatusOK
			if !ok {
				log.Printf("live worker: not ready (status %d, %v)", status, err)
			}
			r.mu.Lock()
			r.at, r.ok, r.checking = time.Now(), ok, nil
			r.mu.Unlock()
			close(done)
		}()
	}
	r.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ok
}

// liveFlights are the live jobs under way, one per cache file: a request for a
// segment (or an init) joins the job making it, as the window productions
// joined ffmpeg's runs, and the supervisor joins identical jobs as well.
type liveFlights struct {
	mu sync.Mutex
	m  map[string]*liveFlight
}

// liveFlight is one job under way.
type liveFlight struct {
	done     chan struct{}
	err      error // what the job returned; read after done
	priority string
	// detached: a prefetch or a warm, which runs to its end; a request's ends
	// when every request waiting for it has gone.
	detached bool
	members  int
	cancel   context.CancelFunc
}

// join joins the job of key, starting run for it unless one is under way.
// A request (detached false) waits for it with wait.
func (fs *liveFlights) join(key, priority string, detached bool, run func(ctx context.Context) error) *liveFlight {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if f := fs.m[key]; f != nil {
		if !detached {
			f.members++
		}
		return f
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &liveFlight{done: make(chan struct{}), priority: priority, detached: detached, cancel: cancel}
	if !detached {
		f.members = 1
	}
	if fs.m == nil {
		fs.m = map[string]*liveFlight{}
	}
	fs.m[key] = f
	go func() {
		defer cancel()
		err := run(ctx)
		fs.mu.Lock()
		if fs.m[key] == f {
			delete(fs.m, key)
		}
		fs.mu.Unlock()
		f.err = err
		close(f.done)
	}()
	return f
}

// running says whether a job for key is under way.
func (fs *liveFlights) running(key string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.m[key] != nil
}

// wait waits for f's end or ctx's; a request that gives up leaves it, and a
// request's job nobody waits for any more ends.
func (fs *liveFlights) wait(ctx context.Context, f *liveFlight) error {
	select {
	case <-f.done:
		fs.leave(f)
		return f.err
	case <-ctx.Done():
		fs.leave(f)
		return ctx.Err()
	}
}

func (fs *liveFlights) leave(f *liveFlight) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f.members--
	if f.members <= 0 && !f.detached {
		select {
		case <-f.done:
		default:
			f.cancel()
		}
	}
}
