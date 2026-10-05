package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// extraTrailer is a trailer of pkgStereoLadder under testdata/packages/extras,
// as the packager's extras mode lays it out with EXTRA_LADDER=720p:h264,
// 480p:h264: pkgStereoLadder's H.264 rungs, v0 720p and v1 480p, and its
// English stereo track, their media playlists as the packager wrote them
// there; the master as the packager assembles it for them; a manifest naming
// the title the extra belongs to (parentId) and its kind (extraKind), without
// trickplay.
const extraTrailer = "7a11e700-0000-4000-8000-000000000009"

// extraURL is the path of route under the extra extra of the title item.
func extraURL(item, extra, route string) string {
	return "/api/play/" + item + "/extras/" + extra + "/" + route
}

// An extra's master is the client's share of its ladder, as an item's is, its
// URIs carrying the request's query (the stream token, caps, q) so every
// rendition fetch is authorised and answered for the same client. Whatever
// the client says it decodes, the master is the package's: nothing is made
// on the fly for an extra (the handler has neither a catalog nor an ffmpeg).
func TestExtraMasterIsTheClientsShareOfItsLadder(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	h := &HLSHandler{}
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,aac"
	w := get(h, extraURL(pkgStereoLadder, extraTrailer, "master.m3u8"+query))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("master: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	body := w.Body.String()
	if got := servedVariants(body); !reflect.DeepEqual(got, []string{"v0/audio", "v1/audio"}) {
		t.Errorf("variants %v:\n%s", got, body)
	}
	for _, uri := range []string{"\nv0/playlist.m3u8" + query + "\n", "\nv1/playlist.m3u8" + query + "\n",
		`URI="a0/playlist.m3u8` + query + `"`, `URI="v0/iframes.m3u8` + query + `"`, `URI="v1/iframes.m3u8` + query + `"`} {
		if !strings.Contains(body, uri) {
			t.Errorf("master lacks %q:\n%s", uri, body)
		}
	}

	both := []string{"v0/audio", "v1/audio"}
	for _, tc := range []struct {
		name, query string
		variants    []string
	}{
		{"H.264 capped at 480: the 480p rung", "?caps=avc:480,aac", []string{"v1/audio"}},
		{"a quality pick", "?caps=avc,aac&q=v0", []string{"v0/audio"}},
		{"no caps (the default set): H.264", "", both},
		{"an HEVC client: the H.264 ladder", "?caps=avc,hvc,aac,eac3", both},
		{"a client that says it decodes no H.264: as packaged", "?caps=hvc,aac", both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(h, extraURL(pkgStereoLadder, extraTrailer, "master.m3u8"+tc.query))
			if w.Code != 200 {
				t.Fatalf("master: %d %q", w.Code, w.Body)
			}
			if got := servedVariants(w.Body.String()); !reflect.DeepEqual(got, tc.variants) {
				t.Errorf("variants %v, want %v:\n%s", got, tc.variants, w.Body)
			}
			var rungs []string
			for _, v := range tc.variants {
				rungs = append(rungs, strings.TrimSuffix(v, "/audio"))
			}
			if got := servedIFrames(w.Body.String()); !reflect.DeepEqual(got, rungs) {
				t.Errorf("I-frame playlists %v, want %v", got, rungs)
			}
		})
	}
}

