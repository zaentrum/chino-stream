package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// A subtitle's styled original is served at its .ass URL byte for byte, as
// the text an ASS renderer reads, cached for good as its .vtt is; its .vtt
// URL serves the WebVTT file as before. A subtitle without one is 404 there,
// a script outside the roots refused, a format the stream service does not
// know a 502, and a script gone a 404.
func TestAStyledOriginalIsServedBesideItsSubtitle(t *testing.T) {
	share := t.TempDir()
	subs := filepath.Join(share, "movies", "f0", "f001aeff-9c18-4183-b51b-51403af2515e", "versions",
		"9a2e0000-0000-4000-8000-000000000001", "subs")
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nA line.\n"
	outside := filepath.Join(t.TempDir(), "0.ass")
	writeFiles(t, map[string]string{filepath.Join(subs, "0.vtt"): vtt, filepath.Join(subs, "0.ass"): assSample,
		filepath.Join(subs, "1.vtt"): vtt, outside: assSample})
	rows := map[string]map[string]any{
		"styled": {"itemId": "i1", "path": filepath.Join(subs, "0.vtt"), "format": "webvtt",
			"styledPath": filepath.Join(subs, "0.ass"), "styledFormat": "ass"},
		"plain":   {"itemId": "i1", "path": filepath.Join(subs, "1.vtt"), "format": "webvtt"},
		"outside": {"itemId": "i1", "path": filepath.Join(subs, "1.vtt"), "format": "webvtt", "styledPath": outside, "styledFormat": "ass"},
		"unknown": {"itemId": "i1", "path": filepath.Join(subs, "1.vtt"), "format": "webvtt",
			"styledPath": filepath.Join(subs, "0.ass"), "styledFormat": "usf"},
		"gone": {"itemId": "i1", "path": filepath.Join(subs, "1.vtt"), "format": "webvtt",
			"styledPath": filepath.Join(subs, "9.ass"), "styledFormat": "ass"},
	}
	katalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row, ok := rows[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/subtitles/"), "/asset")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(row)
	}))
	t.Cleanup(katalog.Close)
	r := styledRouter(&Handler{Catalog: catalog.New(katalog.URL), MediaRoot: share})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	w := get("/api/play/subs/styled.ass")
	if w.Code != http.StatusOK || w.Body.String() != assSample {
		t.Fatalf("styled .ass: %d %q", w.Code, w.Body)
	}
	for k, want := range map[string]string{"Content-Type": "text/x-ssa; charset=utf-8",
		"Cache-Control": "public, max-age=31536000, immutable", "Access-Control-Allow-Origin": "*"} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("styled .ass %s: %q, want %q", k, got, want)
		}
	}
	if w := get("/api/play/subs/styled.vtt"); w.Code != http.StatusOK || w.Body.String() != vtt ||
		w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Errorf("styled .vtt: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	for id, want := range map[string]int{"plain": http.StatusNotFound, "nope": http.StatusNotFound,
		"outside": http.StatusForbidden, "unknown": http.StatusBadGateway, "gone": http.StatusNotFound} {
		if w := get("/api/play/subs/" + id + ".ass"); w.Code != want {
			t.Errorf("%s .ass: %d %q, want %d", id, w.Code, w.Body, want)
		}
	}
}

