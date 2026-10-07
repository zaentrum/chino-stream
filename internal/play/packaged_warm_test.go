package play

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// stagePackages copies test packages into a fresh packages root with a
// stand-in for every init.mp4 and media segment their playlists name (the
// fixtures carry no media) and serves it (usePackages), returning its
// resolver. edit, when set, rewrites each manifest.
func stagePackages(t *testing.T, edit func(map[string]any), ids ...string) *Resolver {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		src, err := filepath.Glob(filepath.Join("testdata", "packages", "*", id[:2], id))
		if err != nil || len(src) != 1 {
			t.Fatalf("package %s: %v %v", id, src, err)
		}
		rel, _ := filepath.Rel(filepath.Join("testdata", "packages"), src[0])
		dst := filepath.Join(root, rel)
		err = filepath.Walk(src[0], func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out := filepath.Join(dst, strings.TrimPrefix(p, src[0]))
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if filepath.Base(p) == "manifest.json" && edit != nil {
				var m map[string]any
				if err := json.Unmarshal(b, &m); err != nil {
					return err
				}
				edit(m)
				if b, err = json.Marshal(m); err != nil {
					return err
				}
			}
			if err := os.WriteFile(out, b, 0o644); err != nil {
				return err
			}
			if filepath.Base(p) != "playlist.m3u8" {
				return nil
			}
			for _, name := range playlistMedia(string(b)) {
				if err := os.WriteFile(filepath.Join(filepath.Dir(out), name), []byte("media "+name), 0o644); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return usePackages(t, root)
}

// playlistMedia lists the files a media playlist names: its init (EXT-X-MAP)
// and its segments.
func playlistMedia(body string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			out = append(out, hlsAttributes(l)["URI"])
		case l != "" && !strings.HasPrefix(l, "#"):
			out = append(out, l)
		}
	}
	return out
}

// cached lists packagedCache's keys as "<item>/<path in the package>".
func cached(t *testing.T) []string {
	t.Helper()
	var out []string
	packagedCache.Range(func(k, _ any) bool {
		p := k.(string)
		rel, err := filepath.Rel(testPackagesRoot, p)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 4) // movies/1a/<id>/<path>
		out = append(out, parts[2][:8]+"/"+parts[3])
		return true
	})
	sort.Strings(out)
	return out
}

func files(item string, rel ...string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = item[:8] + "/" + r
	}
	sort.Strings(out)
	return out
}

