package play

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// A window's segments are served as ffmpeg finishes them, not when the
// whole window is done.
//
// One ffmpeg run produces a window of ten 6 s segments (windowSize). It used
// to run inside the request that asked first, and its segments were
// installed when it exited: a player asking for a window's first segment
// waited for all sixty seconds of it to be encoded — 6.9 s for 1080p HEVC
// to H.264 at veryfast on a 10-core machine, at 1-2x realtime on a two-core
// pod 30-60 s — although ffmpeg had written that segment within the first
// second.
//
// A window is now produced apart from the requests for it (a production),
// one per window, and each segment is installed as ffmpeg finishes it (its
// hls muxer writes seg_N.m4s.tmp and renames it when the segment is
// complete; the init is complete once seg_0 is there). A request waits for
// its own segment, not for the window. The first segment of a window is
// served about a second after the request — whatever the window's length —
// and its later segments as they come, while the player plays the first.
//
// A production started by requests ends when nobody wants it: at once when
// every request waiting for it has gone before any segment was installed
// (the client went away, as a request killed its run before), else when no
// request has asked for a segment of the window for windowIdle (the player
// seeked elsewhere). A warm's production runs to its end, as a warm did.
// Its failures are produceWindow's, unchanged: the failure budget, a run
// coming up short retried on a clean slate, a cancelled run not charged.

const (
	// windowIdle is how long a production goes on without any request for
	// a segment of its window. A player asks for the next segment at least
	// every segment of playback while it buffers; one that has asked for
	// none in this long is playing elsewhere.
	windowIdle = 20 * time.Second
	// windowProductionTimeout bounds one production: a window encodes in
	// well under this, at half realtime too.
	windowProductionTimeout = 5 * time.Minute
	// installPoll is how often the segments a running ffmpeg has finished
	// are looked for.
	installPoll = 25 * time.Millisecond
)

// production is one window's production, shared by the requests for its
// segments.
type production struct {
	done chan struct{} // closed when produceWindow returned
	err  error         // what it returned; read after done

	cancel context.CancelFunc
	// scoped: ends when nobody wants it (a request's); a warm's does not.
	scoped bool
	// idle is how long it goes on unwanted (windowIdle).
	idle time.Duration

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced whenever a file is installed
	installs  int           // files installed so far
	waiters   int           // requests waiting for a segment of it
	last      time.Time     // when a request last asked for a segment of it
	cancelled bool          // ended for lack of interest
}

// productions are the windows being produced, by key.
type productions struct {
	mu sync.Mutex
	m  map[string]*production
	// idle is windowIdle unless set (tests shorten it).
	idle time.Duration
}

// installed notes a file of the window installed: the waiters look again.
func (p *production) installed() {
	p.mu.Lock()
	p.installs++
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// leave is a request no longer waiting for p: when it was the last, a
// request's production nothing of which is installed yet ends now, one that
// is under way after windowIdle unless a request asks again.
func (p *production) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters--
	p.last = time.Now()
	if p.waiters > 0 || !p.scoped {
		return
	}
	if p.installs == 0 {
		p.cancelled = true
		p.cancel()
		return
	}
	time.AfterFunc(p.idle, p.endIfIdle)
}

// endIfIdle ends a production nobody has asked anything of for its idle
// time, or looks again when that time will be up since the last request.
// While requests wait for it, the last of them to leave looks again.
func (p *production) endIfIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiters > 0 || p.cancelled {
		return
	}
	if since := time.Since(p.last); since < p.idle {
		time.AfterFunc(p.idle-since, p.endIfIdle)
		return
	}
	p.cancelled = true
	p.cancel()
}

