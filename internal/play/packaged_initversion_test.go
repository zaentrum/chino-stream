package play

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A rendition's media playlist names its init segment with the init's own
// version, so an init changed in place after its package completed — a
// mend of its boxes, renamed into place — is asked for again by a player
// whose cache keeps the one before: the playlist served once the init
// changed names the new version, whatever the playlist cache held, and the
// segments' URIs carry the query alone.
func TestARenditionPlaylistNamesItsInitByItsVersion(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "playlist.m3u8")
	init := filepath.Join(dir, "init.mp4")
	body := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg-00001.m4s\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(pl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(content string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(init, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(init, at, at); err != nil {
			t.Fatal(err)
		}
	}
	serve := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		serveRenditionPlaylist(rec, httptest.NewRequest(http.MethodGet, "/p?stream=tok", nil), pl, nil)
		return rec.Body.String()
	}
	write("an init", time.Now().Add(-time.Hour))
	first := initVersion(init)
	got := serve()
	if !strings.Contains(got, `#EXT-X-MAP:URI="init.mp4?stream=tok&init=`+first+`"`) || !strings.Contains(got, "\nseg-00001.m4s?stream=tok\n") {
		t.Fatalf("the playlist: %q", got)
	}
	write("an INIT", time.Now()) // mended: same size, renamed into place
	second := initVersion(init)
	if second == first {
		t.Fatal("the init's version did not change with its bytes")
	}
	if got := serve(); !strings.Contains(got, "init="+second+`"`) || strings.Contains(got, first) {
		t.Errorf("the playlist once the init changed: %q", got)
	}
	// no init beside it: the playlist as it is, its URIs carrying the query
	if err := os.Remove(init); err != nil {
		t.Fatal(err)
	}
	if got := serve(); !strings.Contains(got, `URI="init.mp4?stream=tok"`) {
		t.Errorf("a playlist without its init: %q", got)
	}
}
