package play

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// A session is pinned to the version it started on: the master's URIs carry
// v=<version>, and while katalog-api still lists that version — the current
// one or a previous one — every playlist and segment of the session is
// served from it, also after a newer version superseded it. A new session
// starts on the new version. Once the old version is removed (katalog-api no
// longer lists it, its folder gone), a request pinned to it is served the
// current version, as is one pinned to a version katalog-api never knew.
func TestASessionStaysOnItsVersionAcrossASupersede(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libOld))})
	h := &HLSHandler{Packages: l.packages}
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,hvc,aac"
	url := func(route string) string { return "/api/play/" + libItem + "/" + route }

	// A session starts on the old version.
	w := get(h, url("master.m3u8"+query))
	if w.Code != 200 {
		t.Fatalf("master: %d %q", w.Code, w.Body)
	}
	pinOld := query + "&v=" + libOld
	if !strings.Contains(w.Body.String(), "\nv0/playlist.m3u8"+pinOld+"\n") || !strings.Contains(w.Body.String(), `URI="a0/playlist.m3u8`+pinOld+`"`) {
		t.Fatalf("the master's URIs do not pin the version:\n%s", w.Body)
	}
	playlist := get(h, url("v0/playlist.m3u8"+pinOld)).Body.String()
	if !strings.Contains(playlist, "\nseg-00001.m4s"+pinOld+"\n") || !strings.Contains(playlist, `#EXT-X-MAP:URI="init.mp4`+pinOld+`"`) {
		t.Fatalf("the media playlist's URIs do not carry the pin on:\n%s", playlist)
	}

	// A newer version completes and supersedes it.
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew)), Previous: []catalog.PackageRef{l.ref(libOld)}})
	now = now.Add(resolveFresh + time.Second)
	get(h, url("v0/init.mp4"+pinOld)) // the stale answer, its revalidation in the background
	waitLookups(t, l.packages)

	for route, want := range map[string]string{
		"v0/init.mp4":      "a1a1a1a1 init.mp4",
		"v0/seg-00004.m4s": "a1a1a1a1 seg-00004.m4s",
		"v1/seg-00002.m4s": "a1a1a1a1 seg-00002.m4s", // a rung only the old version has
		"a0/seg-00001.m4s": "a1a1a1a1 seg-00001.m4s",
	} {
		if w := get(h, url(route+pinOld)); w.Code != 200 || w.Body.String() != want {
			t.Errorf("the pinned session's %s: %d %q, want %q", route, w.Code, w.Body, want)
		}
	}
	if w := get(h, url("s0/seg-00001.vtt"+pinOld)); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "WEBVTT") {
		t.Errorf("the pinned session's subtitles: %d", w.Code)
	}

	// A new session starts on the new version.
	w = get(h, url("master.m3u8"+query))
	pinNew := query + "&v=" + libNew
	if !strings.Contains(w.Body.String(), "\nv0/playlist.m3u8"+pinNew+"\n") || strings.Contains(w.Body.String(), "TYPE=SUBTITLES") {
		t.Errorf("a new session's master is not the new version's:\n%s", w.Body)
	}
	if w := get(h, url("v0/seg-00001.m4s"+pinNew)); w.Body.String() != "b2b2b2b2 seg-00001.m4s" {
		t.Errorf("the new session's segment: %q", w.Body)
	}
	// Without a pin (a URL from before pinning): the current version.
	if w := get(h, url("v0/seg-00001.m4s"+query)); w.Body.String() != "b2b2b2b2 seg-00001.m4s" {
		t.Errorf("unpinned: %q", w.Body)
	}

	// The grace is over: the old version is removed.
	if err := os.RemoveAll(l.version(libOld)); err != nil {
		t.Fatal(err)
	}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew))})
	now = now.Add(resolveFresh + time.Second)
	get(h, url("v0/init.mp4"+pinNew))
	waitLookups(t, l.packages)
	for _, pin := range []string{libOld, "c0ffee00-0000-4000-8000-000000000000", "garbage"} {
		if w := get(h, url("v0/seg-00001.m4s"+query+"&v="+pin)); w.Code != 200 || w.Body.String() != "b2b2b2b2 seg-00001.m4s" {
			t.Errorf("pinned to %s, not listed: %d %q, want the current version", pin, w.Code, w.Body)
		}
	}
}

// A master asked for with a pin of its own (a reload) is the pinned
// version's while it is listed, its URIs carrying that pin once.
func TestAMasterAskedForWithAPinIsThatVersions(t *testing.T) {
	l := stageLibrary(t)
	h := &HLSHandler{Packages: l.packages}
	w := get(h, "/api/play/"+libItem+"/master.m3u8?caps=avc,hvc,aac&v="+libOld+"&stream=x")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "TYPE=SUBTITLES") ||
		!strings.Contains(body, "\nv0/playlist.m3u8?caps=avc,hvc,aac&stream=x&v="+libOld+"\n") || strings.Count(body, "v="+libOld) != strings.Count(body, "v=") {
		t.Errorf("%d\n%s", w.Code, body)
	}
}

func TestPinnedQuery(t *testing.T) {
	const v = "b2b2b2b2-0000-4000-8000-0000000000b2"
	for _, tc := range []struct{ raw, version, want string }{
		{"", v, "v=" + v},
		{"stream=a.b&caps=avc,hvc&q=auto", v, "stream=a.b&caps=avc,hvc&q=auto&v=" + v},
		{"v=old&stream=a.b", v, "stream=a.b&v=" + v},
		{"stream=a.b&v=old&v=older&v", v, "stream=a.b&v=" + v},
		{"stream=a.b&vv=1&video=2", v, "stream=a.b&vv=1&video=2&v=" + v},
		// A package from before the library: as it came.
		{"stream=a.b&v=old", "", "stream=a.b&v=old"},
		{"", "", ""},
	} {
		if got := pinnedQuery(tc.raw, tc.version); got != tc.want {
			t.Errorf("pinnedQuery(%q, %q) = %q, want %q", tc.raw, tc.version, got, tc.want)
		}
	}
}