// An extra's renditions are served as an item's are: the media playlists,
// their URIs carrying the request's query, the init segments and the media
// segments (ranges and all), the I-frame playlists, and a WebVTT rendition
// where the package has one. What the package has not, and the routes of an
// item that an extra has none of (trickplay, /info), are 404.
func TestExtraRenditionsAreServed(t *testing.T) {
	stagePackages(t, nil, extraTrailer)
	h := &HLSHandler{}
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,aac"
	url := func(route string) string { return extraURL(pkgStereoLadder, extraTrailer, route) }
	for _, rend := range []string{"v0", "v1", "a0"} {
		w := get(h, url(rend+"/playlist.m3u8"+query))
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
			t.Fatalf("%s playlist: %d %q %q", rend, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		for _, uri := range []string{`#EXT-X-MAP:URI="init.mp4` + query + `"`, "\nseg-00001.m4s" + query + "\n", "\nseg-00004.m4s" + query + "\n"} {
			if !strings.Contains(w.Body.String(), uri) {
				t.Errorf("%s playlist lacks %q:\n%s", rend, uri, w.Body)
			}
		}
		for file, ctype := range map[string]string{"init.mp4": "video/mp4", "seg-00001.m4s": "video/iso.segment", "seg-00004.m4s": "video/iso.segment"} {
			w := get(h, url(rend+"/"+file+query))
			if w.Code != 200 || w.Header().Get("Content-Type") != ctype || w.Body.String() != "media "+file {
				t.Errorf("%s/%s: %d %q %q", rend, file, w.Code, w.Header().Get("Content-Type"), w.Body)
			}
		}
	}
	for _, rend := range []string{"v0", "v1"} {
		w := get(h, url(rend+"/iframes.m3u8"+query))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "#EXT-X-I-FRAMES-ONLY") ||
			!strings.Contains(w.Body.String(), "\nseg-00001.m4s"+query+"\n") {
			t.Errorf("%s I-frames: %d %q", rend, w.Code, w.Body)
		}
	}

	// A range of a segment.
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", h.Routes)
	req := httptest.NewRequest(http.MethodGet, url("v0/seg-00002.m4s"), nil)
	req.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "media" {
		t.Errorf("range: %d %q", w.Code, w.Body)
	}

	// A WebVTT rendition, as an extra with a subtitle track has one.
	root := filepath.Join(PackagesRoot, "extras", extraTrailer[:2], extraTrailer, "hls", "s0")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const cue = "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello.\n"
	for name, body := range map[string]string{
		"playlist.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.000,\nseg-00001.vtt\n#EXT-X-ENDLIST\n",
		"seg-00001.vtt": cue,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if w := get(h, url("s0/playlist.m3u8"+query)); w.Code != 200 || !strings.Contains(w.Body.String(), "\nseg-00001.vtt"+query+"\n") {
		t.Errorf("s0 playlist: %d %q", w.Code, w.Body)
	}
	if w := get(h, url("s0/seg-00001.vtt"+query)); w.Code != 200 || w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || w.Body.String() != cue {
		t.Errorf("s0 segment: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}

	for _, route := range []string{
		"v2/playlist.m3u8", "a1/init.mp4", "v0/seg-00009.m4s", "v1/seg-00001.vtt", "s0/init.mp4", "s1/seg-00001.vtt",
		"a0/iframes.m3u8", "trickplay/thumbnails.vtt", "trickplay/sprite-1.jpg", "info", "master.m3u8/x",
	} {
		if w := get(h, url(route)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", route, w.Code)
		}
	}
}

// An extra plays only under its own title, once it is complete: every route
// of it is 404 under another title (a packaged one, one without a package,
// the extra's own id), while its package is not complete (.complete
// missing), when its manifest names no title or cannot be read, for an id
// that is no UUID, and for the id of an item's package.
func TestAnExtraIsServedOnlyUnderItsTitle(t *testing.T) {
	routes := []string{"master.m3u8", "v0/playlist.m3u8", "a0/playlist.m3u8", "v0/iframes.m3u8", "v0/init.mp4",
		"v0/seg-00001.m4s", "a0/seg-00004.m4s"}
	h := &HLSHandler{}
	codes := func(item, extra string) map[string]int {
		out := map[string]int{}
		for _, route := range routes {
			out[route] = get(h, extraURL(item, extra, route)).Code
		}
		return out
	}
	all := func(code int) map[string]int {
		out := map[string]int{}
		for _, route := range routes {
			out[route] = code
		}
		return out
	}
	check := func(t *testing.T, item, extra string, want int) {
		t.Helper()
		if got := codes(item, extra); !reflect.DeepEqual(got, all(want)) {
			t.Errorf("/api/play/%s/extras/%s: %v, want %d", item, extra, got, want)
		}
	}

	stagePackages(t, nil, extraTrailer)
	check(t, pkgStereoLadder, extraTrailer, http.StatusOK)
	for _, other := range []string{pkgLadder, "00000000-0000-4000-8000-000000000000", extraTrailer, strings.ToUpper(pkgStereoLadder)} {
		check(t, other, extraTrailer, http.StatusNotFound)
	}
	for _, id := range []string{
		"7", "7a11e700", "7a11e700-0000-4000-8000-00000000000", "7a11e700-0000-4000-8000-0000000000091",
		"7a11e700-0000-4000-8000-00000000000g", "7a11e700_0000_4000_8000_000000000009", "..",
		"..%2F..%2Fmovies%2F57%2F" + pkgStereoLadder, pkgStereoLadder,
	} {
		check(t, pkgStereoLadder, id, http.StatusNotFound)
	}

	// Not complete: its package is in place, its .complete not yet.
	if err := os.Remove(filepath.Join(PackagesRoot, "extras", extraTrailer[:2], extraTrailer, ".complete")); err != nil {
		t.Fatal(err)
	}
	check(t, pkgStereoLadder, extraTrailer, http.StatusNotFound)

	// A manifest that names no title.
	stagePackages(t, func(m map[string]any) { delete(m, "parentId") }, extraTrailer)
	check(t, pkgStereoLadder, extraTrailer, http.StatusNotFound)

	// A manifest that names another title, rewritten in place: the new one
	// counts, not the one read before.
	stagePackages(t, nil, extraTrailer)
	check(t, pkgStereoLadder, extraTrailer, http.StatusOK)
	mfPath := filepath.Join(PackagesRoot, "extras", extraTrailer[:2], extraTrailer, "manifest.json")
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["parentId"] = pkgLadder
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mfPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute) // a new mtime, whatever the file system's resolution
	if err := os.Chtimes(mfPath, future, future); err != nil {
		t.Fatal(err)
	}
	check(t, pkgStereoLadder, extraTrailer, http.StatusNotFound)
	check(t, pkgLadder, extraTrailer, http.StatusOK)

	// A manifest that cannot be read.
	if err := os.WriteFile(mfPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(mfPath, future.Add(time.Minute), future.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	check(t, pkgLadder, extraTrailer, http.StatusNotFound)
}

// An extra is no item: its package is not among the packaged ids (the Zap
// pager's instant-start set) nor in the Zap pool, and the item routes of its
// id find no package of it — the master asks the catalog, which knows no
// such item.
func TestExtrasAreNoItems(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	ids := walkCompletedPackageIDs()
	sort.Strings(ids)
	want := []string{pkgLegacy, pkgLegacyHDR, pkgLegacyHDRNoRange, pkgLadder, pkgHEVCLadder, pkgUHD, pkgSingle, pkgStereoLadder}
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("packaged ids %v, want the items' %v", ids, want)
	}
	if HasCompletedPackage(extraTrailer) || itemRoot(extraTrailer) != "" {
		t.Errorf("the extra's package is found as an item's")
	}
	if mf, err := ReadPackageManifest(extraTrailer); err == nil {
		t.Errorf("the extra's manifest is read as an item's: %+v", mf)
	}
	if e := warmOneZapItem(nil, extraTrailer); e != nil {
		t.Errorf("the extra is a Zap pool entry: %+v", e)
	}

	katalog := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(katalog.Close)
	h := &HLSHandler{Catalog: catalog.New(katalog.URL)}
	for _, route := range []string{"master.m3u8", "v0/playlist.m3u8", "v0/init.mp4", "v0/seg-00001.m4s"} {
		if w := get(h, "/api/play/"+extraTrailer+"/"+route); w.Code != http.StatusNotFound {
			t.Errorf("/api/play/%s/%s: %d %q, want 404", extraTrailer, route, w.Code, w.Body)
		}
	}
}