// wait waits for ready (the segment a request asked for installed), p's end
// or ctx's. ended: p ended without the segment; err is then p's error, nil
// when it produced what it was started for or ended for lack of interest
// (the caller may start another for its segment).
func (p *production) wait(ctx context.Context, ready func() bool) (ended bool, err error) {
	defer p.leave()
	for {
		p.mu.Lock()
		changed := p.changed
		p.mu.Unlock()
		if ready() {
			return false, nil
		}
		select {
		case <-changed:
		case <-p.done:
			if ready() {
				return false, nil
			}
			p.mu.Lock()
			cancelled := p.cancelled
			p.mu.Unlock()
			if cancelled {
				return true, nil
			}
			return true, p.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// touch notes a request for a segment of the window key that was there
// already: its production, if one runs, is wanted.
func (ws *productions) touch(key string) {
	ws.mu.Lock()
	p := ws.m[key]
	ws.mu.Unlock()
	if p != nil {
		p.mu.Lock()
		p.last = time.Now()
		p.mu.Unlock()
	}
}

// awaitWindow answers a request for the segment seg of the window key (kind
// "vwin" or "awin") once ready says it is installed: it joins the window's
// production, starting one (produceWindow with transcode and invalidate)
// unless one runs, and waits. It returns the production's error when the
// production failed without the segment; when it ended without the segment
// otherwise (it produced another request's, or nobody wanted it) another is
// started for this one. A request already gone starts nothing.
func (h *HLSHandler) awaitWindow(ctx context.Context, kind, key string, seg int, ready func() bool,
	transcode func(ctx context.Context, installed func()) (string, error), invalidate func() error) error {
	id := kind + "/" + key
	for {
		if ready() {
			h.windows.touch(id)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p := h.joinWindow(ctx, id, kind, key, seg, ready, transcode, invalidate)
		ended, err := p.wait(ctx, ready)
		if !ended || err != nil {
			return err
		}
	}
}

// joinWindow is the production of the window id, started for the segment
// seg unless one runs, with the request ctx waiting for it. A warm's
// (withWarmContext) is a warm production: it uses the warm ffmpeg slots and
// runs to its end.
func (h *HLSHandler) joinWindow(ctx context.Context, id, kind, key string, seg int, ready func() bool,
	transcode func(ctx context.Context, installed func()) (string, error), invalidate func() error) *production {
	ws := &h.windows
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if p := ws.m[id]; p != nil {
		p.mu.Lock()
		p.waiters++
		p.last = time.Now()
		p.mu.Unlock()
		return p
	}
	base := context.Background()
	if isWarmContext(ctx) {
		base = withWarmContext(base)
	}
	pctx, cancel := context.WithTimeout(base, windowProductionTimeout)
	idle := ws.idle
	if idle <= 0 {
		idle = windowIdle
	}
	p := &production{done: make(chan struct{}), cancel: cancel, scoped: !isWarmContext(ctx), idle: idle,
		changed: make(chan struct{}), waiters: 1, last: time.Now()}
	if ws.m == nil {
		ws.m = map[string]*production{}
	}
	ws.m[id] = p
	go func() {
		defer cancel()
		err := h.produceWindow(pctx, kind, key, seg, ready,
			func() (string, error) { return transcode(pctx, p.installed) }, invalidate)
		ws.mu.Lock()
		if ws.m[id] == p {
			delete(ws.m, id)
		}
		ws.mu.Unlock()
		p.err = err
		close(p.done)
	}()
	return p
}

// windowInstaller installs the files a window's ffmpeg run writes into its
// temp folder — its init and its segments, in order — into the cache as
// they are complete (installWindow).
type windowInstaller struct {
	h         *HLSHandler
	tmpDir    string
	key       string // the cache key (source.key)
	quality   string // the cache namespace: the rung's, or audio-N
	windowIdx int
	installed func() // called for each file installed; may be nil

	next       int // the next segment of the window to install
	timescales map[uint32]uint32
}

// install installs what the run has finished: the init once the first
// segment is there (ffmpeg writes it before it renames that segment), then
// the segments from the next one on while they are there (renamed by ffmpeg
// when complete). Called with final once ffmpeg has exited cleanly: what is
// there then is complete — the init too, segments or not — and the window
// ends at the first segment missing.
func (wi *windowInstaller) install(final bool) error {
	cacheRoot := filepath.Dir(wi.h.cachePath(wi.key, wi.quality, "init"))
	if wi.timescales == nil {
		if !final && !statOK(filepath.Join(wi.tmpDir, "seg_0.m4s")) {
			return nil
		}
		if err := wi.installInit(cacheRoot, final); err != nil || wi.timescales == nil {
			return err
		}
	}
	for wi.next < windowSize {
		segSrc := filepath.Join(wi.tmpDir, fmt.Sprintf("seg_%d.m4s", wi.next))
		if !statOK(segSrc) {
			return nil // not finished yet; or, final, the end of the window
		}
		if bias := uint64(wi.windowIdx * windowSize * segmentSec); bias > 0 {
			if err := addTfdtBiasInPlace(segSrc, bias, wi.timescales); err != nil {
				return fmt.Errorf("patch tfdt seg %d: %w", wi.next, err)
			}
		}
		absSeg := wi.windowIdx*windowSize + wi.next
		if err := os.Rename(segSrc, filepath.Join(cacheRoot, strconv.Itoa(absSeg)+".bin")); err != nil {
			return fmt.Errorf("install seg %d: %w", absSeg, err)
		}
		wi.next++
		if wi.installed != nil {
			wi.installed()
		}
	}
	return nil
}

// installInit reads the window's init for its tracks' timescales (the
// segments' tfdt are patched by them) and installs it as the rung's
// init.bin, which the windows of a rung share: the first in installs it.
// Not final, an init that names no track yet is still being written: left
// for the next look.
func (wi *windowInstaller) installInit(cacheRoot string, final bool) error {
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return err
	}
	initSrc := filepath.Join(wi.tmpDir, "init.mp4")
	initBytes, err := os.ReadFile(initSrc)
	if err != nil {
		return fmt.Errorf("read window init: %w", err)
	}
	ts := parseInitTimescales(initBytes)
	if len(ts) == 0 && !final {
		return nil
	}
	if initDst := filepath.Join(cacheRoot, "init.bin"); !statOK(initDst) {
		if err := os.Rename(initSrc, initDst); err != nil {
			return fmt.Errorf("install init: %w", err)
		}
		if wi.installed != nil {
			wi.installed()
		}
	}
	if ts == nil {
		ts = map[uint32]uint32{}
	}
	wi.timescales = ts
	return nil
}

// runInstalling runs ffmpeg with args and installs the window's files as
// they are finished (wi), then, once it exited cleanly, the rest. It returns
// the tail of ffmpeg's stderr and the first error.
func (h *HLSHandler) runInstalling(ctx context.Context, args []string, label string, wi *windowInstaller) (string, error) {
	stop, stopped := make(chan struct{}), make(chan error, 1)
	go func() {
		t := time.NewTicker(installPoll)
		defer t.Stop()
		for {
			select {
			case <-stop:
				stopped <- nil
				return
			case <-t.C:
				if err := wi.install(false); err != nil {
					stopped <- err
					return
				}
			}
		}
	}()
	tail, err := h.runFFmpeg(ctx, args, label)
	close(stop)
	installErr := <-stopped
	if err != nil {
		return tail, err
	}
	if installErr != nil {
		return tail, installErr
	}
	return tail, wi.install(true)
}
