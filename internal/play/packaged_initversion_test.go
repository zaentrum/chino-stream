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
// version, a hash of its bytes, so an init changed in place after its
// package completed — a mend of its boxes, renamed into place, its size and
// its modification time as they were — is asked for again by a player
// whose cache keeps the one before: the playlist served once the init
// changed names the new version, whatever the playlist cache held, and the
// segments' URIs carry the query alone. The I-frame playlist names it so
// too, an init the playlist names by another name is versioned by its own
// bytes, and a playlist none of whose inits is there is served as it is.
func TestARenditionPlaylistNamesItsInitByItsVersion(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "playlist.m3u8")
	init := filepath.Join(dir, "init.mp4")
	body := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg-00001.m4s\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(pl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	serve := func(path string, iframes bool) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/p?stream=tok", nil)
		if iframes {
			serveIframesPlaylist(rec, req, path, nil)
		} else {
			serveRenditionPlaylist(rec, req, path, nil)
		}
		return rec.Body.String()
	}
	write("init.mp4", "an init")
	first := initVersionOf([]byte("an init"))
	got := serve(pl, false)
	if !strings.Contains(got, `#EXT-X-MAP:URI="init.mp4?stream=tok&init=`+first+`"`) || !strings.Contains(got, "\nseg-00001.m4s?stream=tok\n") {
		t.Fatalf("the playlist: %q", got)
	}
	write("init.mp4", "an INIT") // mended: same size, the same modification time
	second := initVersionOf([]byte("an INIT"))
	if got := serve(pl, false); !strings.Contains(got, "init="+second+`"`) || strings.Contains(got, first) {
		t.Errorf("the playlist once the init changed: %q", got)
	}
	// the I-frame playlist names it by its version too
	ifr := filepath.Join(dir, "iframes.m3u8")
	if err := os.WriteFile(ifr, []byte(strings.Replace(body, "#EXTINF", "#EXT-X-I-FRAMES-ONLY\n#EXTINF", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := serve(ifr, true); !strings.Contains(got, `#EXT-X-MAP:URI="init.mp4?stream=tok&init=`+second+`"`) {
		t.Errorf("the I-frame playlist: %q", got)
	}
	// an init the playlist names by another name: that file's version
	other := filepath.Join(dir, "other.m3u8")
	if err := os.WriteFile(other, []byte(strings.Replace(body, `URI="init.mp4"`, `URI="init-v2.mp4"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	write("init-v2.mp4", "another init")
	if got := serve(other, false); !strings.Contains(got, `URI="init-v2.mp4?stream=tok&init=`+initVersionOf([]byte("another init"))+`"`) {
		t.Errorf("a playlist naming init-v2.mp4: %q", got)
	}
	// no init beside it: the playlist as it is, its URIs carrying the query
	if err := os.Remove(init); err != nil {
		t.Fatal(err)
	}
	if got := serve(pl, false); !strings.Contains(got, `URI="init.mp4?stream=tok"`) {
		t.Errorf("a playlist without its init: %q", got)
	}
}

// An init asked for by its version is served immutable while it is that
// version, its bytes read anew; asked for by another — a playlist from
// before a mend — it is served as it is now, under no-store.
func TestAnInitAskedForByAnOldVersionIsNotCached(t *testing.T) {
	dir := t.TempDir()
	init := filepath.Join(dir, "init.mp4")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(init, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(init, at, at); err != nil {
			t.Fatal(err)
		}
	}
	get := func(version string) (string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		serveInitSegment(rec, httptest.NewRequest(http.MethodGet, "/init.mp4?stream=tok&init="+version, nil), init, nil)
		return rec.Body.String(), rec.Header().Get("Cache-Control")
	}
	write("an init")
	old := initVersionOf([]byte("an init"))
	if body, cc := get(old); body != "an init" || !strings.Contains(cc, "immutable") {
		t.Errorf("the init asked for by its version: %q %q", body, cc)
	}
	write("an INIT") // mended in place, its size and time as they were
	if body, cc := get(old); body != "an INIT" || cc != "no-store" {
		t.Errorf("the mended init asked for by the old version: %q %q", body, cc)
	}
	if body, cc := get(initVersionOf([]byte("an INIT"))); body != "an INIT" || !strings.Contains(cc, "immutable") {
		t.Errorf("the mended init asked for by its version: %q %q", body, cc)
	}
}
