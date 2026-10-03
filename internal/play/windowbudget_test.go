package play

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// audioOnly is an ffprobe answer for a 9 s source with one 5.1 AC-3 track.
const audioOnly = `{"streams":[{"codec_type":"audio","codec_name":"ac3","channels":6}],"format":{"format_name":"matroska,webm","duration":"9.0"}}`

// audioHandler is an HLSHandler for a 9 s audio-only source, its ffmpeg the
// fake at ffmpegBin and its failure budget on the clock *now.
func audioHandler(t *testing.T, ffmpegBin string, now *time.Time) *HLSHandler {
	t.Helper()
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  ffmpegBin,
		FFprobeBin: probeFFprobe(t, audioOnly),
		CacheDir:   t.TempDir(),
	}
	h.windowFails.now = func() time.Time { return *now }
	return h
}

// Two failed runs give a window up: the next request answers 404 at once
// (the player falls back instead of retrying) without running ffmpeg, and
// says why. Once the cooldown has passed the window runs again — the cap no
// longer lasts until the pod restarts.
func TestFailedWindowIsGivenUpUntilTheCooldownPasses(t *testing.T) {
	logs := captureLog(t)
	state := t.TempDir()
	now := time.Date(2026, 10, 3, 21, 7, 0, 0, time.UTC)
	h := audioHandler(t, windowFFmpeg(t, state, 2, 2, "Unknown encoder 'libfdk_aac'"), &now)

	for i := 1; i <= 2; i++ {
		if w := get(h, "/api/play/i1/audio/0/init.mp4"); w.Code != 502 {
			t.Fatalf("request %d: %d %q, want 502 (a failed run the player may retry)", i, w.Code, w.Body)
		}
	}
	if !strings.Contains(logs.String(), "ffmpeg: exit status 1: Unknown encoder 'libfdk_aac'") {
		t.Errorf("the failed request's log line must carry ffmpeg's stderr tail:\n%s", logs)
	}
	logs.Reset()
	now = now.Add(windowRetryCooldown - time.Second)
	w := get(h, "/api/play/i1/audio/0/init.mp4")
	if w.Code != 404 {
		t.Fatalf("out of budget: %d %q, want 404", w.Code, w.Body)
	}
	if n := len(ffmpegRuns(t, state)); n != 2 {
		t.Errorf("%d ffmpeg runs, want 2 (a window given up on runs none)", n)
	}
	if l := logs.String(); !strings.Contains(l, "gave up on seg 0 after 2 failed runs, next try in 1s") ||
		!strings.Contains(l, "last: ffmpeg: exit status 1: Unknown encoder 'libfdk_aac'") {
		t.Errorf("the give-up log line must say when it retries and why it failed:\n%s", l)
	}

	now = now.Add(time.Second)
	if w := get(h, "/api/play/i1/audio/0/init.mp4"); w.Code != 200 || w.Body.String() != "init" {
		t.Fatalf("after the cooldown: %d %q, want the init", w.Code, w.Body)
	}
	if w := get(h, "/api/play/i1/audio/0/1.m4s"); w.Code != 200 {
		t.Fatalf("seg 1 of the produced window: %d", w.Code)
	}
	if n := len(ffmpegRuns(t, state)); n != 3 {
		t.Errorf("%d ffmpeg runs, want 3", n)
	}
	if rec, ok := h.windowFails.m["i1/audio-0/0"]; ok {
		t.Errorf("a produced window keeps a failure record %+v", rec)
	}
}

