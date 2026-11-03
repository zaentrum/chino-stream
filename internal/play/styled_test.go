package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// A synthetic styled script: CRLF line ends, a style and an override tag, as
// the package keeps it.
const assSample = "[Script Info]\r\nScriptType: v4.00+\r\n\r\n[V4+ Styles]\r\n" +
	"Format: Name, Fontname, Fontsize\r\nStyle: Default,Example Sans,48\r\n\r\n[Events]\r\n" +
	"Format: Layer, Start, End, Style, Text\r\n" +
	"Dialogue: 0,0:00:01.00,0:00:02.00,Default,{\\fnExample Serif}A line.\r\n"

const (
	styledItem  = "f001aeff-9c18-4183-b51b-51403af2515e"
	otherItem   = "a7000000-0000-4000-8000-000000000002"
	styledV1    = "9a2e0000-0000-4000-8000-000000000001"
	styledV2    = "9a2e0000-0000-4000-8000-000000000002"
	otherV1     = "9a2e0000-0000-4000-8000-0000000000b1"
	styledStore = "store"
)

// styledRouter is a router of h's routes for the styled originals and the
// fonts, beside the subtitles' .vtt route, as the router mounts them.
func styledRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/play/subs/{subID}.vtt", h.SidecarSubtitle)
	r.Get("/api/play/subs/{subID}.ass", h.StyledSubtitle)
	for _, ext := range []string{"ttf", "otf", "ttc", "woff", "woff2", "bin"} {
		r.Get("/api/play/fonts/{fontID}."+ext, h.Font)
	}
	return r
}

// writeFiles writes each file of files (path → content), its folders made.
func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// styledKatalog is a katalog-api answering the asset lookups of assets (its
// path, its id → answer) and the playback lookup of each item of packages
// (item → the package folder it plays), which a test may change.
type styledKatalog struct {
	*httptest.Server
	mu       sync.Mutex
	packages map[string]string
}

func newStyledKatalog(t *testing.T, assets map[string]map[string]any, packages map[string]string) *styledKatalog {
	t.Helper()
	k := &styledKatalog{packages: packages}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if item, ok := strings.CutPrefix(r.URL.Path, "/api/v1/items/"); ok && strings.HasSuffix(item, "/playback") {
			item = strings.TrimSuffix(item, "/playback")
			k.mu.Lock()
			dir, ok := k.packages[item]
			k.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"itemId": item, "type": "movie",
				"package":  map[string]any{"versionId": filepath.Base(dir), "dir": dir, "record": "package.json"},
				"previous": []any{}, "original": nil})
			return
		}
		a, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *styledKatalog) plays(item, dir string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.packages[item] = dir
}

// fetch is a GET of path from r, with the header given.
func fetch(r http.Handler, path string, header http.Header) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	r.ServeHTTP(rec, req)
	return rec
}

// A subtitle's styled original is served at its .ass URL byte for byte, as
// the text an ASS renderer reads, never sniffed for another, cached for good
// by the browser alone; its .vtt URL serves the WebVTT file as before. It is
// served only from the subs/ of a package of its own title, every link
// followed: a script beside the package's subs/, in another title's
// package, outside the roots or behind a link that leaves the package is
// refused. A subtitle without one is 404 there, a format the stream service
// does not know a 502, and a script gone a 404.
func TestAStyledOriginalIsServedFromItsTitlesPackage(t *testing.T) {
	share := t.TempDir()
	pkg := filepath.Join(share, "movies", "f0", styledItem, "versions", styledV1)
	other := filepath.Join(share, "movies", "a7", otherItem, "versions", otherV1)
	subs := filepath.Join(pkg, "subs")
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nA line.\n"
	outside := filepath.Join(t.TempDir(), "0.ass")
	writeFiles(t, map[string]string{filepath.Join(subs, "0.vtt"): vtt, filepath.Join(subs, "0.ass"): assSample,
		filepath.Join(subs, "1.vtt"): vtt, outside: assSample, filepath.Join(pkg, "hls", "0.ass"): assSample,
		filepath.Join(other, "subs", "0.ass"): assSample})
	if err := os.Symlink(outside, filepath.Join(subs, "away.ass")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(subs, "0.ass"), filepath.Join(subs, "here.ass")); err != nil {
		t.Fatal(err)
	}
	row := func(styled, format string) map[string]any {
		return map[string]any{"itemId": styledItem, "path": filepath.Join(subs, "0.vtt"), "format": "webvtt",
			"styledPath": styled, "styledFormat": format}
	}
	k := newStyledKatalog(t, map[string]map[string]any{
		"/api/v1/subtitles/styled/asset":  row(filepath.Join(subs, "0.ass"), "ass"),
		"/api/v1/subtitles/linked/asset":  row(filepath.Join(subs, "here.ass"), "ass"),
		"/api/v1/subtitles/plain/asset":   {"itemId": styledItem, "path": filepath.Join(subs, "1.vtt"), "format": "webvtt"},
		"/api/v1/subtitles/outside/asset": row(outside, "ass"),
		"/api/v1/subtitles/beside/asset":  row(filepath.Join(pkg, "hls", "0.ass"), "ass"),
		"/api/v1/subtitles/another/asset": row(filepath.Join(other, "subs", "0.ass"), "ass"),
		"/api/v1/subtitles/away/asset":    row(filepath.Join(subs, "away.ass"), "ass"),
		"/api/v1/subtitles/unknown/asset": row(filepath.Join(subs, "0.ass"), "usf"),
		"/api/v1/subtitles/gone/asset":    row(filepath.Join(subs, "9.ass"), "ass"),
	}, map[string]string{styledItem: pkg, otherItem: other})
	c := catalog.New(k.URL)
	r := styledRouter(&Handler{Catalog: c, Packages: NewResolver(c), MediaRoot: share})
	w := fetch(r, "/api/play/subs/styled.ass", nil)
	if w.Code != http.StatusOK || w.Body.String() != assSample {
		t.Fatalf("styled .ass: %d %q", w.Code, w.Body)
	}
	for k, want := range map[string]string{"Content-Type": "text/x-ssa; charset=utf-8", "X-Content-Type-Options": "nosniff",
		"Cache-Control": "private, max-age=31536000, immutable", "Access-Control-Allow-Origin": "*"} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("styled .ass %s: %q, want %q", k, got, want)
		}
	}
	if w := fetch(r, "/api/play/subs/styled.vtt", nil); w.Code != http.StatusOK || w.Body.String() != vtt ||
		w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("styled .vtt: %d %q %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"), w.Body)
	}
	if w := fetch(r, "/api/play/subs/linked.ass", nil); w.Code != http.StatusOK || w.Body.String() != assSample {
		t.Errorf("a link to a script of the same package: %d %q", w.Code, w.Body)
	}
	for id, want := range map[string]int{"plain": http.StatusNotFound, "nope": http.StatusNotFound,
		"outside": http.StatusForbidden, "beside": http.StatusForbidden, "another": http.StatusForbidden,
		"away": http.StatusForbidden, "unknown": http.StatusBadGateway, "gone": http.StatusNotFound} {
		if w := fetch(r, "/api/play/subs/"+id+".ass", nil); w.Code != want {
			t.Errorf("%s .ass: %d %q, want %d", id, w.Code, w.Body, want)
		}
	}
}

