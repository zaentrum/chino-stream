package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
