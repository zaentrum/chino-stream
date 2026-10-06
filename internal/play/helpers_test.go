package play

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// fakeBin writes an executable shell script standing in for ffmpeg or
// ffprobe and returns its path. body runs with the arguments the real binary
// would get.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binaries are shell scripts")
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// windowFFmpeg is a fake ffmpeg for the window transcoders. Each run appends
// its arguments (one per line, then an empty line) to <state>/args — in one
// write, so runs at the same time (a window's video and audio) do not
// interleave — and counts itself in <state>/runs. The first `fails` runs
// print `stderr` and exit 1; later runs write init.mp4 and `segs` segments
// next to the playlist (the last argument), as `ffmpeg -f hls` does.
func windowFFmpeg(t *testing.T, state string, fails, segs int, stderr string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "stderr"), []byte(stderr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fakeBin(t, "ffmpeg", `
state='`+state+`'
run=$(mktemp "$state/run.XXXXXX")
for a; do printf '%s\n' "$a"; done > "$run"
printf '\n' >> "$run"
cat "$run" >> "$state/args"
rm -f "$run"
n=$(cat "$state/runs" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$state/runs"
if [ "$n" -le `+itoa(fails)+` ]; then
  cat "$state/stderr" >&2
  exit 1
fi
for last; do :; done
out=$(dirname "$last")
printf 'init' > "$out/init.mp4"
i=0
while [ "$i" -lt `+itoa(segs)+` ]; do printf 'seg' > "$out/seg_$i.m4s"; i=$((i+1)); done
`)
}

// probeFFprobe is a fake ffprobe answering every probe with the JSON given.
func probeFFprobe(t *testing.T, probeJSON string) string {
	t.Helper()
	return fakeBin(t, "ffprobe", "cat <<'EOF'\n"+probeJSON+"\nEOF")
}

// ffmpegRuns returns the argument lists of the fake's runs so far.
func ffmpegRuns(t *testing.T, state string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "args"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs [][]string
	for _, block := range strings.Split(strings.TrimRight(string(b), "\n"), "\n\n") {
		if block != "" {
			runs = append(runs, strings.Split(block, "\n"))
		}
	}
	return runs
}

// argAfter returns the argument following flag in args ("" if absent).
func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }

// fakeKatalog is a katalog-api resolving every item to the asset at path.
func fakeKatalog(t *testing.T, path string) *catalog.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/asset") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"path": path, "isPrimary": true})
	}))
	t.Cleanup(srv.Close)
	return catalog.New(srv.URL)
}

// mediaFile creates an (empty) source file in a fresh media root and returns
// the root and the file's path.
func mediaFile(t *testing.T, name string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, path
}

// get runs one GET against h's routes, mounted as the router mounts them.
func get(h *HLSHandler, path string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", h.Routes)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// realFFmpeg returns the ffmpeg and ffprobe on $PATH, skipping the test when
// either is missing.
func realFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on $PATH")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on $PATH")
	}
	return ffmpeg, ffprobe
}

// run runs a command, failing the test with its output when it fails.
func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out
}
