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
// A client served a mixed group (an E-AC-3 client's one group of 5.1 and
// stereo, which Media3 reads every rendition of before it starts) gets the
// playlist and init of each other member warmed too; a native player each
// group's start, and of its mixed 5.1 group the others'. A stereo-only
// client's warm is the start alone.
func TestMasterFetchWarmsTheStartingVariant(t *testing.T) {
	cases := []struct {
		name, pkg, query string
		want             []string
	}{
		{"H.264 client: the top H.264 rung and the default audio", pkgLadder, "?caps=avc,aac",
			[]string{"hls/master.m3u8", "hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{"HEVC client", pkgLadder, "?caps=avc,hvc,aac",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
		{"HEVC and E-AC-3 client: the default English's 5.1 companion, and the mixed group's other members", pkgLadder, "?caps=avc,hvc,aac,eac3",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a2/playlist.m3u8", "hls/a2/init.mp4",
				"hls/a0/playlist.m3u8", "hls/a0/init.mp4", "hls/a1/playlist.m3u8", "hls/a1/init.mp4"}},
		{"HEVC and E-AC-3 client with t=13: segments of its start only", pkgLadder, "?caps=avc,hvc,aac,eac3&t=13",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/v0/seg-00003.m4s", "hls/v0/seg-00004.m4s",
				"hls/a2/playlist.m3u8", "hls/a2/init.mp4", "hls/a2/seg-00003.m4s", "hls/a2/seg-00004.m4s",
				"hls/a0/playlist.m3u8", "hls/a0/init.mp4", "hls/a1/playlist.m3u8", "hls/a1/init.mp4"}},
		{"a native HEVC and E-AC-3 player: each group's start, and German (stereo in the mixed 5.1 group)", pkgLadder, "?caps=avc,hvc,aac,eac3,native",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4",
				"hls/a2/playlist.m3u8", "hls/a2/init.mp4", "hls/a1/playlist.m3u8", "hls/a1/init.mp4"}},
		{"a native player without E-AC-3: the start alone", pkgLadder, "?caps=avc,hvc,aac,native",
			[]string{"hls/master.m3u8", "hls/v0/playlist.m3u8", "hls/v0/init.mp4", "hls/a0/playlist.m3u8", "hls/a0/init.mp4"}},
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
	// An E-AC-3 client starts on the default English's 5.1 companion, a2; of
	// its mixed group the other members' playlists and inits.
	want := files(pkgLadder, "hls/master.m3u8",
		"hls/v1/playlist.m3u8", "hls/v1/init.mp4", "hls/v1/seg-00003.m4s", "hls/v1/seg-00004.m4s",
		"hls/a2/playlist.m3u8", "hls/a2/init.mp4", "hls/a2/seg-00003.m4s", "hls/a2/seg-00004.m4s",
		"hls/a0/playlist.m3u8", "hls/a0/init.mp4", "hls/a1/playlist.m3u8", "hls/a1/init.mp4")
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

// nineLanguages is a title of nine stereo tracks, English the default, and
// English's 5.1 companion (a9) — with every, a companion for each language
// (a9 to a17): a mixed group of ten renditions (eighteen) for an E-AC-3
// client, more than a warm reads.
func nineLanguages(every bool) (stereo, surround []unionTrack) {
	for i, l := range []struct{ lang, hls, name string }{
		{"eng", "en", "English"}, {"ger", "de", "German"}, {"fre", "fr", "French"}, {"ita", "it", "Italian"},
		{"spa", "es", "Spanish"}, {"dut", "nl", "Dutch"}, {"por", "pt", "Portuguese"}, {"swe", "sv", "Swedish"}, {"dan", "da", "Danish"},
	} {
		stereo = append(stereo, unionTrack{id: "a" + itoa(i), lang: l.lang, hlsLang: l.hls, name: l.name, codec: "mp4a.40.2",
			channels: 2, src: i, def: i == 0})
		if i == 0 || every {
			surround = append(surround, unionTrack{id: "a" + itoa(9+i), lang: l.lang, hlsLang: l.hls, name: l.name + " 5.1",
				codec: "ec-3", channels: 6, src: i, def: i == 0})
		}
	}
	return stereo, surround
}

// warmedRenditions are the renditions of the package in dir whose playlist
// and init are both in packagedCache, of those named.
func warmedRenditions(dir string, names ...string) []string {
	var out []string
	for _, r := range names {
		_, playlist := packagedCache.Load(filepath.Join(dir, "hls", r, "playlist.m3u8"))
		_, init := packagedCache.Load(filepath.Join(dir, "hls", r, "init.mp4"))
		if playlist && init {
			out = append(out, r)
		}
	}
	return out
}

// audioWindowsWarmed are the on-the-fly audio tracks whose first window's
// init is in h's cache ("audio-1").
func audioWindowsWarmed(t *testing.T, h *HLSHandler) []string {
	t.Helper()
	inits, err := filepath.Glob(filepath.Join(h.CacheDir, "*", "audio-*", "init.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range inits {
		out = append(out, filepath.Base(filepath.Dir(p)))
	}
	sort.Strings(out)
	return out
}

// A packaged master that serves a mixed group warms the playlist and init
// of each of its members — what Media3 reads before it starts, a TV
// limited to stereo then starting on the stereo twin — at most eight
// renditions, the start first, then in the master's order. A stereo-only
// client's warm is its start alone. Either layout.
func TestAMixedGroupWarmsEveryMembersPlaylistAndInit(t *testing.T) {
	all := []string{"a0", "a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"}
	for _, record := range []bool{false, true} {
		t.Run(map[bool]string{false: "manifest.json", true: "package.json"}[record], func(t *testing.T) {
			h, _, dir := unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
			if w := get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac,eac3"); w.Code != 200 {
				t.Fatalf("master: %d %q", w.Code, w.Body)
			}
			if got := warmedRenditions(dir, "a0", "a1", "a2"); !reflect.DeepEqual(got, []string{"a0", "a1", "a2"}) {
				t.Errorf("a mixed group warmed %v, want every member", got)
			}

			h, _, dir = unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac")
			if got := warmedRenditions(dir, "a0", "a1", "a2"); !reflect.DeepEqual(got, []string{"a1"}) {
				t.Errorf("a stereo-only client warmed %v, want its start a1 alone", got)
			}

			stereo, surround := nineLanguages(false)
			h, _, dir = unionPackage(t, record, stereo, surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac,eac3")
			if got, want := warmedRenditions(dir, all...), []string{"a0", "a1", "a2", "a3", "a4", "a5", "a6", "a9"}; !reflect.DeepEqual(got, want) {
				t.Errorf("ten renditions: warmed %v, want %v (the start a9, then the master's first seven)", got, want)
			}
			// Of the others no segment: the start's are what ?t= asks for.
			if _, ok := packagedCache.Load(filepath.Join(dir, "hls", "a0", "seg-00001.m4s")); ok {
				t.Error("a member's segment was warmed")
			}
		})
	}
}

// On the fly from a package, a client served the mixed group gets warmed
// what is a file read: the companion it starts on (as before) and each
// other companion's playlist and init from the package, at most eight
// renditions in all. No transcode window starts for a stereo member — that
// is a 60 s encode a client that never plays it (hls.js) would pay for; a
// client that asks for one (Media3, preparing) starts it then. A
// stereo-only client's warm is the default track's window alone, a native
// player's that and its 5.1 group's companion, as before.
func TestOnTheFlyAMixedGroupWarmsItsCompanionsAlone(t *testing.T) {
	twoCompanions := struct{ stereo, surround []unionTrack }{
		stereo: []unionTrack{
			{id: "a0", lang: "eng", hlsLang: "en", name: "English", codec: "mp4a.40.2", channels: 2, src: 0, def: true},
			{id: "a1", lang: "ger", hlsLang: "de", name: "German", codec: "mp4a.40.2", channels: 2, src: 1},
		},
		surround: []unionTrack{
			{id: "a2", lang: "eng", hlsLang: "en", name: "English 5.1", codec: "ec-3", channels: 6, src: 0, def: true},
			{id: "a3", lang: "ger", hlsLang: "de", name: "German 5.1", codec: "ec-3", channels: 6, src: 1},
		},
	}
	for _, record := range []bool{false, true} {
		t.Run(map[bool]string{false: "manifest.json", true: "package.json"}[record], func(t *testing.T) {
			h, _, dir := unionPackage(t, record, twoCompanions.stereo, twoCompanions.surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,aac,eac3")
			waitProductions(t, h)
			if got := warmedRenditions(dir, "a0", "a1", "a2", "a3"); !reflect.DeepEqual(got, []string{"a2", "a3"}) {
				t.Errorf("warmed %v, want the companions a2 (its start) and a3", got)
			}
			if got := audioWindowsWarmed(t, h); got != nil {
				t.Errorf("audio windows started: %v, want none (it starts on a companion)", got)
			}

			h, _, _ = unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,aac")
			waitProductions(t, h)
			if got := audioWindowsWarmed(t, h); !reflect.DeepEqual(got, []string{"audio-1"}) {
				t.Errorf("a stereo-only client warmed %v, want the default track's window alone", got)
			}

			h, _, dir = unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,aac,eac3,native")
			waitProductions(t, h)
			if got := audioWindowsWarmed(t, h); !reflect.DeepEqual(got, []string{"audio-1"}) {
				t.Errorf("a native player warmed %v, want its stereo start's window alone", got)
			}
			if got := warmedRenditions(dir, "a2"); len(got) != 1 {
				t.Error("a native player's 5.1 group's companion was not warmed")
			}

			stereo, surround := nineLanguages(true)
			h, _, dir = unionPackage(t, record, stereo, surround)
			get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,aac,eac3")
			waitProductions(t, h)
			companions := []string{"a9", "a10", "a11", "a12", "a13", "a14", "a15", "a16", "a17"}
			if got, want := warmedRenditions(dir, companions...), companions[:8]; !reflect.DeepEqual(got, want) {
				t.Errorf("nine companions: warmed %v, want %v (the start a9, then seven)", got, want)
			}
			if got := audioWindowsWarmed(t, h); got != nil {
				t.Errorf("audio windows started: %v, want none", got)
			}
		})
	}
}
