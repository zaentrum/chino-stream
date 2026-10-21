package play

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/mkv"
	"github.com/zaentrum/chino-stream/internal/streamcopy"
)

// Originals the stream copy must not trip over, through the HTTP routes:
// damaged or cut short, oddly timed, unreadable for a while.

// plainFilm is an H.264 (a keyframe every 2 s, no B-frames) Matroska film of
// dur seconds with audioDur seconds of stereo AAC (none for 0) and, with
// subs, SubRip subtitles every 2 s — which ffmpeg writes as BlockGroups.
func plainFilm(t *testing.T, ffmpeg string, dur, audioDur int, subs bool) (root, file string) {
	t.Helper()
	if enc := allEncoders(t, ffmpeg); !enc["libx264"] || audioDur > 0 && !enc["aac"] {
		t.Skip("this ffmpeg has no libx264 or aac")
	}
	root = t.TempDir()
	file = filepath.Join(root, "film.mkv")
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001:duration=" + strconv.Itoa(dur)}
	maps := []string{"-map", "0"}
	n := 1
	if audioDur > 0 {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+strconv.Itoa(audioDur))
		maps = append(maps, "-map", strconv.Itoa(n))
		n++
	}
	if subs {
		srt := filepath.Join(root, "subs.srt")
		var sb strings.Builder
		for i := 0; i < dur/2; i++ {
			sb.WriteString(strconv.Itoa(i+1) + "\n00:00:" + twoDigits(i*2) + ",000 --> 00:00:" + twoDigits(i*2+1) + ",500\nLine " +
				strconv.Itoa(i) + strings.Repeat(" padding text", 20) + "\n\n")
		}
		if err := os.WriteFile(srt, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-i", srt)
		maps = append(maps, "-map", strconv.Itoa(n))
	}
	args = append(append(args, maps...), "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-keyint_min", "48",
		"-sc_threshold", "0", "-bf", "0")
	if audioDur > 0 {
		args = append(args, "-c:a", "aac", "-ac", "2")
	}
	if subs {
		args = append(args, "-c:s", "srt")
	}
	run(t, ffmpeg, append(args, "-y", file)...)
	return root, file
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// fileBlock is a block of a Matroska file and where it is.
type fileBlock struct {
	track        uint64
	ts           int64
	key          bool
	pos, cluster int64
	frameLen     int
}

func fileBlocks(t *testing.T, file string) []fileBlock {
	t.Helper()
	fh, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	f, err := mkv.Open(fh, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	rd := f.Blocks(fh, f.FirstCluster)
	var out []fileBlock
	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if len(b.Frames) > 0 {
			n = len(b.Frames[0])
		}
		out = append(out, fileBlock{track: b.Track, ts: b.Timestamp, key: b.Keyframe, pos: b.Pos, cluster: b.Cluster, frameLen: n})
	}
	return out
}

// copySession is a stream copy session's master: its variant's URI (the
// video's playlist, with its query) and its audio renditions' URIs.
func copySession(t *testing.T, h *HLSHandler, caps string) (video string, audio []string) {
	t.Helper()
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	media, variants := copyMaster(m.Body.String())
	if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") {
		t.Fatalf("not a stream copy:\n%s", m.Body)
	}
	for _, a := range media {
		audio = append(audio, a["URI"])
	}
	return variants[0]["uri"], audio
}

// segmentURI is segment n of the playlist at uri.
func segmentURI(uri string, n int) string {
	return "/api/play/i1/" + strings.Replace(uri, "index.m3u8", strconv.Itoa(n)+".m4s", 1)
}

func isStreamCopy(h *HLSHandler, caps string) bool {
	return strings.Contains(get(h, "/api/play/i1/master.m3u8"+caps).Body.String(), "video/index.m3u8")
}

