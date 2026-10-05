package play

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file packaged again at the same path is served as it is now, not as the
// cache last held it: the master, playlists and segments of a title encoded
// again, whatever was cached of its earlier package (pinned by the Zap pool
// or not), and the cache's byte count follows the new size.
func TestAFilePackagedAgainIsServedAsItIsNow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.m3u8")
	t.Cleanup(func() {
		if v, ok := packagedCache.LoadAndDelete(path); ok {
			packagedCacheBytes.Add(-int64(len(v.(*packagedCacheEntry).bytes)))
		}
		zapPinnedPaths.Delete(path)
	})
	write := func(body string, at time.Time) time.Time {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime()
	}
	zapPinnedPaths.Store(path, struct{}{})

	before := packagedCacheBytes.Load()
	t1 := write("#EXTM3U\n320x180\n", time.Now().Add(-time.Hour))
	if b, err := readPackagedBytes(path, t1); err != nil || string(b) != "#EXTM3U\n320x180\n" {
		t.Fatalf("first read: %q %v", b, err)
	}
	t2 := write("#EXTM3U\n1920x1080 1280x720 854x480\n", time.Now())
	if b, err := readPackagedBytes(path, t2); err != nil || string(b) != "#EXTM3U\n1920x1080 1280x720 854x480\n" {
		t.Fatalf("after the file was packaged again: %q %v, want the new file", b, err)
	}
	if got := packagedCacheBytes.Load() - before; got != int64(len("#EXTM3U\n1920x1080 1280x720 854x480\n")) {
		t.Errorf("the cache counts %d bytes for the path, want the new file's %d", got, len("#EXTM3U\n1920x1080 1280x720 854x480\n"))
	}

	// The static path (segments, init, trickplay) too.
	write("an old segment", time.Now().Add(-time.Minute))
	rec := httptest.NewRecorder()
	servePackagedStatic(rec, httptest.NewRequest(http.MethodGet, "/seg", nil), path, "video/mp4", "not found")
	write("a new segment", time.Now())
	rec = httptest.NewRecorder()
	servePackagedStatic(rec, httptest.NewRequest(http.MethodGet, "/seg", nil), path, "video/mp4", "not found")
	if rec.Body.String() != "a new segment" {
		t.Errorf("servePackagedStatic: %q, want the new file", rec.Body.String())
	}
}
