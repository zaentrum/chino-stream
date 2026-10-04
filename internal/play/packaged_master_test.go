package play

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// -update rewrites the golden files under testdata/golden from what the code
// serves now: go test ./internal/play -run Golden -update
var updateGolden = flag.Bool("update", false, "rewrite testdata/golden")

// The packages under testdata/packages are real packager output, cut down to
// their text: manifest.json, .complete, the master and the media playlists
// (no init.mp4 or media segments). Regenerated with the transcoder and the
// packager on a generated clip.
const (
	// One HEVC 1080p rendition, two stereo AAC tracks: today's packages, by
	// the packager before renditions.json (shaka's own master).
	pkgLegacy = "1e9ac700-0000-4000-8000-000000000005"
	// The same for an HDR10 source: shaka writes VIDEO-RANGE=PQ itself.
	pkgLegacyHDR = "1e9ac7d2-0000-4000-8000-000000000006"
	// pkgLegacyHDR from a shaka that wrote no VIDEO-RANGE: the attribute
	// taken out of its master, the manifest still saying hdr.
	pkgLegacyHDRNoRange = "1e9ac7d3-0000-4000-8000-000000000007"
	// One rendition by the current packager with every flag off: its own
	// master, the WebVTT renditions on disk but not in the master.
	pkgSingle = "5119e000-0000-4000-8000-000000000004"
)

// usePackages points PackagesRoot at root for the test and empties the
// per-item caches, before and after.
func usePackages(t *testing.T, root string) {
	t.Helper()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	old := PackagesRoot
	PackagesRoot = abs
	resetPackageCaches()
	t.Cleanup(func() {
		PackagesRoot = old
		resetPackageCaches()
	})
}

func resetPackageCaches() {
	for _, m := range []interface {
		Range(func(k, v any) bool)
		Delete(k any)
	}{&itemRootCache, &manifestCache, &playlistCache, &packagedCache, &zapPinnedPaths} {
		m.Range(func(k, _ any) bool { m.Delete(k); return true })
	}
	packagedCacheBytes.Store(0)
}

// golden compares got with testdata/golden/name, or writes it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from what is served:\n--- served\n%s\n--- golden\n%s", name, got, want)
	}
}

// A package with one video rendition and one audio group — every package
// on disk today — is served exactly as it is packaged, whatever the client
// says it decodes and whichever quality it asks for: its master with each
// URI carrying the request's query, and VIDEO-RANGE stamped on an HDR
// package whose master lacks it. The golden files are what chino-stream
// served for these packages before the ladder work.
func TestGoldenSingleRenditionMasterIsServedAsPackaged(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	h := &HLSHandler{}
	packages := map[string]string{
		"legacy":             pkgLegacy,
		"legacy-hdr":         pkgLegacyHDR,
		"legacy-hdr-norange": pkgLegacyHDRNoRange,
		"single":             pkgSingle,
	}
	queries := map[string]string{
		// An HEVC-capable browser.
		"hevc": "?caps=avc,hvc,aac",
		// The Zap pager: a stream token, its fixed caps, q=medium.
		"zap": "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,hvc,aac,opus,mp3&q=medium",
		// A 1080p TV decoding AC-3/E-AC-3 and 5.1 AAC, asking for v0.
		"tv": "?caps=hvc:1080,avc:1080,aac,aacmc,ac3,eac3&q=v0",
	}
	for pname, id := range packages {
		for qname, query := range queries {
			t.Run(pname+"/"+qname, func(t *testing.T) {
				w := get(h, "/api/play/"+id+"/master.m3u8"+query)
				if w.Code != 200 {
					t.Fatalf("master: %d %q", w.Code, w.Body)
				}
				if ct := w.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
					t.Errorf("Content-Type %q", ct)
				}
				golden(t, "master-"+pname+"-"+qname+".m3u8", w.Body.String())
			})
		}
	}
}

// Through the router: a ladder package's master is the client's share of
// it, its URIs carrying the request's query (the stream token, caps, q),
// so every rendition fetch is authorised and answered for the same client.
func TestPackagedMasterServesTheClientsShareOfTheLadder(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	h := &HLSHandler{}
	const query = "?stream=dXNlci0xfDE3OTEwNjI5NDM.c2ln&caps=avc,aac"
	w := get(h, "/api/play/"+pkgLadder+"/master.m3u8"+query)
	if w.Code != 200 {
		t.Fatalf("master: %d %q", w.Code, w.Body)
	}
	body := w.Body.String()
	if got := servedVariants(body); !reflect.DeepEqual(got, []string{"v1/audio", "v2/audio"}) {
		t.Errorf("variants %v:\n%s", got, body)
	}
	for _, uri := range []string{"\nv1/playlist.m3u8" + query + "\n", "\nv2/playlist.m3u8" + query + "\n",
		`URI="a0/playlist.m3u8` + query + `"`, `URI="s1/playlist.m3u8` + query + `"`, `URI="v1/iframes.m3u8` + query + `"`} {
		if !strings.Contains(body, uri) {
			t.Errorf("master lacks %q:\n%s", uri, body)
		}
	}
	if strings.Contains(body, "hvc1") || strings.Contains(body, "ec-3") {
		t.Errorf("HEVC or E-AC-3 served to a client decoding neither:\n%s", body)
	}

	// The same package, another client: its own share, not the first
	// client's from the cache.
	w = get(h, "/api/play/"+pkgLadder+"/master.m3u8?caps=avc,hvc,aac,eac3&q=auto")
	if got := servedVariants(w.Body.String()); !reflect.DeepEqual(got, []string{"v0/audio", "v0/audio-surround"}) {
		t.Errorf("an HEVC + E-AC-3 client: %v", got)
	}
	// And the one rung it asks for.
	w = get(h, "/api/play/"+pkgLadder+"/master.m3u8?caps=avc,hvc,aac&q=v2")
	if got := servedVariants(w.Body.String()); !reflect.DeepEqual(got, []string{"v2/audio"}) {
		t.Errorf("q=v2: %v", got)
	}
}

// A client that decodes none of the rungs is not served the package: the
// master falls through to the on-the-fly pipeline, as for any package
// before the ladder.
func TestPackagedLadderNoRungPlaysFallsThroughToTheTranscode(t *testing.T) {
	usePackages(t, filepath.Join("testdata", "packages"))
	captureLog(t)
	root, src := mediaFile(t, "film.mkv")
	cache, err := os.MkdirTemp("", "chino-stream-master-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  fakeBin(t, "ffmpeg", "exit 1"),
		FFprobeBin: probeFFprobe(t, probeJSON("matroska,webm", "hevc", 1920, 1080, "aac", 2, 8_000_000)),
		CacheDir:   cache,
	}
	// Neither HEVC nor H.264 at 2160p: the uhd package's H.264 rungs are
	// 1080p and 720p, over a 480 cap.
	w := get(h, "/api/play/"+pkgUHD+"/master.m3u8?caps=avc:480,aac")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "\nhigh/index.m3u8") {
		t.Errorf("want the transcode master: %d\n%s", w.Code, w.Body)
	}
}