// A segment that fails the same way every time (two video frames at one
// time): answered 404 at once — no 503 to be asked again for ever, no second
// read — and the title's new sessions take the encode path.
func TestASegmentThatFailsTheSameWayFailsTheTitle(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 24, 24, true)
	blocks := fileBlocks(t, file)
	b, _ := os.ReadFile(file)
	var prev *fileBlock
	patched := false
	for i := range blocks {
		bl := &blocks[i]
		if bl.track != 1 {
			continue
		}
		if bl.ts >= 13000 && !bl.key && prev != nil && prev.cluster == bl.cluster {
			p := bl.pos + 1
			l := int64(1)
			for m := byte(0x80); b[p]&m == 0; m >>= 1 {
				l++
			}
			p += l + 1 // past the SimpleBlock's size and its track
			rel := int64(int16(binary.BigEndian.Uint16(b[p:])))
			binary.BigEndian.PutUint16(b[p:], uint16(prev.ts-(bl.ts-rel)))
			patched = true
			break
		}
		prev = bl
	}
	if !patched {
		t.Fatal("not patched")
	}
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	video, _ := copySession(t, h, caps)
	ix, _, _ := h.streamIndex(context.Background(), &source{key: "i1", file: file, probe: mustProbe(t, ffprobe, file)})
	bad := -1
	for n, s := range ix.Spans() {
		if s.Start <= 13 && 13 < s.Start+s.Dur {
			bad = n
		}
	}
	var made atomic.Int32
	produceHook = func(n int) {
		if n == bad {
			made.Add(1)
		}
	}
	t.Cleanup(func() { produceHook = nil })
	if w := get(h, segmentURI(video, bad)); w.Code != 404 {
		t.Errorf("the segment that cannot be made: %d %s, want 404", w.Code, w.Header().Get("Retry-After"))
	}
	if made.Load() != 1 {
		t.Errorf("the segment was made %d times for one request", made.Load())
	}
	if isStreamCopy(h, caps) {
		t.Error("a new session is a stream copy again")
	}
}

// A segment the storage cannot read (the file unreadable for a while): 503
// with Retry-After, asked again; after copySegmentTries such failures in a
// row 404, and the title's new sessions take the encode path — for a while
// only: once the file reads again, it is copied again.
func TestAPassingFailureIsAskedAgainThenFailsTheTitleForAWhile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0")
	}
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	// Long enough for the failure to hold while it is looked at, even with
	// the race detector on a busy machine.
	old := copyIndexRetry
	copyIndexRetry = 2 * time.Second
	t.Cleanup(func() { copyIndexRetry = old })
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	video, _ := copySession(t, h, caps)
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
	for try := 1; try <= copySegmentTries; try++ {
		w := get(h, segmentURI(video, 1))
		want := 503
		if try == copySegmentTries {
			want = 404
		}
		if w.Code != want || (want == 503 && w.Header().Get("Retry-After") == "") {
			t.Fatalf("try %d: %d (Retry-After %q), want %d", try, w.Code, w.Header().Get("Retry-After"), want)
		}
	}
	if isStreamCopy(h, caps) {
		t.Error("a new session is a stream copy while the file cannot be read")
	}
	if entries, _ := os.ReadDir(h.indexDir()); len(entries) != 1 {
		t.Errorf("%d persisted entries: a passing failure was kept with the index", len(entries))
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * copyIndexRetry)
	if !isStreamCopy(h, caps) {
		t.Error("the file reads again, and is not copied again")
	}
}

// A Matroska file cut short inside a subtitle's BlockGroup (its Cues, at the
// end, cut away): read through in the background without a panic, and then
// copied, every segment.
func TestTheScanOfAFileCutShort(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 24, 24, true)
	var last *fileBlock
	blocks := fileBlocks(t, file)
	for i := range blocks {
		if blocks[i].track == 3 {
			last = &blocks[i]
		}
	}
	if last == nil || last.frameLen < 40 {
		t.Fatalf("no subtitle block large enough: %+v", last)
	}
	b, _ := os.ReadFile(file)
	// BlockGroup a0 size, Block a1 size, the block's header, 20 bytes in.
	p := last.pos + 1
	l := int64(1)
	for m := byte(0x80); b[p]&m == 0; m >>= 1 {
		l++
	}
	p += l
	if b[p] != 0xA1 {
		t.Fatalf("no Block in the BlockGroup at %d", last.pos)
	}
	p++
	l = 1
	for m := byte(0x80); b[p]&m == 0; m >>= 1 {
		l++
	}
	p += l + 4 + 20
	if err := os.WriteFile(file, b[:p], 0o644); err != nil {
		t.Fatal(err)
	}
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	if isStreamCopy(h, caps) {
		t.Fatal("a file without Cues copied before it was read through")
	}
	h.streamIndexes.jobs.Wait()
	video, audio := copySession(t, h, caps)
	pl := get(h, "/api/play/i1/"+video).Body.String()
	for n := 0; n < strings.Count(pl, ".m4s"); n++ {
		for _, uri := range append([]string{video}, audio...) {
			if w := get(h, segmentURI(uri, n)); w.Code != 200 {
				t.Errorf("%s segment %d: %d", uri, n, w.Code)
			}
		}
	}
}

