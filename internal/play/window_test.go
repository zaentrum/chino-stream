package play

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gatedFFmpeg is a fake ffmpeg for a window: it writes a real init (the
// media fixture's video init, whose timescale the installer reads) and
// segment 0, then waits for <state>/go before it writes the other segs
// segments and exits — or until it is killed, or the test that ran it is
// gone (a run that timed out would leave it waiting for good). Each run is
// counted in <state>/runs.
func gatedFFmpeg(t *testing.T, state string, segs int) string {
	t.Helper()
	initSrc := filepath.Join(mediaDir(t), "hls", "v0", "init.mp4")
	return fakeBin(t, "ffmpeg", `
state='`+state+`'
n=$(cat "$state/runs" 2>/dev/null || echo 0); echo $((n+1)) > "$state/runs"
for last; do :; done
out=$(dirname "$last")
cp '`+initSrc+`' "$out/init.mp4"
printf 'seg0' > "$out/seg_0.m4s"
while [ ! -e "$state/go" ] && kill -0 "$PPID" 2>/dev/null; do sleep 0.01; done
i=1
while [ "$i" -lt `+itoa(segs)+` ]; do printf "seg$i" > "$out/seg_$i.m4s"; i=$((i+1)); done
`)
}

// waitProductions waits until h produces no window any more.
func waitProductions(t *testing.T, h *HLSHandler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.windows.mu.Lock()
		n := len(h.windows.m)
		h.windows.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a window is still being produced")
}

// The first segment of a window is served while ffmpeg is still encoding
// the rest, and the later ones as they are finished — not when the whole
// window is done; one ffmpeg run serves every request for the window.
func TestAWindowsSegmentsAreServedAsTheyAreFinished(t *testing.T) {
	captureLog(t)
	state := t.TempDir()
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: gatedFFmpeg(t, state, 10),
		FFprobeBin: probeFFprobe(t, probeJSON("matroska,webm", "hevc", 1920, 1080, "aac", 2, 8_000_000)),
		CacheDir:   t.TempDir(), TranscodePreset: "veryfast"}
	ctx := context.Background()
	source := &source{key: "i1", file: src, probe: &Probe{VideoCodec: "hevc", Width: 1920, Height: 1080, DurationMs: 600_000}}

	start := time.Now()
	if err := h.ensureVideoWindow(ctx, source, "high", QualityLadder["high"], 1, 10, 0); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("segment 10 took %s, with the window's ffmpeg still running", took)
	}
	if _, err := os.Stat(filepath.Join(state, "go")); err == nil {
		t.Fatal("the gate opened early")
	}
	if b, _ := os.ReadFile(h.cachePath("i1", "high", "10")); string(b) != "seg0" {
		t.Errorf("segment 10: %q", b)
	}
	// The next segment waits for ffmpeg to finish it, in the same run.
	got := make(chan error, 1)
	go func() { got <- h.ensureVideoWindow(ctx, source, "high", QualityLadder["high"], 1, 13, 0) }()
	select {
	case err := <-got:
		t.Fatalf("segment 13 before ffmpeg wrote it: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(state, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("segment 13 never came")
	}
	if b, _ := os.ReadFile(h.cachePath("i1", "high", "13")); string(b) != "seg3" {
		t.Errorf("segment 13: %q", b)
	}
	waitProductions(t, h)
	if b, _ := os.ReadFile(filepath.Join(state, "runs")); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("%s ffmpeg runs, want 1", b)
	}
	if !statOK(h.cachePath("i1", "high", "init")) || !statOK(h.cachePath("i1", "high", "19")) {
		t.Error("the window is not installed whole")
	}
}

// A production under way goes on after the request that started it has its
// segment (the player asks for the next ones), and ends when no request has
// asked for a segment of its window for windowIdle — killing its ffmpeg —
// without a charge against the window.
func TestAWindowNobodyAsksForAnyMoreIsGivenUp(t *testing.T) {
	captureLog(t)
	state := t.TempDir()
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: gatedFFmpeg(t, state, 10), CacheDir: t.TempDir()}
	h.windows.idle = 300 * time.Millisecond
	idle := h.windows.idle
	source := &source{key: "i1", file: src, probe: &Probe{VideoCodec: "hevc", Width: 1920, Height: 1080, DurationMs: 600_000}}
	if err := h.ensureVideoWindow(context.Background(), source, "high", QualityLadder["high"], 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	h.windows.mu.Lock()
	p := h.windows.m["vwin/i1/high/0"]
	h.windows.mu.Unlock()
	if p == nil {
		t.Fatal("the production ended with its first segment")
	}
	// Asked for again within its idle time: still wanted.
	time.Sleep(idle / 2)
	if err := h.ensureVideoWindow(context.Background(), source, "high", QualityLadder["high"], 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		t.Fatal("ended while wanted")
	case <-time.After(idle * 3 / 4):
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("not ended when nobody wanted it")
	}
	if rec, _ := h.windowFails.check("i1/high/0"); rec.count != 0 {
		t.Errorf("an abandoned window was charged: %+v", rec)
	}
}

// A warm's production runs to its end, wanted or not, on the warm slots.
func TestAWarmProductionRunsToItsEnd(t *testing.T) {
	captureLog(t)
	state := t.TempDir()
	root, src := mediaFile(t, "film.mkv")
	h := &HLSHandler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFmpegBin: gatedFFmpeg(t, state, 10), CacheDir: t.TempDir()}
	h.windows.idle = 50 * time.Millisecond
	source := &source{key: "i1", file: src, probe: &Probe{VideoCodec: "hevc", Width: 1920, Height: 1080, DurationMs: 600_000}}
	ctx, cancel := context.WithCancel(withWarmContext(context.Background()))
	if err := h.ensureVideoWindow(ctx, source, "high", QualityLadder["high"], 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(4 * h.windows.idle)
	var warm atomic.Bool
	h.windows.mu.Lock()
	warm.Store(h.windows.m["vwin/i1/high/0"] != nil)
	h.windows.mu.Unlock()
	if !warm.Load() {
		t.Fatal("a warm's production ended unfinished")
	}
	if err := os.WriteFile(filepath.Join(state, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.ensureVideoWindow(context.Background(), source, "high", QualityLadder["high"], 0, 9, 0); err != nil {
		t.Fatal(err)
	}
	waitProductions(t, h)
}