// A package's font is served at its URL whatever its extension says, as the
// catalog's format names it (a font of no format as bytes), never sniffed
// for another, from the subs/fonts/ of a package of its own title — a
// version's under the media root, or the package store's — every link
// followed; cached for a day by the browser alone, and answered 304 while
// unchanged. A font of a title packaged anew is found in its new package
// though the answer the stream service kept names the one before. An id of
// no font is 404; a font beside subs/fonts/, in another title's package,
// outside the roots or behind a link that leaves the package is refused; a
// format the stream service does not know is a 502.
func TestAPackagesFontIsServedFromItsTitlesPackage(t *testing.T) {
	share := t.TempDir()
	store := filepath.Join(t.TempDir(), "packages")
	old := legacyPackageStore
	legacyPackageStore = store
	t.Cleanup(func() { legacyPackageStore = old })
	pkg := filepath.Join(share, "movies", "f0", styledItem, "versions", styledV1)
	next := filepath.Join(share, "movies", "f0", styledItem, "versions", styledV2)
	other := filepath.Join(share, "movies", "a7", otherItem, "versions", otherV1)
	stored := filepath.Join(store, "movies", "a7", otherItem)
	fonts := filepath.Join(pkg, "subs", "fonts")
	outside := filepath.Join(t.TempDir(), "0.ttf")
	files := map[string]string{
		filepath.Join(fonts, "0.ttf"): "\x00\x01\x00\x00 a ttf", filepath.Join(fonts, "1.otf"): "OTTO an otf, longer",
		filepath.Join(fonts, "2.ttc"): "ttcf a ttc", filepath.Join(fonts, "3.woff"): "wOFF a woff",
		filepath.Join(fonts, "4.woff2"): "wOF2 a woff2", filepath.Join(fonts, "5.bin"): "some bytes",
		filepath.Join(next, "subs", "fonts", "0.ttf"):   "\x00\x01\x00\x00 the new package's",
		filepath.Join(pkg, "subs", "6.ttf"):             "\x00\x01\x00\x00 beside the fonts",
		filepath.Join(other, "subs", "fonts", "0.ttf"):  "\x00\x01\x00\x00 another title's",
		filepath.Join(stored, "subs", "fonts", "0.otf"): "OTTO the store's", outside: "\x00\x01\x00\x00 outside",
	}
	writeFiles(t, files)
	if err := os.Symlink(outside, filepath.Join(fonts, "away.ttf")); err != nil {
		t.Fatal(err)
	}
	asset := func(path, format string) map[string]any {
		a := map[string]any{"itemId": styledItem, "path": path}
		if format != "" {
			a["format"] = format
		}
		return a
	}
	k := newStyledKatalog(t, map[string]map[string]any{
		"/api/v1/fonts/ttf/asset": asset(filepath.Join(fonts, "0.ttf"), "ttf"), "/api/v1/fonts/otf/asset": asset(filepath.Join(fonts, "1.otf"), "otf"),
		"/api/v1/fonts/ttc/asset": asset(filepath.Join(fonts, "2.ttc"), "ttc"), "/api/v1/fonts/woff/asset": asset(filepath.Join(fonts, "3.woff"), "woff"),
		"/api/v1/fonts/woff2/asset": asset(filepath.Join(fonts, "4.woff2"), "woff2"), "/api/v1/fonts/bin/asset": asset(filepath.Join(fonts, "5.bin"), ""),
		"/api/v1/fonts/store/asset":   {"itemId": otherItem, "path": filepath.Join(stored, "subs", "fonts", "0.otf"), "format": "otf"},
		"/api/v1/fonts/new/asset":     asset(filepath.Join(next, "subs", "fonts", "0.ttf"), "ttf"),
		"/api/v1/fonts/beside/asset":  asset(filepath.Join(pkg, "subs", "6.ttf"), "ttf"),
		"/api/v1/fonts/another/asset": asset(filepath.Join(other, "subs", "fonts", "0.ttf"), "ttf"),
		"/api/v1/fonts/outside/asset": asset(outside, "ttf"), "/api/v1/fonts/away/asset": asset(filepath.Join(fonts, "away.ttf"), "ttf"),
		"/api/v1/fonts/eot/asset": asset(filepath.Join(fonts, "0.ttf"), "eot"), "/api/v1/fonts/gone/asset": asset(filepath.Join(fonts, "9.ttf"), "ttf"),
	}, map[string]string{styledItem: pkg, otherItem: stored})
	c := catalog.New(k.URL)
	resolver, clock := NewResolver(c), time.Now()
	var clockMu sync.Mutex
	resolver.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	later := func() {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(2 * resolveRefetchMin)
	}
	r := styledRouter(&Handler{Catalog: c, Packages: resolver, MediaRoot: share})
	for id, want := range map[string]string{"ttf": "font/ttf", "otf": "font/otf", "ttc": "font/collection",
		"woff": "font/woff", "woff2": "font/woff2", "bin": "application/octet-stream", "store": "font/otf"} {
		ext := id
		if id == "store" {
			ext = "otf"
		}
		w := fetch(r, "/api/play/fonts/"+id+"."+ext, nil)
		path := k.answer(t, "/api/v1/fonts/"+id+"/asset")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want || w.Body.String() != files[path] {
			t.Errorf("font %s: %d %q %q, want %s", id, w.Code, w.Header().Get("Content-Type"), w.Body, want)
		}
		for h, v := range map[string]string{"Cache-Control": "private, max-age=86400", "X-Content-Type-Options": "nosniff",
			"Access-Control-Allow-Origin": "*"} {
			if got := w.Header().Get(h); got != v {
				t.Errorf("font %s %s: %q, want %q", id, h, got, v)
			}
		}
	}
	// The extension is advisory: the catalog's format names the MIME.
	if w := fetch(r, "/api/play/fonts/otf.bin", nil); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/otf" {
		t.Errorf("an otf at its .bin URL: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	since := http.Header{"If-Modified-Since": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}}
	if w := fetch(r, "/api/play/fonts/ttf.ttf", since); w.Code != http.StatusNotModified {
		t.Errorf("an unchanged font asked for again by its time: %d", w.Code)
	}
	etag := fetch(r, "/api/play/fonts/ttf.ttf", nil).Header().Get("ETag")
	if w := fetch(r, "/api/play/fonts/ttf.ttf", http.Header{"If-None-Match": {etag}}); etag == "" || w.Code != http.StatusNotModified {
		t.Errorf("an unchanged font asked for again by its ETag %q: %d", etag, w.Code)
	}
	if w := fetch(r, "/api/play/fonts/otf.otf", http.Header{"If-None-Match": {etag}}); w.Code != http.StatusOK {
		t.Errorf("another font asked for by the first's ETag: %d", w.Code)
	}
	// packaged anew since the stream service asked: the new package's font,
	// asked for again once the answer kept is a second old
	k.plays(styledItem, next)
	later()
	if w := fetch(r, "/api/play/fonts/new.ttf", nil); w.Code != http.StatusOK || w.Body.String() != files[filepath.Join(next, "subs", "fonts", "0.ttf")] {
		t.Errorf("a font of the package the title plays now: %d %q", w.Code, w.Body)
	}
	k.plays(styledItem, pkg)
	later()
	for id, want := range map[string]int{"nope": http.StatusNotFound, "outside": http.StatusForbidden,
		"beside": http.StatusForbidden, "another": http.StatusForbidden, "away": http.StatusForbidden,
		"eot": http.StatusBadGateway, "gone": http.StatusNotFound} {
		if w := fetch(r, "/api/play/fonts/"+id+".ttf", nil); w.Code != want {
			t.Errorf("font %s: %d %q, want %d", id, w.Code, w.Body, want)
		}
	}
}

// answer is the path katalog-api's answer at path names.
func (k *styledKatalog) answer(t *testing.T, path string) string {
	t.Helper()
	res, err := http.Get(k.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var a struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a.Path
}