// Audio that ends before the video (credits without sound): every segment
// of the copied audio is there, the ones after its end without samples.
func TestStreamCopyOfAudioShorterThanTheVideo(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 30, 12, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	video, audio := copySession(t, h, "?caps=avc,aac")
	if len(audio) != 1 || !strings.HasPrefix(audio[0], copiedPrefix) {
		t.Fatalf("audio %v", audio)
	}
	n := strings.Count(get(h, "/api/play/i1/"+video).Body.String(), ".m4s")
	if n < 4 {
		t.Fatalf("%d segments", n)
	}
	for k := 0; k < n; k++ {
		if w := get(h, segmentURI(audio[0], k)); w.Code != 200 {
			t.Errorf("audio segment %d: %d %s", k, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// A video without audio: its variant names no audio group (AVPlayer rejects
// one that is not defined).
func TestStreamCopyOfAVideoWithoutAudio(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 14, 0, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	for _, caps := range []string{"?caps=avc,aac", "?caps=avc,aac,native"} {
		m := get(h, "/api/play/i1/master.m3u8"+caps).Body.String()
		if !strings.Contains(m, "video/index.m3u8") || strings.Contains(m, "AUDIO=") || strings.Contains(m, "TYPE=AUDIO") {
			t.Errorf("%s:\n%s", caps, m)
		}
	}
}

// A request for a segment a prefetch is waiting to make (every prefetch slot
// busy) is served at once, not after the prefetch.
func TestARequestDoesNotWaitForAPrefetch(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	root, file := plainFilm(t, ffmpeg, 20, 20, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	video, _ := copySession(t, h, "?caps=avc,aac")
	h.copyWork.Wait()
	src := &source{key: "i1", file: file, probe: mustProbe(t, ffprobe, file)}
	ix, _, _ := h.streamIndex(context.Background(), src)
	// Every prefetch slot taken.
	for i := 0; i < cap(copyPrefetchSlots); i++ {
		copyPrefetchSlots <- struct{}{}
	}
	released := false
	release := func() {
		if !released {
			released = true
			for i := 0; i < cap(copyPrefetchSlots); i++ {
				<-copyPrefetchSlots
			}
		}
	}
	t.Cleanup(release)
	n := len(ix.Spans()) - 1
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		_, _ = h.ensureCopySegment(context.Background(), src, ix, n, true)
	}()
	time.Sleep(50 * time.Millisecond) // the prefetch waits for its slot
	done := make(chan int, 1)
	go func() { done <- get(h, segmentURI(video, n)).Code }()
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("segment %d: %d", n, code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request waits for the prefetch")
	}
	release()
	<-waiting
}

// A Matroska file without Cues whose background scan cannot read it (the
// storage's failure): nothing is kept of the failure with the persisted
// indexes, and after a while the file is queued again, read and copied.
func TestAScanThatCannotReadTheFileIsTriedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0")
	}
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	old := copyIndexRetry
	copyIndexRetry = 200 * time.Millisecond
	t.Cleanup(func() { copyIndexRetry = old })
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	voidCues(t, file)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	var once sync.Once
	beforeBuild = func() { once.Do(func() { _ = os.Chmod(file, 0) }) }
	t.Cleanup(func() { beforeBuild = nil; _ = os.Chmod(file, 0o644) })
	if isStreamCopy(h, caps) {
		t.Fatal("copied before the scan")
	}
	h.streamIndexes.jobs.Wait()
	if entries, _ := os.ReadDir(h.indexDir()); len(entries) != 0 {
		t.Errorf("%d persisted entries after a scan that could not read the file", len(entries))
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * copyIndexRetry)
	if isStreamCopy(h, caps) {
		t.Fatal("copied before the second scan")
	}
	h.streamIndexes.jobs.Wait()
	if !isStreamCopy(h, caps) {
		t.Error("not copied after the second scan")
	}
}

// A file the scan queue has no room for: asked again after a while, not
// pending for ever.
func TestAFileTheScanQueueHasNoRoomForIsAskedAgain(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	old := copyIndexRetry
	copyIndexRetry = 200 * time.Millisecond
	t.Cleanup(func() { copyIndexRetry = old })
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	voidCues(t, file)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	// A queue that takes nothing.
	s := &h.streamIndexes
	s.mu.Lock()
	s.started, s.queue = true, make(chan copyScanJob)
	s.mu.Unlock()
	const caps = "?caps=avc,aac"
	if isStreamCopy(h, caps) {
		t.Fatal("copied without a scan")
	}
	// Room again.
	s.mu.Lock()
	s.started, s.queue = false, nil
	s.mu.Unlock()
	time.Sleep(2 * copyIndexRetry)
	_ = isStreamCopy(h, caps) // queued now
	h.streamIndexes.jobs.Wait()
	if !isStreamCopy(h, caps) {
		t.Error("not copied once the queue had room")
	}
}

// A persisted index whose tracks are not the probe's (the file remuxed in
// place with its mtime kept): made anew, not used — and no master fails.
func TestAPersistedIndexOfOtherTracksIsMadeAgain(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	copySession(t, h, caps)
	entries, _ := os.ReadDir(h.indexDir())
	if len(entries) != 1 {
		t.Fatalf("%d persisted indexes", len(entries))
	}
	path := filepath.Join(h.indexDir(), entries[0].Name())
	b, _ := os.ReadFile(path)
	var p persistedIndex
	if err := json.Unmarshal(b, &p); err != nil || p.Index == nil {
		t.Fatal(err)
	}
	p.Index.Audio = append(p.Index.Audio, p.Index.Audio[0]) // a track the probe does not list
	b, _ = json.Marshal(p)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	made := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("cues"))
	again := &HLSHandler{Catalog: h.Catalog, MediaRoot: root, FFmpegBin: h.FFmpegBin, FFprobeBin: ffprobe, CacheDir: h.CacheDir}
	quiesce(t, again)
	w := get(again, "/api/play/i1/master.m3u8"+caps)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "video/index.m3u8") {
		t.Fatalf("the master: %d\n%s", w.Code, w.Body)
	}
	if got := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("cues")); got != made+1 {
		t.Errorf("indexes made from Cues: %v more, want 1", got-made)
	}
}

