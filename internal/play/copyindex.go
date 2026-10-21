package play

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/streamcopy"
)

// The stream copy's indexes (streamcopy): an original's keyframes and its
// tracks' headers, made once per file — keyed by its path, size and mtime —
// and kept in memory and, for a Matroska original, in a folder of the cache
// (copy-index), which the cache's sweep leaves alone: across restarts for as
// long as the cache's volume lasts. A Matroska index is the file's Cues, read
// in milliseconds; an MP4's its sample tables, read anew after a restart (too
// large to keep). A file whose container has no usable index is read through
// in the background, one file at a time at a bounded rate, while its first
// sessions play the encode path. A file the copy fails on (a damaged index,
// a stream feature the copy does not take) is remembered so: its sessions
// take the encode path until the file changes.

// copyIndexDir is the cache folder of the persisted indexes.
const copyIndexDir = "copy-index"

// copyIndexMemory is how many indexes stay in memory: a Matroska one is
// tens of kilobytes, an MP4's sample tables a few megabytes (fewer of them).
const (
	copyIndexMemory    = 256
	copyIndexMemoryMP4 = 4
)

// copyIndexEntries bounds the entries kept in memory in all, failures and
// pending ones with them.
const copyIndexEntries = 4096

// copyIndexKeep is how long a persisted index unused is kept.
const copyIndexKeep = 30 * 24 * time.Hour

// copyIndexRetry is how soon an index that failed for a passing reason (the
// file could not be read) is tried again (a variable for the tests).
var copyIndexRetry = time.Minute

// copyScanRate is the read rate (bytes/s) of the background scan of a file
// without an index: well below what playback reads.
var copyScanRate int64 = 32 << 20

// The states of an index.
const (
	indexReady   = iota // indexed: segments can be made
	indexPending        // being read through in the background
	indexFailed         // not copied: why says why
)

// streamIndexEntry is the index of one file, or why there is none.
type streamIndexEntry struct {
	size  int64
	mtime time.Time
	ix    *streamcopy.Index
	state int
	why   string
	// until: a passing failure, tried again after it.
	until time.Time
	used  time.Time
	mp4   bool
}

// streamIndexStore holds the indexes.
type streamIndexStore struct {
	mu sync.Mutex
	m  map[string]*streamIndexEntry
	// builds are the indexes being made, by path: one build per file, which
	// every request for it waits on (as long as it waits).
	builds map[string]*indexBuild

	queue   chan copyScanJob
	queued  map[string]bool
	started bool
	jobs    sync.WaitGroup // the background builds (tests wait for them)
}

// indexBuild is an index being made; e is what it made, once done is closed.
type indexBuild struct {
	done chan struct{}
	e    *streamIndexEntry
}

// copyScanJob is a background build of an index.
type copyScanJob struct {
	path  string
	size  int64
	mtime time.Time
	probe Probe
	dir   string
	// passPlan: an original of another container, whose muxed stream copy
	// needs its keyframes probed (buildPassPlan).
	passPlan func()
}

// persistedIndex is an index file: the index, or why the file is not
// copied.
type persistedIndex struct {
	Version int               `json:"v"`
	Path    string            `json:"path"`
	Size    int64             `json:"size"`
	ModTime int64             `json:"mtime"`
	Failed  string            `json:"failed,omitempty"`
	Index   *streamcopy.Index `json:"index,omitempty"`
}

// copyFingerprint names a file's copy outputs in the cache: another file at
// the same path, or the same file changed, is another fingerprint.
func copyFingerprint(path string, size int64, mtime time.Time) string {
	h := sha1.Sum([]byte(path + "|" + strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(mtime.UnixNano(), 10) + "|" +
		strconv.Itoa(streamcopy.IndexVersion)))
	return hex.EncodeToString(h[:6])
}

func (h *HLSHandler) indexDir() string {
	if h.CacheDir == "" {
		return ""
	}
	return filepath.Join(h.CacheDir, copyIndexDir)
}

func indexFile(dir, path string) string {
	sum := sha1.Sum([]byte(path))
	return filepath.Join(dir, hex.EncodeToString(sum[:10])+".json")
}