// Fetching a master warms the variant the client starts on — the served
// master's first variant and its audio rendition — and nothing else of a
// ladder: their playlists and inits, and with ?t= the segments from there.
func TestMasterFetchWarmsTheStartingVariant(t *testing.T) {
	cases := []struct {
		name, pkg, query string
		want             []string
	}{
		{"H.264 client: the top H.264 rung and the default audio", pkgLadder, "?caps=avc,aac",
			[]string{"hls/master.m3u8", "hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{"HEVC client", pkgLadder, "?caps=avc,hvc,aac",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{"HEVC and E-AC-3 client: the default English's 5.1 companion", pkgLadder, "?caps=avc,hvc,aac,eac3",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a2/playlist.m3u8", "hls/a2/init.mp4"}},
		{"a quality pick", pkgLadder, "?caps=avc,hvc,aac&q=v2",
			[]string{"hls/master.m3u8", "hls/v2/playlist.m3u8", "hls/v2/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{"with t=0, from the first segment", pkgLadder, "?caps=avc,aac&t=0",
			[]string{"hls/master.m3u8", "hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4",
				"hls/v1/seg-00001.m4s", "hls/v1/seg-00002.m4s", "hls/v1/seg-00003.m4s", "hls/v1/seg-00004.m4s",
				"hls/a0/seg-00001.m4s", "hls/a0/seg-00002.m4s", "hls/a0/seg-00003.m4s", "hls/a0/seg-00004.m4s"}},
		{"with t=13, from the segment holding it", pkgLadder, "?caps=avc,aac&t=13",
			[]string{"hls/master.m3u8", "hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4",
				"hls/v1/seg-00003.m4s", "hls/v1/seg-00004.m4s", "hls/a0/seg-00003.m4s", "hls/a0/seg-00004.m4s"}},
		{"a package before the ladder: its one variant, its first audio rendition", pkgLegacy, "?caps=avc,hvc,aac",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &HLSHandler{Packages: stagePackages(t, nil, tc.pkg)}
			if w := get(h, "/api/play/"+tc.pkg+"/master.m3u8"+tc.query); w.Code != 200 {
				t.Fatalf("master: %d %q", w.Code, w.Body)
			}
			if got, want := cached(t), files(tc.pkg, tc.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("warmed\n %v\nwant\n %v", got, want)
			}
		})
	}
}

// POST /prewarm warms the variant the client will start on, for its caps
// and q, from ?t= — not every rendition of the ladder.
func TestPrewarmWarmsTheStartingVariant(t *testing.T) {
	h := &HLSHandler{Packages: stagePackages(t, nil, pkgLadder)}
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", h.Routes)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/play/"+pkgLadder+"/prewarm?caps=avc,aac,eac3&q=medium&t=13", nil))
	if w.Code != http.StatusAccepted || w.Body.String() != "packaged-warming" {
		t.Fatalf("prewarm: %d %q", w.Code, w.Body)
	}
	// An E-AC-3 client starts on the default English's 5.1 companion, a2.
	want := files(pkgLadder, "hls/master.m3u8",
		"hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/v1/seg-00003.m4s", "hls/v1/seg-00004.m4s",
		"hls/a2/playlist.m3u8", "hls/a2/init.mp4", "hls/a2/seg-00003.m4s", "hls/a2/seg-00004.m4s")
	if got := cached(t); !reflect.DeepEqual(got, want) {
		t.Errorf("warmed\n %v\nwant\n %v", got, want)
	}

	// Without t, from the start.
	h.Packages = stagePackages(t, nil, pkgLadder)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/play/"+pkgLadder+"/prewarm?caps=avc,hvc,aac&q=v2", nil))
	if got := cached(t); !reflect.DeepEqual(got, files(pkgLadder, "hls/master.m3u8",
		"hls/v2/playlist.m3u8", "hls/v2/init.mp4", "hls/v2/seg-00001.m4s", "hls/v2/seg-00002.m4s", "hls/v2/seg-00003.m4s", "hls/v2/seg-00004.m4s",
		"hls/a0/playlist.m3u8", "hls/a0/init.mp4", "hls/a0/seg-00001.m4s", "hls/a0/seg-00002.m4s", "hls/a0/seg-00003.m4s", "hls/a0/seg-00004.m4s")) {
		t.Errorf("q=v2 warmed %v", got)
	}
}

// A Zap pool entry warms the variants Zap clients start on — an HEVC
// client's and any other's — at its seek point, not every rendition; a
// package with one rendition, that one once.
func TestZapPoolWarmsTheStartingVariants(t *testing.T) {
	long := func(m map[string]any) { m["durationMs"] = 600_000 } // long enough for a midpoint
	h := &HLSHandler{Packages: stagePackages(t, long, pkgLadder, pkgLegacy)}
	for _, tc := range []struct {
		pkg  string
		want []string
	}{
		{pkgLadder, []string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/v1/playlist.m3u8", "hls/v1/init.mp4",
			"hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{pkgLegacy, []string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
	} {
		e := warmOneZapItem(h, tc.pkg)
		if e == nil {
			t.Fatalf("%s: no entry", tc.pkg)
		}
		var got []string
		for _, p := range e.Paths {
			if rel, _ := filepath.Rel(legacyDir(t, tc.pkg), p); !strings.HasSuffix(rel, ".m4s") {
				got = append(got, tc.pkg[:8]+"/"+filepath.ToSlash(rel))
			}
		}
		sort.Strings(got)
		if want := files(tc.pkg, tc.want...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s warmed\n %v\nwant\n %v", tc.pkg, got, want)
		}
		segs := 0
		for _, p := range e.Paths {
			if strings.HasSuffix(p, ".m4s") {
				segs++
				if _, ok := packagedCache.Load(p); !ok {
					t.Errorf("%s not resident", p)
				}
			}
		}
		if segs == 0 {
			t.Errorf("%s: no segment warmed at the seek point", tc.pkg)
		}
		if len(e.Paths) != len(dedupe(e.Paths)) {
			t.Errorf("%s: paths twice in %v", tc.pkg, e.Paths)
		}
	}
}

func dedupe(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}