// Sessions of a file asking for its index at once: one build, which all of
// them get.
func TestOneIndexBuildPerFile(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	probe := mustProbe(t, ffprobe, file)
	made := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("cues"))
	var wg sync.WaitGroup
	got := make([]*streamcopy.Index, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _, _ = h.streamIndex(context.Background(), &source{key: "i1", file: file, probe: probe})
		}(i)
	}
	wg.Wait()
	if n := testutil.ToFloat64(metrics.CopyIndexes.WithLabelValues("cues")); n != made+1 {
		t.Errorf("%v builds", n-made)
	}
	for i := range got {
		if got[i] == nil || got[i] != got[0] {
			t.Fatalf("session %d got another index", i)
		}
	}
}

// A background build that panics fails its file, kept so; the process goes
// on, and the file's sessions take the encode path.
func TestABackgroundBuildThatPanicsFailsItsFile(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	voidCues(t, file)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	beforeBuild = func() { panic("a reader bug") }
	t.Cleanup(func() { beforeBuild = nil })
	panics := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("panic"))
	const caps = "?caps=avc,aac"
	_ = isStreamCopy(h, caps)
	h.streamIndexes.jobs.Wait()
	if got := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("panic")); got != panics+1 {
		t.Errorf("panics counted: %v more, want 1", got-panics)
	}
	ph := &Handler{Catalog: h.Catalog, MediaRoot: root, FFprobeBin: ffprobe, HLS: h}
	var info map[string]any
	_ = json.Unmarshal(getPlay(ph, "/api/play/i1/info"+caps).Body.Bytes(), &info)
	if info["mode"] != "transcode" || !strings.Contains(info["reason"].(string), "could not be played as it is") {
		t.Errorf("info %v", info)
	}
	if entries, _ := os.ReadDir(h.indexDir()); len(entries) != 1 {
		t.Errorf("%d persisted entries: the failure is not kept", len(entries))
	}
}

// A segment whose making panics: 404, the title failed, the panic counted.
func TestASegmentThatPanicsFailsTheTitle(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	const caps = "?caps=avc,aac"
	video, _ := copySession(t, h, caps)
	h.copyWork.Wait()
	produceHook = func(n int) {
		if n == 1 {
			panic("a muxer bug")
		}
	}
	t.Cleanup(func() { produceHook = nil })
	panics := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("panic"))
	if w := get(h, segmentURI(video, 1)); w.Code != 404 {
		t.Errorf("segment 1: %d", w.Code)
	}
	if got := testutil.ToFloat64(metrics.CopyFallbacks.WithLabelValues("panic")); got != panics+1 {
		t.Errorf("panics counted: %v more", got-panics)
	}
	if isStreamCopy(h, caps) {
		t.Error("the title is still copied")
	}
}