// streamIndex is the index of the original of src as the copy uses it, or why
// there is none (indexPending: being made in the background; indexFailed:
// not copied). It reads a Matroska file's Cues, or an MP4's sample tables,
// the first time; never the whole file.
func (h *HLSHandler) streamIndex(ctx context.Context, src *source) (*streamcopy.Index, int, string) {
	if src.fromPackage() || src.file == "" || src.probe == nil {
		return nil, indexFailed, "no original"
	}
	st, err := os.Stat(src.file)
	if err != nil {
		return nil, indexFailed, err.Error()
	}
	s := &h.streamIndexes
	for {
		if e := s.get(src.file, st); e != nil {
			return e.ix, e.state, e.why
		}
		// One build per file: the first request makes it, the others (/info
		// and the master of a session, its prewarm, a second viewer) wait for
		// it, each as long as it is there.
		s.mu.Lock()
		if s.builds == nil {
			s.builds = map[string]*indexBuild{}
		}
		b := s.builds[src.file]
		if b == nil {
			b = &indexBuild{done: make(chan struct{})}
			s.builds[src.file] = b
			s.mu.Unlock()
			h.buildStreamIndex(src, st, b)
			return b.e.ix, b.e.state, b.e.why
		}
		s.mu.Unlock()
		select {
		case <-b.done:
			if e := b.e; e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
				return e.ix, e.state, e.why
			}
			// A build of the file before it changed: look again.
		case <-ctx.Done():
			return nil, indexPending, "the request ended while the file was being indexed"
		}
	}
}

// buildStreamIndex makes the entry of the original of src as the build b,
// which the requests waiting for it get whatever happens.
func (h *HLSHandler) buildStreamIndex(src *source, st os.FileInfo, b *indexBuild) {
	s := &h.streamIndexes
	defer func() {
		if r := recover(); r != nil {
			b.e = &streamIndexEntry{size: st.Size(), mtime: st.ModTime(), state: indexFailed,
				why: recoveredCopy("the index of "+filepath.Base(src.file), r).Error(), until: time.Now().Add(copyIndexRetry)}
		}
		s.mu.Lock()
		delete(s.builds, src.file)
		s.mu.Unlock()
		close(b.done)
	}()
	b.e = h.loadStreamIndex(src, st)
	s.put(src.file, b.e)
}

// get is the entry of path when it is of the file as st describes it and
// still holds.
func (s *streamIndexStore) get(path string, st os.FileInfo) *streamIndexEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[path]
	if e == nil || e.size != st.Size() || !e.mtime.Equal(st.ModTime()) {
		return nil
	}
	if e.state != indexReady && !e.until.IsZero() && time.Now().After(e.until) {
		delete(s.m, path) // a passing failure, or a scan never queued: try again
		return nil
	}
	e.used = time.Now()
	return e
}

// dropPending forgets the pending entry of path (a background build that
// ended without one), so that the next session asks again.
func (s *streamIndexStore) dropPending(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[path]; e != nil && e.state == indexPending {
		delete(s.m, path)
	}
}

// put keeps e for path, the least used dropped beyond the memory's room.
func (s *streamIndexStore) put(path string, e *streamIndexEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(path, e)
}

func (s *streamIndexStore) putLocked(path string, e *streamIndexEntry) {
	if s.m == nil {
		s.m = map[string]*streamIndexEntry{}
	}
	e.used = time.Now()
	s.m[path] = e
	for _, mp4 := range []bool{false, true} {
		limit := copyIndexMemory
		if mp4 {
			limit = copyIndexMemoryMP4
		}
		var paths []string
		for p, x := range s.m {
			if x.mp4 == mp4 && x.ix != nil {
				paths = append(paths, p)
			}
		}
		if len(paths) <= limit {
			continue
		}
		sort.Slice(paths, func(i, j int) bool { return s.m[paths[i]].used.Before(s.m[paths[j]].used) })
		for _, p := range paths[:len(paths)-limit] {
			delete(s.m, p)
		}
	}
	if len(s.m) > copyIndexEntries {
		var paths []string
		for p := range s.m {
			paths = append(paths, p)
		}
		sort.Slice(paths, func(i, j int) bool { return s.m[paths[i]].used.Before(s.m[paths[j]].used) })
		for _, p := range paths[:len(paths)-copyIndexEntries] {
			delete(s.m, p)
		}
	}
}