// A package's font is served at its URL whatever its extension says, as the
// catalog's format names it (a font of no format as bytes), from a version's
// package under the media root and from the package store; cached for a day,
// and answered 304 while unchanged. An id of no font is 404, a font outside
// the roots refused, a format the stream service does not know a 502.
func TestAPackagesFontIsServed(t *testing.T) {
	share := t.TempDir()
	store := filepath.Join(t.TempDir(), "packages")
	old := legacyPackageStore
	legacyPackageStore = store
	t.Cleanup(func() { legacyPackageStore = old })
	fonts := filepath.Join(share, "movies", "f0", "f001aeff-9c18-4183-b51b-51403af2515e", "versions",
		"9a2e0000-0000-4000-8000-000000000001", "subs", "fonts")
	storeFont := filepath.Join(store, "movies", "f0", "f001aeff-9c18-4183-b51b-51403af2515e", "subs", "fonts", "0.otf")
	outside := filepath.Join(t.TempDir(), "0.ttf")
	files := map[string]string{
		filepath.Join(fonts, "0.ttf"): "\x00\x01\x00\x00 a ttf", filepath.Join(fonts, "1.otf"): "OTTO an otf, longer",
		filepath.Join(fonts, "2.ttc"): "ttcf a ttc", filepath.Join(fonts, "3.woff"): "wOFF a woff",
		filepath.Join(fonts, "4.woff2"): "wOF2 a woff2", filepath.Join(fonts, "5.bin"): "some bytes",
		storeFont: "OTTO the store's", outside: "\x00\x01\x00\x00 outside",
	}
	writeFiles(t, files)
	rows := map[string][2]string{ // id → path, format
		"ttf": {filepath.Join(fonts, "0.ttf"), "ttf"}, "otf": {filepath.Join(fonts, "1.otf"), "otf"},
		"ttc": {filepath.Join(fonts, "2.ttc"), "ttc"}, "woff": {filepath.Join(fonts, "3.woff"), "woff"},
		"woff2": {filepath.Join(fonts, "4.woff2"), "woff2"}, "bin": {filepath.Join(fonts, "5.bin"), ""},
		"store": {storeFont, "otf"}, "outside": {outside, "ttf"}, "eot": {filepath.Join(fonts, "0.ttf"), "eot"},
		"gone": {filepath.Join(fonts, "9.ttf"), "ttf"},
	}
	katalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row, ok := rows[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/fonts/"), "/asset")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		a := map[string]any{"itemId": "i1", "path": row[0]}
		if row[1] != "" {
			a["format"] = row[1]
		}
		_ = json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(katalog.Close)
	r := styledRouter(&Handler{Catalog: catalog.New(katalog.URL), MediaRoot: share})
	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		r.ServeHTTP(rec, req)
		return rec
	}
	for id, want := range map[string]string{"ttf": "font/ttf", "otf": "font/otf", "ttc": "font/collection",
		"woff": "font/woff", "woff2": "font/woff2", "bin": "application/octet-stream", "store": "font/otf"} {
		ext := id
		if id == "store" {
			ext = "otf"
		}
		w := get("/api/play/fonts/"+id+"."+ext, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want || w.Body.String() != files[rows[id][0]] {
			t.Errorf("font %s: %d %q %q, want %s", id, w.Code, w.Header().Get("Content-Type"), w.Body, want)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=86400" {
			t.Errorf("font %s: Cache-Control %q", id, got)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("font %s: Access-Control-Allow-Origin %q", id, got)
		}
	}
	// The extension is advisory: the catalog's format names the MIME.
	if w := get("/api/play/fonts/otf.bin", nil); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/otf" {
		t.Errorf("an otf at its .bin URL: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	since := http.Header{"If-Modified-Since": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}}
	if w := get("/api/play/fonts/ttf.ttf", since); w.Code != http.StatusNotModified {
		t.Errorf("an unchanged font asked for again by its time: %d", w.Code)
	}
	etag := get("/api/play/fonts/ttf.ttf", nil).Header().Get("ETag")
	if w := get("/api/play/fonts/ttf.ttf", http.Header{"If-None-Match": {etag}}); etag == "" || w.Code != http.StatusNotModified {
		t.Errorf("an unchanged font asked for again by its ETag %q: %d", etag, w.Code)
	}
	if w := get("/api/play/fonts/otf.otf", http.Header{"If-None-Match": {etag}}); w.Code != http.StatusOK {
		t.Errorf("another font asked for by the first's ETag: %d", w.Code)
	}
	for id, want := range map[string]int{"nope": http.StatusNotFound, "outside": http.StatusForbidden,
		"eot": http.StatusBadGateway, "gone": http.StatusNotFound} {
		if w := get("/api/play/fonts/"+id+".ttf", nil); w.Code != want {
			t.Errorf("font %s: %d %q, want %d", id, w.Code, w.Body, want)
		}
	}
}