// The file's own Dolby Vision record decides, whatever the probe said.
func TestIndexDolbyVisionWhy(t *testing.T) {
	for _, tc := range []struct {
		profile, compat int
		caps            string
		why             string
	}{
		{0, 0, "hvc", ""},
		{8, 1, "hvc", ""},
		{5, 0, "hvc", "does not play Dolby Vision"},
		{5, 0, "hvc,dvh", ""},
		{8, 0, "hvc", "does not play Dolby Vision"},
		{7, 6, "hvc,dvh", "enhancement layer"},
	} {
		ix := &streamcopy.Index{Video: streamcopy.VideoTrack{DVProfile: tc.profile, DVCompatibility: tc.compat}}
		if got := indexDolbyVisionWhy(ix, ParseCaps(tc.caps)); (tc.why == "") != (got == "") || !strings.Contains(got, tc.why) {
			t.Errorf("profile %d compat %d, caps %s: %q", tc.profile, tc.compat, tc.caps, got)
		}
	}
}

// An MPEG-TS original's plan, made by reading it through once, outlives the
// cache's sweep of its item's folder: the session after the sweep is the
// muxed copy at once.
func TestAnMPEGTSPlanOutlivesTheSweep(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	_, mp4 := copyOriginal(t, ffmpeg, false)
	root := t.TempDir()
	ts := filepath.Join(root, "film.ts")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", mp4, "-c", "copy", "-y", ts)
	h := copyHandler(t, root, ts, ffmpeg, ffprobe)
	const caps = "?caps=avc,aac"
	_ = get(h, "/api/play/i1/master.m3u8"+caps)
	h.streamIndexes.jobs.Wait()
	if m := get(h, "/api/play/i1/master.m3u8"+caps); !strings.Contains(m.Body.String(), "\ncopy/index.m3u8") {
		t.Fatalf("the master once its keyframes are known:\n%s", m.Body)
	}
	h.copyWork.Wait()
	waitProductions(t, h)
	h.sweep(0) // every item's folder, the persisted indexes kept
	if statOK(h.cachePath("i1", "copy-"+copyPipelineVersion, "plan")) {
		t.Fatal("the sweep kept the item's plan")
	}
	queued := testutil.ToFloat64(metrics.CopyScanQueue)
	m := get(h, "/api/play/i1/master.m3u8"+caps)
	if !strings.Contains(m.Body.String(), "\ncopy/index.m3u8") {
		t.Fatalf("the master after the sweep:\n%s", m.Body)
	}
	if testutil.ToFloat64(metrics.CopyScanQueue) != queued {
		t.Error("the file is read through again")
	}
	pl := get(h, "/api/play/i1/copy/index.m3u8"+caps)
	if pl.Code != 200 || !strings.Contains(pl.Body.String(), ".m4s") {
		t.Errorf("the copy's playlist after the sweep: %d", pl.Code)
	}
}

// A segment refused for a while (the storage failed it copySegmentTries
// times in a row) that is made meanwhile — by a prefetch, or found in the
// cache — is served, not refused for the rest of the while.
func TestARefusedSegmentIsServedOnceMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0")
	}
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	old := copyIndexRetry
	copyIndexRetry = time.Minute // refused for longer than the test runs
	t.Cleanup(func() { copyIndexRetry = old })
	root, file := plainFilm(t, ffmpeg, 14, 14, false)
	h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 0, 10, ""), ffprobe)
	video, _ := copySession(t, h, "?caps=avc,aac")
	refuse := func() {
		t.Helper()
		if err := os.Chmod(file, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(file, 0o644) }()
		for try := 1; try <= copySegmentTries; try++ {
			if w := get(h, segmentURI(video, 1)); try == copySegmentTries && w.Code != 404 {
				t.Fatalf("try %d: %d", try, w.Code)
			}
		}
		if w := get(h, segmentURI(video, 1)); w.Code != 404 {
			t.Fatalf("not refused: %d", w.Code)
		}
	}
	refuse()
	src := &source{key: "i1", file: file, probe: mustProbe(t, ffprobe, file)}
	ix, _, _ := h.streamIndex(context.Background(), src)
	if ix == nil {
		t.Fatal("no index")
	}
	// Made by a prefetch.
	if _, err := h.ensureCopySegment(context.Background(), src, ix, 1, true); err != nil {
		t.Fatal(err)
	}
	if w := get(h, segmentURI(video, 1)); w.Code != 200 {
		t.Errorf("made by a prefetch: %d", w.Code)
	}
	// In the cache while refused.
	key := h.copyKey(src, ix, 1)
	seg := h.copyCache.get(key)
	h.copyCache.mu.Lock()
	h.copyCache.m, h.copyCache.bytes = nil, 0
	h.copyCache.mu.Unlock()
	refuse()
	h.copyCache.put(key, seg)
	if w := get(h, segmentURI(video, 1)); w.Code != 200 {
		t.Errorf("in the cache: %d", w.Code)
	}
}