// loadStreamIndex makes the entry of the original of src: the persisted index
// (or failure) of a Matroska file, else one made now.
func (h *HLSHandler) loadStreamIndex(src *source, st os.FileInfo) *streamIndexEntry {
	e := &streamIndexEntry{size: st.Size(), mtime: st.ModTime()}
	probe := src.probe
	switch {
	case isMatroska(probe.Container):
	case isoBMFF(probe.Container):
		e.mp4 = true
	default:
		e.state, e.why = indexFailed, "a "+probe.Container+" original is not stream-copied"
		return e
	}
	dir := h.indexDir()
	if !e.mp4 && dir != "" {
		// A persisted index of another track list than the probe's (a file
		// remuxed in place keeping its mtime) is made anew.
		if p, ok := readPersistedIndex(dir, src.file, st); ok && (p.Failed != "" || len(p.Index.Audio) == len(probe.AudioTracks)) {
			metrics.CopyIndexes.WithLabelValues("persisted").Inc()
			if p.Failed != "" {
				e.state, e.why = indexFailed, p.Failed
			} else {
				e.ix = p.Index
			}
			return e
		}
	}
	t0 := time.Now()
	ix, err := safeIndexOriginal(src.file, st, probe)
	how := "moov"
	if !e.mp4 {
		how = "cues"
	}
	metrics.CopyIndexDuration.WithLabelValues(how).Observe(time.Since(t0).Seconds())
	switch {
	case err == nil:
		metrics.CopyIndexes.WithLabelValues(how).Inc()
		e.ix = ix
		if !e.mp4 {
			writePersistedIndex(dir, src.file, st, ix, "")
		}
	case errors.Is(err, streamcopy.ErrNoIndex) && ix != nil && !e.mp4:
		// No usable Cues: read the file through, apart from any request — or,
		// with the queue full, ask again after a while.
		metrics.CopyIndexes.WithLabelValues("pending").Inc()
		e.state, e.why = indexPending, err.Error()
		if !h.streamIndexes.enqueue(copyScanJob{path: src.file, size: st.Size(), mtime: st.ModTime(), probe: *probe, dir: dir}, h) {
			e.until = time.Now().Add(copyIndexRetry)
		}
	case permanentCopyError(err):
		metrics.CopyIndexes.WithLabelValues("failed").Inc()
		e.state, e.why = indexFailed, err.Error()
		if !e.mp4 {
			writePersistedIndex(dir, src.file, st, nil, e.why)
		}
		log.Printf("stream copy of %s: %v — it plays the encode path", filepath.Base(src.file), err)
	default:
		metrics.CopyIndexes.WithLabelValues("error").Inc()
		e.state, e.why, e.until = indexFailed, err.Error(), time.Now().Add(copyIndexRetry)
		log.Printf("stream copy index of %s: %v — the encode path for now", filepath.Base(src.file), err)
	}
	return e
}

// transientCopyError says whether err is a passing one: the storage failing
// a read (an *fs.PathError, an errno), or the request gone. Every other error
// of the copy is the file's — its format, a stream the copy does not take, a
// segment it cannot make — and comes again the same way.
func transientCopyError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pathErr *fs.PathError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

// permanentCopyError says whether err is about the file itself, not about
// reading it: the file is then not copied until it changes.
func permanentCopyError(err error) bool {
	return err != nil && !transientCopyError(err) && !errors.Is(err, errOriginalChanged)
}

// errCopyMismatch is an index whose tracks are not the probe's.
var errCopyMismatch = errors.New("the file's tracks are not the ones its probe lists")

// errOriginalChanged is a copy segment of a session whose file changed since
// it was indexed: neither the file's failure nor the storage's.
var errOriginalChanged = errors.New("the original changed since it was indexed")

// errCopyPanic is a file the stream copy panicked on: the file's, so that it
// is not tried again until it changes.
var errCopyPanic = errors.New("the stream copy failed on this file")

