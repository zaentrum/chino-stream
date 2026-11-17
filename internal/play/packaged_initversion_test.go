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
// version; asked for by another, it is the bytes held in memory when they
// are that version — the version names the bytes — and else the bytes the
// init has now, read anew and served under no-store.
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
	old, mended := initVersionOf([]byte("an init")), initVersionOf([]byte("an INIT"))
	if body, cc := get(old); body != "an init" || !strings.Contains(cc, "immutable") {
		t.Errorf("the init asked for by its version: %q %q", body, cc)
	}
	write("an INIT") // mended in place, its size and time as they were
	if body, cc := get(old); body != "an init" || !strings.Contains(cc, "immutable") {
		t.Errorf("the old version, its bytes in memory: %q %q", body, cc)
	}
	if body, cc := get(mended); body != "an INIT" || !strings.Contains(cc, "immutable") {
		t.Errorf("the mended init asked for by its version: %q %q", body, cc)
	}
	if body, cc := get(old); body != "an INIT" || cc != "no-store" {
		t.Errorf("the mended init asked for by the old version, the new bytes in memory: %q %q", body, cc)
	}
}

// An init's version is kept while the file is the one it was, and the
// versioned init is served from memory while the bytes held there are that
// version: the init is read once, however often the playlist and the init
// are asked for. One renamed into place with the same size and
// modification time is another file, and named anew; the playlist cache
// keeps no entry of the version before. What EXT-X-MAP names that is too
// large to be an init is named as it is.
func TestAnInitsVersionIsKeptWhileItIsTheSameFile(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "playlist.m3u8")
	init := filepath.Join(dir, "init.mp4")
	body := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg-00001.m4s\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(pl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	place := func(content string) {
		t.Helper()
		tmp := init + ".1-ab.mending"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, init); err != nil {
			t.Fatal(err)
		}
	}
	serve := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		serveRenditionPlaylist(rec, httptest.NewRequest(http.MethodGet, "/p?stream=tok", nil), pl, nil)
		return rec.Body.String()
	}
	get := func(version string) (int, string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		serveInitSegment(rec, httptest.NewRequest(http.MethodGet, "/init.mp4?stream=tok&init="+version, nil), init, nil)
		return rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control")
	}
	var reads int
	t.Cleanup(func() { readInit = os.ReadFile })
	readInit = func(p string) ([]byte, error) { reads++; return os.ReadFile(p) }
	place("an init")
	first := initVersionOf([]byte("an init"))
	if got := serve(); !strings.Contains(got, "init="+first+`"`) || reads != 1 {
		t.Fatalf("the playlist: %q, the init read %d times", got, reads)
	}
	for i := 0; i < 3; i++ {
		if got := serve(); !strings.Contains(got, "init="+first+`"`) {
			t.Errorf("the playlist again: %q", got)
		}
		if code, body, cc := get(first); code != 200 || body != "an init" || !strings.Contains(cc, "immutable") {
			t.Errorf("the init asked for by its version: %d %q %q", code, body, cc)
		}
	}
	if reads != 1 {
		t.Errorf("the init was read %d times while it was the same file", reads)
	}
	place("an INIT") // mended: renamed into place, the same size and modification time
	second := initVersionOf([]byte("an INIT"))
	if got := serve(); !strings.Contains(got, "init="+second+`"`) {
		t.Errorf("the playlist once the init was renamed into place: %q", got)
	}
	if _, ok := playlistCache.Load(pl + "?stream=tok#init=init.mp4=" + first + ";"); ok {
		t.Errorf("the playlist cache keeps the entry of the version before")
	}
	if code, body, cc := get(first); code != 200 || body != "an INIT" || cc != "no-store" {
		t.Errorf("the init asked for by the version before: %d %q %q", code, body, cc)
	}
	// too large to be an init: named as it is
	big := make([]byte, maxInitBytes+1)
	if err := os.WriteFile(init, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := serve(); !strings.Contains(got, `URI="init.mp4?stream=tok"`) {
		t.Errorf("a playlist whose EXT-X-MAP names a file too large to be an init: %q", got)
	}
}
