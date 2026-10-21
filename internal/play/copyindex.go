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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/mkv"
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
// file could not be read) is tried again.
const copyIndexRetry = time.Minute

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
	mu    sync.Mutex
	m     map[string]*streamIndexEntry
	locks sync.Map // map[string]*sync.Mutex: one build per path at a time

	queue   chan copyScanJob
	queued  map[string]bool
	started bool
	jobs    sync.WaitGroup // the background builds (tests wait for them)
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
func (h *HLSHandler) streamIndex(_ context.Context, src *source) (*streamcopy.Index, int, string) {
	if src.fromPackage() || src.file == "" || src.probe == nil {
		return nil, indexFailed, "no original"
	}
	st, err := os.Stat(src.file)
	if err != nil {
		return nil, indexFailed, err.Error()
	}
	s := &h.streamIndexes
	if e := s.get(src.file, st); e != nil {
		return e.ix, e.state, e.why
	}
	mu, _ := s.locks.LoadOrStore(src.file, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if e := s.get(src.file, st); e != nil {
		return e.ix, e.state, e.why
	}
	e := h.loadStreamIndex(src, st)
	s.put(src.file, e)
	return e.ix, e.state, e.why
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
	if e.state == indexFailed && !e.until.IsZero() && time.Now().After(e.until) {
		delete(s.m, path) // a passing failure: try again
		return nil
	}
	e.used = time.Now()
	return e
}

// put keeps e for path, the least used dropped beyond the memory's room.
func (s *streamIndexStore) put(path string, e *streamIndexEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if p, ok := readPersistedIndex(dir, src.file, st); ok {
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
	ix, err := indexOriginal(src.file, st, probe)
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
	case errors.Is(err, streamcopy.ErrNoIndex) && ix != nil:
		// No usable Cues: read the file through, apart from any request.
		metrics.CopyIndexes.WithLabelValues("pending").Inc()
		e.state, e.why = indexPending, err.Error()
		h.streamIndexes.enqueue(copyScanJob{path: src.file, size: st.Size(), mtime: st.ModTime(), probe: *probe, dir: dir}, h)
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

// permanentCopyError says whether err is about the file itself (its format,
// a stream the copy does not take), not about reading it: the file is then
// not copied until it changes.
func permanentCopyError(err error) bool {
	return errors.Is(err, mkv.ErrFormat) || errors.Is(err, streamcopy.ErrUnsupported) || errors.Is(err, streamcopy.ErrNoIndex) ||
		errors.Is(err, errCopyMismatch)
}

// errCopyMismatch is an index whose tracks are not the probe's.
var errCopyMismatch = errors.New("the file's tracks are not the ones its probe lists")

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

// copyFailed notes that the copy of the original of src failed (a segment
// could not be made): new sessions of it take the encode path, until the
// file changes when the failure is about the file, else for a while.
func (h *HLSHandler) copyFailed(src *source, reason string, err error) {
	st, serr := os.Stat(src.file)
	if serr != nil {
		return
	}
	metrics.CopyFallbacks.WithLabelValues(reason).Inc()
	e := &streamIndexEntry{size: st.Size(), mtime: st.ModTime(), state: indexFailed, why: err.Error(), mp4: !isMatroska(src.probe.Container)}
	if permanentCopyError(err) {
		if !e.mp4 {
			writePersistedIndex(h.indexDir(), src.file, st, nil, e.why)
		}
	} else {
		e.until = time.Now().Add(copyIndexRetry)
	}
	h.streamIndexes.put(src.file, e)
	log.Printf("stream copy of %s failed (%s): %v — new sessions play the encode path", src.label(), reason, err)
}

// enqueue queues a background build, one per file; the builder runs one at a
// time.
func (s *streamIndexStore) enqueue(j copyScanJob, h *HLSHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued == nil {
		s.queued = map[string]bool{}
	}
	if s.queued[j.path] {
		return
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
	default:
		// A full queue: the file is asked again by a later session.
	}
}

// beforeBuild, when set (tests), runs before each background build.
var beforeBuild func()

// run is the background builder: the queued jobs, one after another.
func (s *streamIndexStore) run(h *HLSHandler) {
	for j := range s.queue {
		if beforeBuild != nil {
			beforeBuild()
		}
		metrics.CopyScanQueue.Dec()
		if j.passPlan != nil {
			j.passPlan()
		} else {
			h.scanCopyIndex(j)
		}
		s.mu.Lock()
		delete(s.queued, j.path)
		s.mu.Unlock()
		s.jobs.Done()
	}
}

// scanCopyIndex reads a Matroska file without usable Cues through for its
// keyframes, at copyScanRate, and keeps the index (or why there is none).
func (h *HLSHandler) scanCopyIndex(j copyScanJob) {
	t0 := time.Now()
	st, err := os.Stat(j.path)
	if err != nil || st.Size() != j.size || !st.ModTime().Equal(j.mtime) {
		return // gone or changed: a later session queues it anew
	}
	ix, err := indexOriginal(j.path, st, &j.probe)
	if err == nil {
		h.putScanned(j, st, ix, "")
		return
	}
	if ix == nil || !errors.Is(err, streamcopy.ErrNoIndex) {
		h.putScanned(j, st, nil, err.Error())
		return
	}
	f, err := os.Open(j.path)
	if err != nil {
		return
	}
	defer f.Close()
	if err = ix.ScanMatroska(&rateReader{r: f, rate: copyScanRate, start: time.Now()}); err == nil {
		err = ix.Check()
	}
	metrics.CopyIndexDuration.WithLabelValues("scan").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.CopyIndexes.WithLabelValues("failed").Inc()
		log.Printf("stream copy scan of %s: %v — it plays the encode path", filepath.Base(j.path), err)
		h.putScanned(j, st, nil, err.Error())
		return
	}
	metrics.CopyIndexes.WithLabelValues("scan").Inc()
	log.Printf("stream copy: %s indexed by a scan in %v (%d keyframes)", filepath.Base(j.path), time.Since(t0).Round(time.Second), len(ix.Keyframes))
	h.putScanned(j, st, ix, "")
}

// putScanned keeps what a background build found.
func (h *HLSHandler) putScanned(j copyScanJob, st os.FileInfo, ix *streamcopy.Index, why string) {
	e := &streamIndexEntry{size: st.Size(), mtime: st.ModTime(), ix: ix}
	if ix == nil {
		e.state, e.why = indexFailed, why
	}
	writePersistedIndex(j.dir, j.path, st, ix, why)
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