// recoveredCopy is the error of a panic of the copy's (r) on what: logged
// with its stack and counted.
func recoveredCopy(what string, r any) error {
	log.Printf("stream copy panicked on %s: %v\n%s", what, r, debug.Stack())
	metrics.CopyFallbacks.WithLabelValues("panic").Inc()
	return fmt.Errorf("%w: %v", errCopyPanic, r)
}

// safeIndexOriginal is indexOriginal, a panic of it an error of the file's.
func safeIndexOriginal(path string, st os.FileInfo, probe *Probe) (ix *streamcopy.Index, err error) {
	defer func() {
		if r := recover(); r != nil {
			ix, err = nil, recoveredCopy("the index of "+filepath.Base(path), r)
		}
	}()
	return indexOriginal(path, st, probe)
}

// indexOriginal indexes the original at path (st) whose probe is probe:
// streamcopy's Matroska or MP4 index, checked against the probe. A Matroska
// file without usable Cues comes back with its index and ErrNoIndex.
func indexOriginal(path string, st os.FileInfo, probe *Probe) (*streamcopy.Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	o := streamcopy.Options{OriginNs: probe.StartTimeNs, DurationNs: probe.DurationMs * 1e6, Path: path, ModTime: st.ModTime().UnixNano()}
	var ix *streamcopy.Index
	if isMatroska(probe.Container) {
		ix, err = streamcopy.IndexMatroska(f, st.Size(), o)
	} else {
		ix, err = streamcopy.IndexMP4(f, st.Size(), o)
	}
	if err != nil {
		return nil, err
	}
	if len(ix.Audio) != len(probe.AudioTracks) {
		return nil, fmt.Errorf("%w: %d audio tracks, the probe lists %d", errCopyMismatch, len(ix.Audio), len(probe.AudioTracks))
	}
	if err := ix.Check(); err != nil {
		return ix, err
	}
	return ix, nil
}

// isMatroska says whether ffprobe's format name is Matroska or WebM.
func isMatroska(format string) bool {
	return format == "matroska,webm" || format == "matroska" || format == "webm"
}

// readPersistedIndex reads the persisted index of path, when it is of the
// file st describes and of this version.
func readPersistedIndex(dir, path string, st os.FileInfo) (*persistedIndex, bool) {
	file := indexFile(dir, path)
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	var p persistedIndex
	if json.Unmarshal(b, &p) != nil || p.Version != streamcopy.IndexVersion || p.Path != path || p.Size != st.Size() ||
		p.ModTime != st.ModTime().UnixNano() || (p.Failed == "" && (p.Index == nil || p.Index.Check() != nil)) {
		return nil, false
	}
	// Used: kept another copyIndexKeep (at most one touch a day).
	if fi, err := os.Stat(file); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
		now := time.Now()
		_ = os.Chtimes(file, now, now)
	}
	return &p, true
}

// writePersistedIndex persists the index (or the failure why) of path.
func writePersistedIndex(dir, path string, st os.FileInfo, ix *streamcopy.Index, why string) {
	if dir == "" {
		return
	}
	b, err := json.Marshal(persistedIndex{Version: streamcopy.IndexVersion, Path: path, Size: st.Size(), ModTime: st.ModTime().UnixNano(),
		Failed: why, Index: ix})
	if err == nil {
		err = writeFileAtomic(indexFile(dir, path), b)
	}
	if err != nil {
		log.Printf("stream copy index of %s not kept: %v", filepath.Base(path), err)
	}
}