// A run the request abandons (client seek, warm timeout, slot wait given up)
// is not charged: before, two of them killed the window for good.
func TestAbandonedRunsAreNotCharged(t *testing.T) {
	state := t.TempDir()
	now := time.Now()
	// The first run hangs until killed; later runs produce the window.
	ffmpeg := fakeBin(t, "ffmpeg", `
state='`+state+`'
n=$(cat "$state/runs" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$state/runs"
if [ "$n" -eq 1 ]; then exec sleep 30; fi
for last; do :; done
out=$(dirname "$last")
printf 'init' > "$out/init.mp4"; printf 'seg' > "$out/seg_0.m4s"`)
	h := audioHandler(t, ffmpeg, &now)
	src := h.MediaRoot + "/film.mkv"
	probe := &Probe{DurationMs: 9000, AudioTracks: []TrackInfo{{Index: 0, Codec: "ac3", Channels: 6}}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.ensureAudioWindow(ctx, "i1", src, 0, probe, 0, 0); err == nil {
		t.Fatal("a run killed with its request must fail")
	}
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	for i := 0; i < 3; i++ {
		if err := h.ensureAudioWindow(dead, "i1", src, 0, probe, 0, 0); err == nil {
			t.Fatal("a request already gone must not produce the window")
		}
	}
	if rec, _ := h.windowFails.check("i1/audio-0/0"); rec.count != 0 {
		t.Fatalf("abandoned runs were charged: %+v", rec)
	}
	if err := h.ensureAudioWindow(context.Background(), "i1", src, 0, probe, 0, 0); err != nil {
		t.Fatalf("the next live request: %v", err)
	}
}

// ffmpeg exiting 0 without the asked-for segment (the NVENC truncation) is
// retried at once, on a clean slate; a second short run gives the window
// up, naming the stderr tail. What the last run produced is served.
func TestShortWindowIsRetriedOnceThenGivenUp(t *testing.T) {
	logs := captureLog(t)
	state := t.TempDir()
	// Both runs come up short of seg 2 (a 15 s source has three): the
	// first leaves segs 0 and 1, the second seg 0 only.
	ffmpeg := fakeBin(t, "ffmpeg", `
state='`+state+`'
n=$(cat "$state/runs" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$state/runs"
echo '[hls @ 0x1] Opening seg_0.m4s for writing' >&2
echo 'Error while decoding stream #0:0: Invalid data found' >&2
for last; do :; done
out=$(dirname "$last")
printf 'init' > "$out/init.mp4"; printf "seg0-run$n" > "$out/seg_0.m4s"
if [ "$n" -eq 1 ]; then printf 'seg1-run1' > "$out/seg_1.m4s"; fi`)
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  ffmpeg,
		FFprobeBin: probeFFprobe(t, strings.Replace(audioOnly, `"9.0"`, `"15.0"`, 1)),
		CacheDir:   t.TempDir(),
	}

	if w := get(h, "/api/play/i1/audio/0/2.m4s"); w.Code != 404 {
		t.Fatalf("seg 2 never lands: %d %q, want 404", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(state + "/runs"); strings.TrimSpace(string(b)) != "2" {
		t.Errorf("ffmpeg ran %s times, want 2 (one retry)", b)
	}
	if l := logs.String(); !strings.Contains(l, "partial transcode (ffmpeg exited 0 without seg 2; stderr: "+
		"[hls @ 0x1] Opening seg_0.m4s for writing | Error while decoding stream #0:0: Invalid data found)") {
		t.Errorf("a short run must be logged with its stderr tail:\n%s", l)
	}
	if w := get(h, "/api/play/i1/audio/0/0.m4s"); w.Code != 200 || w.Body.String() != "seg0-run2" {
		t.Errorf("seg 0: %d %q, want the retry's", w.Code, w.Body)
	}
	if w := get(h, "/api/play/i1/audio/0/1.m4s"); w.Code != 404 {
		t.Errorf("seg 1 of the first run survived the retry: %d %q", w.Code, w.Body)
	}
}

// A window whose cooldown has passed runs again — after wiping what the
// failed runs left — and a failure then starts a fresh count.
func TestWindowFailuresCheck(t *testing.T) {
	now := time.Now()
	f := &windowFailures{now: func() time.Time { return now }}
	if rec, wait := f.check("w"); rec.count != 0 || wait != 0 {
		t.Fatalf("unknown window: %+v, %s", rec, wait)
	}
	f.fail("w", "first")
	if rec, wait := f.check("w"); rec.count != 1 || wait != 0 {
		t.Errorf("one failure: %+v, %s; want it to run again", rec, wait)
	}
	f.fail("w", "second")
	now = now.Add(30 * time.Second)
	if rec, wait := f.check("w"); rec.cause != "second" || wait != windowRetryCooldown-30*time.Second {
		t.Errorf("out of budget: %+v, %s", rec, wait)
	}
	now = now.Add(windowRetryCooldown)
	if rec, wait := f.check("w"); rec.count != 2 || wait != 0 {
		t.Errorf("after the cooldown: %+v, %s; want the record (to wipe its leftovers) and no wait", rec, wait)
	}
	if n := f.fail("w", "third"); n != 1 {
		t.Errorf("a failure after the cooldown counts %d, want 1", n)
	}
}

// The cache sweeper also drops failure records past their cooldown.
func TestSweepPrunesVoidFailureRecords(t *testing.T) {
	now := time.Now()
	h := &HLSHandler{CacheDir: t.TempDir()}
	h.windowFails.now = func() time.Time { return now }
	h.windowFails.fail("i1/high/0", "x")
	now = now.Add(windowRetryCooldown)
	h.sweep(time.Hour)
	if len(h.windowFails.m) != 0 {
		t.Errorf("records after the sweep: %v", h.windowFails.m)
	}
}

// The video rendition maps a given-up window to 404 as well — its init
// included, which used to answer 500 ("cache read").
func TestVideoWindowGivenUpAnswers404(t *testing.T) {
	captureLog(t)
	state := t.TempDir()
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{
		Catalog:         fakeKatalog(t, src),
		MediaRoot:       root,
		FFmpegBin:       windowFFmpeg(t, state, 99, 0, "Invalid Level"),
		FFprobeBin:      probeFFprobe(t, `{"streams":[{"codec_type":"video","codec_name":"hevc","width":1920,"height":1080}],"format":{"format_name":"matroska,webm","duration":"9.0"}}`),
		CacheDir:        t.TempDir(),
		TranscodePreset: "veryfast",
	}
	for i, want := range []int{502, 502, 404, 404} {
		if w := get(h, "/api/play/i1/high/init.mp4"); w.Code != want {
			t.Fatalf("request %d: %d %q, want %d", i+1, w.Code, w.Body, want)
		}
	}
	if w := get(h, "/api/play/i1/high/0.m4s"); w.Code != 404 {
		t.Errorf("a segment of the given-up window: %d, want 404", w.Code)
	}
	if n := len(ffmpegRuns(t, state)); n != 2 {
		t.Errorf("%d ffmpeg runs, want 2", n)
	}
}

func TestRunFFmpegErrorCarriesTheStderrTail(t *testing.T) {
	logs := captureLog(t)
	h := &HLSHandler{FFmpegBin: fakeBin(t, "ffmpeg", `
i=0; while [ $i -lt 200 ]; do echo "warning $i: non-monotonic DTS" >&2; i=$((i+1)); done
echo "Unknown encoder 'libfdk_aac'" >&2
exit 1`)}
	tail, err := h.runFFmpeg(context.Background(), nil, "awin 0/0 film.mkv")
	if err == nil || !strings.HasSuffix(err.Error(), "warning 199: non-monotonic DTS | Unknown encoder 'libfdk_aac'") ||
		!strings.HasPrefix(err.Error(), "ffmpeg: exit status 1: ") {
		t.Fatalf("err = %v", err)
	}
	if len(tail) > stderrTailBytes || strings.HasPrefix(tail, "ing") || !strings.HasPrefix(tail, "warning ") {
		t.Errorf("tail is not the last whole lines within %d bytes: %q", stderrTailBytes, tail)
	}
	if !strings.Contains(logs.String(), "ffmpeg awin 0/0 film.mkv: warning 0: non-monotonic DTS") {
		t.Errorf("stderr is still logged as it arrives, with the label:\n%.300s", logs)
	}
	if _, err := (&HLSHandler{FFmpegBin: fakeBin(t, "ffmpeg", "exit 0")}).runFFmpeg(context.Background(), nil, "x"); err != nil {
		t.Errorf("a clean run: %v", err)
	}
}

func TestTailBuffer(t *testing.T) {
	cases := []struct {
		name   string
		max    int
		writes []string
		want   string
	}{
		{"fits", 32, []string{"first line\r\n", "  second line \n"}, "first line | second line"},
		{"cut mid-line drops the partial line", 16, []string{"abcdefghij\n", "klmno\n"}, "klmno"},
		{"cut at a line break keeps the next line", 6, []string{"abcdefghij\n", "klmno\n"}, "klmno"},
		{"joined tail stays within max", 12, []string{"aaa\nbbb\nccc\n"}, "bbb | ccc"},
		{"one long line is kept as it is", 8, []string{"0123456789abcdef"}, "89abcdef"},
	}
	for _, tc := range cases {
		tb := &tailBuffer{max: tc.max}
		for _, w := range tc.writes {
			_, _ = tb.Write([]byte(w))
		}
		if got := tb.String(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWindowFailuresPruneVoidRecords(t *testing.T) {
	now := time.Now()
	f := &windowFailures{now: func() time.Time { return now }}
	f.fail("old", "x")
	now = now.Add(windowRetryCooldown)
	f.fail("new", "y")
	f.prune()
	if _, ok := f.m["old"]; ok {
		t.Error("a record past the cooldown survived prune")
	}
	if _, ok := f.m["new"]; !ok {
		t.Error("prune dropped a live record")
	}
	// A failure after the cooldown starts a fresh count.
	f.fail("new", "z")
	now = now.Add(windowRetryCooldown)
	if n := f.fail("new", "z"); n != 1 {
		t.Errorf("count %d after the cooldown, want 1", n)
	}
}
