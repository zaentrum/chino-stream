package play

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// copyPackage copies the test package id under testdata/packages into a
// fresh packages root, its manifest changed by edit, points PackagesRoot at
// that root and returns the package's folder there.
func copyPackage(t *testing.T, id string, edit func(m map[string]any)) string {
	t.Helper()
	srcs, err := filepath.Glob(filepath.Join("testdata", "packages", "*", id[:2], id))
	if err != nil || len(srcs) != 1 {
		t.Fatalf("package %s: %v %v", id, srcs, err)
	}
	src := srcs[0]
	root := t.TempDir()
	rel, err := filepath.Rel(filepath.Join("testdata", "packages"), src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, rel)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		to := filepath.Join(dst, strings.TrimPrefix(path, src))
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	usePackages(t, root)
	return dst
}

// infoBody is GET /api/play/{id}/info for the item, its body as a string
// and its audio and subtitle tracks.
func infoBody(t *testing.T, h *Handler, id string) (string, []map[string]any, []map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/play/{itemId}/info", h.Info)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/play/"+id+"/info?caps=avc,hvc,aac", nil))
	if w.Code != 200 {
		t.Fatalf("info: %d %q", w.Code, w.Body)
	}
	var info struct {
		Audio     []map[string]any `json:"audio_tracks"`
		Subtitles []map[string]any `json:"subtitle_tracks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return w.Body.String(), info.Audio, info.Subtitles
}

// names lists each track's name, and fails where its title is not that name.
func names(t *testing.T, tracks []map[string]any) []string {
	t.Helper()
	var out []string
	for _, tr := range tracks {
		name, _ := tr["name"].(string)
		if tr["title"] != name {
			t.Errorf("track %v: title %q is not its name %q", tr["index"], tr["title"], name)
		}
		out = append(out, name)
	}
	return out
}

// /play/info names a package's tracks as the packager named them, else -
// a manifest from before names - by their language, then what their title
// says besides; title is the same name. The source's title as it is never
// reaches a player: here a codec ("AC3 5.1 @ 640 Kbps"). A subtitle file
// from next to the source is listed after the source's tracks, its sN past
// theirs, and served.
func TestPlayInfoNamesAPackagesTracksNeverByTheSourcesTitle(t *testing.T) {
	dir := copyPackage(t, pkgSingle, func(m map[string]any) {
		audio := m["renditions"].(map[string]any)["audio"].([]any)
		a0, a1 := audio[0].(map[string]any), audio[1].(map[string]any)
		a0["language"], a0["title"], a0["name"] = "zxx", "AC3 5.1 @ 640 Kbps", "" // a manifest from before names
		a1["title"], a1["name"] = "Commentary", "English · Commentary"
		subs := m["subtitles"].([]any)
		subs[2].(map[string]any)["name"] = "German · Songs"
		m["subtitles"] = append(subs, map[string]any{
			"id": "sub3", "language": "fre", "title": "Français", "default": false, "forced": false,
			"visible": true, "path": "subs/3.vtt", "format": "webvtt", "hls": "hls/s3", "external": true,
		})
	})
	// The sidecar's HLS rendition, as the packager writes it at hls/s3.
	for _, f := range []string{"playlist.m3u8", "seg-00001.vtt"} {
		b, err := os.ReadFile(filepath.Join(dir, "hls", "s2", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "hls", "s3"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "hls", "s3", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root, src := mediaFile(t, "film.mkv")
	body, audio, subs := infoBody(t, &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root}, pkgSingle)
	if got, want := names(t, audio), []string{"No dialogue", "English · Commentary"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio names %q, want %q", got, want)
	}
	if got, want := names(t, subs), []string{"English", "English · Forced", "German · Songs", "French"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtitle names %q, want %q", got, want)
	}
	if s := subs[3]; s["id"] != "sub3" || s["hls"] != "hls/s3" || s["external"] != true || s["language"] != "fre" {
		t.Errorf("the sidecar from next to the source: %v", s)
	}
	if strings.Contains(body, "AC3 5.1 @ 640 Kbps") || strings.Contains(body, "Français") {
		t.Errorf("a source title reaches the player:\n%s", body)
	}

	h := &HLSHandler{}
	if w := get(h, "/api/play/"+pkgSingle+"/s3/playlist.m3u8"); w.Code != 200 || !strings.Contains(w.Body.String(), "seg-00001.vtt") {
		t.Errorf("s3 playlist: %d %q", w.Code, w.Body)
	}
	if w := get(h, "/api/play/"+pkgSingle+"/s3/seg-00001.vtt"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "WEBVTT") {
		t.Errorf("s3 segment: %d %q", w.Code, w.Body)
	}
}

// A package from before names: its tracks by their language.
func TestPlayInfoNamesAnOldPackagesTracksByTheirLanguage(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	root, src := mediaFile(t, "film.mkv")
	_, audio, subs := infoBody(t, &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root}, pkgLegacy)
	if got, want := names(t, audio), []string{"English", "German"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio names %q, want %q", got, want)
	}
	if got, want := names(t, subs), []string{"English", "English · Forced", "German"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtitle names %q, want %q", got, want)
	}
}

// On the fly, /play/info names a source's audio tracks as the master's NAMEs
// do and its subtitles by their language and what their titles say besides;
// title is the same name, never the source's title as it is.
func TestPlayInfoNamesASourcesTracksNeverByItsTitle(t *testing.T) {
	usePackages(t, t.TempDir())
	probe := `{"streams":[` +
		`{"codec_type":"video","codec_name":"h264","width":1280,"height":720},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":6,"tags":{"language":"eng","title":"AC3 5.1 @ 640 Kbps"},"disposition":{"default":1}},` +
		`{"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"zxx"}},` +
		`{"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"title":"Track 3"}},` +
		`{"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"eng","title":"Commentary"}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng","title":"SDH"}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"},"disposition":{"forced":1}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"title":"Signs"}}` +
		`],"format":{"format_name":"matroska,webm","duration":"600.0","bit_rate":"4000000"}}`
	root, src := mediaFile(t, "film.mkv")
	h := &Handler{Catalog: fakeKatalog(t, src), MediaRoot: root, FFprobeBin: probeFFprobe(t, probe)}
	body, audio, subs := infoBody(t, h, "i1")
	if got, want := names(t, audio), []string{"English", "No dialogue", "Unknown", "English · Commentary"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio names %q, want %q", got, want)
	}
	if got, want := names(t, subs), []string{"English · SDH", "English (forced)", "Signs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtitle names %q, want %q", got, want)
	}
	if a := audio[0]; a["index"] != 0.0 || a["language"] != "eng" || a["default"] != true || a["channels"] != 6.0 {
		t.Errorf("first audio track: %v", a)
	}
	if strings.Contains(body, "AC3 5.1 @ 640 Kbps") || strings.Contains(body, "Track 3") {
		t.Errorf("a source title reaches the player:\n%s", body)
	}
}
