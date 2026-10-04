package play

import (
	"flag"
	"os"
	"path/filepath"
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
