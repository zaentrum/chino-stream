package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zaentrum/chino-stream/internal/play"
)

// The router's request log (stdout) blanks out the stream token and the
// bearer of a request URL.
func TestRouterRequestLogRedactsCredentials(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = wr // NewRouter hands its request logger os.Stdout
	h, err := NewRouter(Deps{FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir()})
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz?stream=s3cr3t.s1g&token=eyJhbGciOiJIUzI1NiJ9.e30.x", nil))
	_ = wr.Close()
	out, _ := io.ReadAll(rd)
	if !strings.Contains(string(out), "/healthz?stream=REDACTED&token=REDACTED") || strings.Contains(string(out), "s3cr3t") ||
		strings.Contains(string(out), "eyJhbGci") {
		t.Errorf("request log: %q", out)
	}
}

// /readyz is up only while ffmpeg has an AAC encoder the on-the-fly
// pipelines can use; /healthz (liveness) is up regardless.
func TestReadyzNeedsAnAACEncoder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a shell script")
	}
	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"native aac", "echo ' A....D aac                  AAC (Advanced Audio Coding)'", http.StatusOK},
		{"no AAC encoder", "echo ' A....D ac3                  ATSC A/52A (AC-3)'", http.StatusServiceUnavailable},
		{"ffmpeg fails", "exit 1", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			h, err := NewRouter(Deps{FFmpegBin: ffmpeg, FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]int{"/readyz": tc.want, "/healthz": http.StatusOK} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != want {
					t.Errorf("%s: %d %q, want %d", path, w.Code, w.Body, want)
				}
			}
		})
	}
}

// With LIVE_WORKER_MODE on nothing of a new session is encoded here, so
// /readyz no longer waits for an AAC encoder; with the worker off or shadowed
// it does, as before — and the worker's own readiness never gates it (with
// the worker down only live encodes may fail).
func TestReadyzWithTheLiveWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a shell script")
	}
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	for mode, want := range map[play.LiveWorkerMode]int{
		play.LiveWorkerOn: http.StatusOK, play.LiveWorkerShadow: http.StatusServiceUnavailable, play.LiveWorkerOff: http.StatusServiceUnavailable,
	} {
		h, err := NewRouter(Deps{FFmpegBin: ffmpeg, FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir(), LiveWorkerURL: down.URL, LiveWorkerMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != want {
			t.Errorf("%s: /readyz %d %q, want %d", mode, w.Code, w.Body, want)
		}
	}
	// A mode without a worker to send anything to is no mode at all.
	h, err := NewRouter(Deps{FFmpegBin: ffmpeg, FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir(), LiveWorkerMode: play.LiveWorkerOn})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("on without a URL: /readyz %d", w.Code)
	}
}
