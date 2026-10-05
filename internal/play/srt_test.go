package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// A SubRip file as files have it: CRLF, cue numbers, a comma before the
// milliseconds, an ASS position block, one-digit hours, a dot, a short
// fraction, coordinates after the end time, and an arrow in the text.
const srtSample = "1\r\n00:00:01,000 --> 00:00:02,500\r\n{\\an8}Hello there.\r\n\r\n" +
	"2\r\n0:00:03.25 --> 0:00:04,5 X1:10 X2:20 Y1:30 Y2:40\r\n<i>A --> B</i>\r\n\r\n"

func TestASubRipFileIsWebVTT(t *testing.T) {
	got := string(srtToWebVTT([]byte(srtSample)))
	want := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:02.500\nHello there.\n\n" +
		"00:00:03.250 --> 00:00:04.500\n<i>A → B</i>\n"
	if got != want {
		t.Fatalf("converted:\n%q\nwant:\n%q", got, want)
	}
}

// Files lay their cues out every way SubRip readers take: blank lines between
// a cue's number, its timing and its text (and several between cues), lines
// of spaces, no blank line before the next cue, two-line text.
func TestASubRipFileLaidOutLooselyIsWebVTT(t *testing.T) {
	for name, in := range map[string]string{
		"blank lines inside cues": "1\n\n00:00:01,000 --> 00:00:02,000\n\nOne.\n\n\n\n2\n\n00:00:03,000 --> 00:00:04,000\n\nTwo,\nstill two.\n\n\n\n",
		"lines of spaces":         "1\n00:00:01,000 --> 00:00:02,000\nOne.\n   \n2\n00:00:03,000 --> 00:00:04,000\nTwo,\nstill two.\n \n",
		"no blank line between":   "1\n00:00:01,000 --> 00:00:02,000\nOne.\n2\n00:00:03,000 --> 00:00:04,000\nTwo,\nstill two.",
	} {
		got := string(srtToWebVTT([]byte(in)))
		want := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nOne.\n\n00:00:03.000 --> 00:00:04.000\nTwo,\nstill two.\n"
		if got != want {
			t.Errorf("%s:\n%q\nwant:\n%q", name, got, want)
		}
	}
	// A cue without text is left out; a line of digits that is a cue's own
	// text, not the next one's number, stays.
	got := string(srtToWebVTT([]byte("1\n00:00:01,000 --> 00:00:02,000\n\n2\n00:00:03,000 --> 00:00:04,000\n1984\n\nThe end.\n")))
	if want := "WEBVTT\n\n00:00:03.000 --> 00:00:04.000\n1984\nThe end.\n"; got != want {
		t.Errorf("empty cue and digits:\n%q\nwant:\n%q", got, want)
	}
}

func TestASubtitleFilesTextIsDecoded(t *testing.T) {
	utf16le := func(s string, bom bool) []byte {
		var b []byte
		if bom {
			b = append(b, 0xFF, 0xFE)
		}
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u), byte(u>>8))
		}
		return b
	}
	utf16be := func(s string) []byte {
		b := []byte{0xFE, 0xFF}
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u>>8), byte(u))
		}
		return b
	}
	for name, tc := range map[string]struct {
		in   []byte
		want string
	}{
		"utf-8":                {[]byte("1\nCafé"), "1\nCafé"},
		"utf-8 with its mark":  {append([]byte{0xEF, 0xBB, 0xBF}, "1\nCafé"...), "1\nCafé"},
		"utf-16le, marked":     {utf16le("1\nНорвежский", true), "1\nНорвежский"},
		"utf-16le, not marked": {utf16le("1\nnorsk", false), "1\nnorsk"},
		"utf-16be, marked":     {utf16be("1\nnorsk"), "1\nnorsk"},
		// "Café ‘quoted’ – €5" in Windows-1252.
		"windows-1252": {[]byte{'C', 'a', 'f', 0xE9, ' ', 0x91, 'q', 0x92, ' ', 0x96, ' ', 0x80, '5'}, "Café ‘q’ – €5"},
	} {
		if got := decodeSubtitleText(tc.in); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// The handler serves a SubRip row asked for at its .vtt URL as WebVTT, and a
// WebVTT row as the file it is.
func TestASubRipSidecarIsServedAsWebVTT(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"srt":    filepath.Join(root, "Film (2020).en.srt"),
		"webvtt": filepath.Join(root, "Film (2020).de.vtt"),
	}
	if err := os.WriteFile(files["srt"], []byte(srtSample), 0o644); err != nil {
		t.Fatal(err)
	}
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHallo.\n"
	if err := os.WriteFile(files["webvtt"], []byte(vtt), 0o644); err != nil {
		t.Fatal(err)
	}
	katalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		format := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/subtitles/"), "/asset")
		path, ok := files[format]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"itemId": "i1", "path": path, "format": format, "lang": "eng"})
	}))
	t.Cleanup(katalog.Close)
	h := &Handler{Catalog: catalog.New(katalog.URL), MediaRoot: root}
	r := chi.NewRouter()
	r.Get("/api/play/subs/{subID}.vtt", h.SidecarSubtitle)

	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/play/subs/"+id+".vtt", nil))
		return rec
	}
	srt := get("srt")
	if srt.Code != http.StatusOK || srt.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Fatalf("srt: %d %q", srt.Code, srt.Header().Get("Content-Type"))
	}
	if body := srt.Body.String(); !strings.HasPrefix(body, "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\nHello there.\n") {
		t.Errorf("srt body %q", body)
	}
	webvtt := get("webvtt")
	if webvtt.Code != http.StatusOK || webvtt.Body.String() != vtt {
		t.Errorf("webvtt: %d %q", webvtt.Code, webvtt.Body.String())
	}
}
