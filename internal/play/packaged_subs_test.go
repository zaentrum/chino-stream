package play

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The WebVTT renditions of a package (hls/sN) are served: the media
// playlist, its URIs carrying the request's query like every other
// packaged playlist, and the segments as WebVTT, ranges and all. They are
// on disk whether or not the master references them (HLS_SUBTITLES only
// changes the master), so the routes serve them either way.
func TestPackagedSubtitleRenditionsAreServed(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	h := &HLSHandler{}
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln"
	for _, id := range []string{pkgLadder, pkgSingle} {
		w := get(h, "/api/play/"+id+"/s0/playlist.m3u8"+query)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
			t.Fatalf("%s playlist: %d %q %q", id, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		for _, seg := range []string{"seg-00001.vtt", "seg-00002.vtt", "seg-00003.vtt"} {
			if !strings.Contains(w.Body.String(), "\n"+seg+query+"\n") {
				t.Errorf("%s playlist lacks %s%s:\n%s", id, seg, query, w.Body)
			}
		}
		want, err := os.ReadFile(packagePath(id, "hls", "s0", "seg-00001.vtt"))
		if err != nil {
			t.Fatal(err)
		}
		w = get(h, "/api/play/"+id+"/s0/seg-00001.vtt"+query)
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || w.Body.String() != string(want) {
			t.Errorf("%s segment: %d %q %q", id, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		if !strings.HasPrefix(w.Body.String(), "WEBVTT\n") {
			t.Errorf("not a WebVTT file: %q", w.Body)
		}
	}

	// A range of a segment.
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", h.Routes)
	req := httptest.NewRequest(http.MethodGet, "/api/play/"+pkgLadder+"/s1/seg-00001.vtt", nil)
	req.Header.Set("Range", "bytes=0-5")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "WEBVTT" {
		t.Errorf("range: %d %q", w.Code, w.Body)
	}

	// What is not there, or not a subtitle rendition, is a 404.
	for _, path := range []string{
		"/s9/playlist.m3u8", "/s0/seg-00009.vtt", "/s0/init.mp4", "/s0/seg-00001.m4s",
		"/v0/seg-00001.vtt", "/a0/seg-00001.vtt", "/sx/playlist.m3u8", "/s/playlist.m3u8",
	} {
		if w := get(h, "/api/play/"+pkgLadder+path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}