// persistedPlan is the muxed stream copy's plan of an original mp4ff does
// not read (MPEG-TS), kept with the persisted indexes: its keyframes are
// probed by reading the file through, and the cache's sweep would drop the
// plan of an item with the rest of its folder.
type persistedPlan struct {
	Version  int       `json:"v"`
	Pipeline string    `json:"pipeline"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  int64     `json:"mtime"`
	Failed   string    `json:"failed,omitempty"`
	Plan     *passPlan `json:"plan,omitempty"`
}

func planFile(dir, path string) string { return indexFile(dir, "plan|"+path) }

// readPersistedPlan reads the persisted plan of path, when it is of the file
// st describes and of this version.
func readPersistedPlan(dir, path string, st os.FileInfo) (*persistedPlan, bool) {
	file := planFile(dir, path)
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	var p persistedPlan
	if json.Unmarshal(b, &p) != nil || p.Version != streamcopy.IndexVersion || p.Pipeline != copyPipelineVersion || p.Path != path ||
		p.Size != st.Size() || p.ModTime != st.ModTime().UnixNano() || (p.Failed == "" && (p.Plan == nil || len(p.Plan.Segments) == 0)) {
		return nil, false
	}
	if fi, err := os.Stat(file); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
		now := time.Now()
		_ = os.Chtimes(file, now, now)
	}
	return &p, true
}

// writePersistedPlan persists the plan (or the failure why) of path.
func writePersistedPlan(dir, path string, st os.FileInfo, plan *passPlan, why string) {
	if dir == "" {
		return
	}
	b, err := json.Marshal(persistedPlan{Version: streamcopy.IndexVersion, Pipeline: copyPipelineVersion, Path: path, Size: st.Size(),
		ModTime: st.ModTime().UnixNano(), Failed: why, Plan: plan})
	if err == nil {
		err = writeFileAtomic(planFile(dir, path), b)
	}
	if err != nil {
		log.Printf("stream copy plan of %s not kept: %v", filepath.Base(path), err)
	}
}

// copyFailed notes that the copy of the original of src failed (a segment
// could not be made): new sessions of it take the encode path, until the
// file changes when the failure is about the file, else for a while.
func (h *HLSHandler) copyFailed(src *source, reason string, err error) {
	st, serr := os.Stat(src.file)
	if serr != nil {
		return
	}
	e := &streamIndexEntry{size: st.Size(), mtime: st.ModTime(), state: indexFailed, why: err.Error(), mp4: !isMatroska(src.probe.Container)}
	if !permanentCopyError(err) {
		e.until = time.Now().Add(copyIndexRetry)
	}
	// Once per failure of the file: a request and the prefetch of the same
	// segment fail together.
	if !h.streamIndexes.putFailure(src.file, e) {
		return
	}
	if e.until.IsZero() && !e.mp4 {
		writePersistedIndex(h.indexDir(), src.file, st, nil, e.why)
	}
	metrics.CopyFallbacks.WithLabelValues(reason).Inc()
	log.Printf("stream copy of %s failed (%s): %v — new sessions play the encode path", src.label(), reason, err)
}

// putFailure keeps the failure e of path, unless the file has failed so
// already; false then.
func (s *streamIndexStore) putFailure(path string, e *streamIndexEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.m[path]
	if old != nil && old.size == e.size && old.mtime.Equal(e.mtime) {
		if old.state == indexFailed && (old.until.IsZero() || time.Now().Before(old.until)) {
			return false
		}
		// The sessions begun on the index go on with it (copyRequest).
		e.ix = old.ix
	}
	s.putLocked(path, e)
	return true
}

// enqueue queues a background build, one per file; the builder runs one at a
// time. false: the queue is full, the file not queued.
func (s *streamIndexStore) enqueue(j copyScanJob, h *HLSHandler) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued == nil {
		s.queued = map[string]bool{}
	}
	if s.queued[j.path] {
		return true
	}
	if !s.started {
		s.queue = make(chan copyScanJob, 256)
		s.started = true
		go s.run(h)
	}
	select {
	case s.queue <- j:
		s.queued[j.path] = true
		s.jobs.Add(1)
		metrics.CopyScanQueue.Inc()
		return true
	default:
		return false // asked again by a session after copyIndexRetry
	}
}

// beforeBuild, when set (tests), runs before each background build.
var beforeBuild func()

// run is the background builder: the queued jobs, one after another. A job
// that panics fails its file, which is not built again until it changes.
func (s *streamIndexStore) run(h *HLSHandler) {
	for j := range s.queue {
		metrics.CopyScanQueue.Dec()
		h.runCopyJob(j)
		s.mu.Lock()
		delete(s.queued, j.path)
		s.mu.Unlock()
		s.jobs.Done()
	}
}

// runCopyJob runs one background build.
func (h *HLSHandler) runCopyJob(j copyScanJob) {
	defer func() {
		if r := recover(); r != nil {
			err := recoveredCopy("the background build of "+filepath.Base(j.path), r)
			if j.passPlan != nil {
				h.passPlanFailed(j, err)
				return
			}
			if st, serr := os.Stat(j.path); serr == nil && st.Size() == j.size && st.ModTime().Equal(j.mtime) {
				h.putScanned(j, st, nil, err)
			} else {
				h.streamIndexes.dropPending(j.path)
			}
		}
	}()
	if beforeBuild != nil {
		beforeBuild()
	}
	if j.passPlan != nil {
		j.passPlan()
		return
	}
	h.scanCopyIndex(j)
}

// scanCopyIndex reads a Matroska file without usable Cues through for its
// keyframes, at copyScanRate, and keeps the index (or why there is none).
func (h *HLSHandler) scanCopyIndex(j copyScanJob) {
	t0 := time.Now()
	st, err := os.Stat(j.path)
	if err != nil || st.Size() != j.size || !st.ModTime().Equal(j.mtime) {
		h.streamIndexes.dropPending(j.path) // gone or changed: a later session asks anew
		return
	}
	ix, err := safeIndexOriginal(j.path, st, &j.probe)
	if err == nil || ix == nil || !errors.Is(err, streamcopy.ErrNoIndex) {
		h.putScanned(j, st, ix, err)
		return
	}
	f, err := os.Open(j.path)
	if err != nil {
		h.putScanned(j, st, nil, err) // the storage's: tried again after a while
		return
	}
	defer f.Close()
	if err = safeScan(ix, f, j.path); err == nil {
		err = ix.Check()
	}
	metrics.CopyIndexDuration.WithLabelValues("scan").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.CopyIndexes.WithLabelValues("failed").Inc()
		log.Printf("stream copy scan of %s: %v — it plays the encode path", filepath.Base(j.path), err)
		h.putScanned(j, st, nil, err)
		return
	}
	metrics.CopyIndexes.WithLabelValues("scan").Inc()
	log.Printf("stream copy: %s indexed by a scan in %v (%d keyframes)", filepath.Base(j.path), time.Since(t0).Round(time.Second), len(ix.Keyframes))
	h.putScanned(j, st, ix, nil)
}

// safeScan reads the Matroska file f through for its keyframes at
// copyScanRate, a panic an error of the file's.
func safeScan(ix *streamcopy.Index, f io.ReaderAt, path string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoveredCopy("the scan of "+filepath.Base(path), r)
		}
	}()
	return ix.ScanMatroska(&rateReader{r: f, rate: copyScanRate, start: time.Now()})
}

// putScanned keeps what a background build found: the index (err nil), or
// why there is none — the file's, kept with the persisted indexes, or a
// passing one (the storage failing a read), kept in memory and tried again
// after copyIndexRetry.
func (h *HLSHandler) putScanned(j copyScanJob, st os.FileInfo, ix *streamcopy.Index, err error) {
	e := &streamIndexEntry{size: st.Size(), mtime: st.ModTime(), ix: ix}
	switch {
	case err == nil:
		writePersistedIndex(j.dir, j.path, st, ix, "")
	case permanentCopyError(err):
		e.ix, e.state, e.why = nil, indexFailed, err.Error()
		writePersistedIndex(j.dir, j.path, st, nil, e.why)
	default:
		e.ix, e.state, e.why, e.until = nil, indexFailed, err.Error(), time.Now().Add(copyIndexRetry)
	}
	h.streamIndexes.put(j.path, e)
}

// rateReader is an io.ReaderAt that reads no faster than rate bytes/s on
// average.
type rateReader struct {
	r     io.ReaderAt
	rate  int64
	start time.Time
	mu    sync.Mutex
	n     int64
}

func (rr *rateReader) ReadAt(b []byte, off int64) (int, error) {
	n, err := rr.r.ReadAt(b, off)
	if rr.rate > 0 {
		rr.mu.Lock()
		rr.n += int64(n)
		due := rr.start.Add(time.Duration(float64(rr.n) / float64(rr.rate) * float64(time.Second)))
		rr.mu.Unlock()
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
	}
	return n, err
}

// sweepIndexes removes the persisted indexes unused for copyIndexKeep.
func (h *HLSHandler) sweepIndexes() {
	dir := h.indexDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-copyIndexKeep)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
