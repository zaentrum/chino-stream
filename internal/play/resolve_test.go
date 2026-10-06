package play

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// The library under testdata/library: one movie (libItem) with two versions,
// libOld superseded by libNew, and an extra (libExtra). Their hls/ folders
// are test packages' (cut to their text: playlists, no media): libOld is
// pkgLadder's (HEVC 1080p, H.264 720p and 480p), libNew pkgSingle's (one
// HEVC rendition), libExtra the trailer's; each has a package.json as the
// packager writes it into the library and a .complete naming its hash.
const (
	libItem  = "11b2a7e0-0000-4000-8000-00000000000a"
	libOld   = "a1a1a1a1-0000-4000-8000-0000000000a1"
	libNew   = "b2b2b2b2-0000-4000-8000-0000000000b2"
	libExtra = "e3e3e3e3-0000-4000-8000-0000000000e3"
)

// library is the test library staged for a test (stageLibrary).
type library struct {
	root, item string // the library root, the item's folder
	katalog    *fakeLibrary
	packages   *Resolver
}

func (l *library) version(id string) string { return filepath.Join(l.item, "versions", id) }

// ref is the katalog-api answer of the version id: its folder, read by its
// package.json.
func (l *library) ref(id string) catalog.PackageRef {
	return catalog.PackageRef{VersionID: id, Dir: l.version(id), Record: "package.json"}
}

// stageLibrary copies testdata/library into a fresh root, with a stand-in
// for every init.mp4 and segment its playlists name ("<version> <name>", so
// a test tells the versions apart), and serves it through a katalog-api
// answering libItem as current libNew, previous libOld.
func stageLibrary(t *testing.T) *library {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("testdata", "library")
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(root, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		if filepath.Base(p) != "playlist.m3u8" {
			return nil
		}
		// "versions/<id>/hls/v0/playlist.m3u8" → "<id>"
		owner := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
		for _, name := range playlistMedia(string(b)) {
			if err := os.WriteFile(filepath.Join(filepath.Dir(out), name), []byte(owner[:8]+" "+name), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	usePackages(t, t.TempDir()) // the package store: empty
	l := &library{root: root, item: filepath.Join(root, "movies", libItem[:2], libItem), katalog: newFakeLibrary(t)}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew)), Previous: []catalog.PackageRef{l.ref(libOld)}})
	l.packages = l.katalog.resolver()
	return l
}

func ptr[T any](v T) *T { return &v }

// waitLookups waits until no katalog-api lookup of res is in flight.
func waitLookups(t *testing.T, res *Resolver) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res.mu.Lock()
		busy := false
		for _, e := range res.items {
			busy = busy || e.fetching != nil
		}
		for _, e := range res.extras {
			busy = busy || e.fetching != nil
		}
		res.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("a lookup is still in flight")
}

const itemPlayback = "/api/v1/items/" + libItem + "/playback"

// A library version plays as a package from before it does: its master as
// packaged for the client, its playlists, init and segments from its own
// folder, /play/info read from its package.json — the folder katalog-api
// answers, the current version.
func TestALibraryVersionIsServedFromTheFolderKatalogAnswers(t *testing.T) {
	l := stageLibrary(t)
	h := &HLSHandler{Packages: l.packages}
	w := get(h, "/api/play/"+libItem+"/master.m3u8?caps=avc,hvc,aac")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "\nv0/playlist.m3u8?caps=avc,hvc,aac") ||
		strings.Contains(w.Body.String(), "avc1") {
		t.Fatalf("master: %d\n%s", w.Code, w.Body)
	}
	for route, want := range map[string]string{
		"v0/init.mp4":      "b2b2b2b2 init.mp4",
		"v0/seg-00002.m4s": "b2b2b2b2 seg-00002.m4s",
		"a1/seg-00001.m4s": "b2b2b2b2 seg-00001.m4s",
	} {
		if w := get(h, "/api/play/"+libItem+"/"+route); w.Code != 200 || w.Body.String() != want {
			t.Errorf("%s: %d %q, want %q", route, w.Code, w.Body, want)
		}
	}
	if w := get(h, "/api/play/"+libItem+"/s2/seg-00001.vtt"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "WEBVTT") {
		t.Errorf("subtitle segment: %d %q", w.Code, w.Body)
	}
	if w := get(h, "/api/play/"+libItem+"/v1/playlist.m3u8"); w.Code != 404 {
		t.Errorf("a rendition only the superseded version has: %d, want 404", w.Code)
	}
	info, _ := getInfo(t, l.packages, libItem, "?caps=avc,hvc,aac")
	if info.Mode != "packaged" || info.VideoCodec != "hvc1.1.6.L120.90" || info.Width != 1920 || len(info.AudioTracks) != 2 {
		t.Errorf("info %+v", info)
	}
	if n := l.katalog.lookups(itemPlayback); n != 1 {
		t.Errorf("%d lookups for a master, five files and /info within seconds, want 1", n)
	}
}

// An answer is served without asking again for 15 s; then served as it is
// while one lookup revalidates it, whose answer the next request gets.
func TestAnswersAreFreshFor15sThenRevalidatedInTheBackground(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	ctx := context.Background()
	dirOf := func() string {
		t.Helper()
		p, _, err := l.packages.itemPackage(ctx, libItem, "", true)
		if err != nil || p == nil {
			t.Fatalf("%+v %v", p, err)
		}
		return filepath.Base(p.dir)
	}
	if got := dirOf(); got != libNew {
		t.Fatalf("served %s", got)
	}
	// Katalog-api now answers the old version as current (a version put
	// back); within 15 s the cached answer stands.
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libOld))})
	now = now.Add(resolveFresh - time.Second)
	if got := dirOf(); got != libNew || l.katalog.lookups(itemPlayback) != 1 {
		t.Fatalf("within 15 s: %s after %d lookups", got, l.katalog.lookups(itemPlayback))
	}
	// Past 15 s: the cached answer once more, the lookup in the background.
	now = now.Add(2 * time.Second)
	if got := dirOf(); got != libNew {
		t.Errorf("stale-while-revalidate served %s, want the cached answer at once", got)
	}
	waitLookups(t, l.packages)
	if n := l.katalog.lookups(itemPlayback); n != 2 {
		t.Errorf("%d lookups, want 2", n)
	}
	if got := dirOf(); got != libOld {
		t.Errorf("after the revalidation: %s, want the new answer", got)
	}
}

// Requests for an item not cached share one lookup.
func TestConcurrentRequestsShareOneLookup(t *testing.T) {
	l := stageLibrary(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, _, err := l.packages.itemPackage(context.Background(), libItem, "", false); err != nil || p == nil {
				t.Errorf("%+v %v", p, err)
			}
		}()
	}
	wg.Wait()
	if n := l.katalog.lookups(itemPlayback); n != 1 {
		t.Errorf("%d lookups for 20 concurrent requests, want 1", n)
	}
}

// While katalog-api fails, the last answer is served for up to 10 min, and
// playback goes on; past that the item cannot be resolved (502 for a
// packaged file, which players retry), until katalog-api answers again.
func TestTheLastAnswerIsServedFor10MinWhileKatalogFails(t *testing.T) {
	logs := captureLog(t)
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	h := &HLSHandler{Packages: l.packages}
	seg := "/api/play/" + libItem + "/v0/seg-00001.m4s"
	if w := get(h, seg); w.Code != 200 {
		t.Fatalf("seg: %d", w.Code)
	}
	l.katalog.setDown(true)
	answered := now
	for _, after := range []time.Duration{resolveFresh + time.Second, 5 * time.Minute, resolveStaleMax - time.Second} {
		now = answered.Add(after)
		if w := get(h, seg); w.Code != 200 || w.Body.String() != "b2b2b2b2 seg-00001.m4s" {
			t.Errorf("%s after the last answer: %d %q", after, w.Code, w.Body)
		}
		waitLookups(t, l.packages)
	}
	if !strings.Contains(logs.String(), "503 db not configured") {
		t.Errorf("the failing lookup is not logged:\n%s", logs)
	}
	now = answered.Add(resolveStaleMax + time.Second)
	if w := get(h, seg); w.Code != 502 {
		t.Errorf("past 10 min: %d %q, want 502", w.Code, w.Body)
	}
	l.katalog.setDown(false)
	if w := get(h, seg); w.Code != 200 {
		t.Errorf("katalog-api back: %d", w.Code)
	}
}

// That the catalog has no such item is remembered for 5 s.
func TestAnUnknownItemIsRememberedFor5s(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	const unknown = "00000000-0000-4000-8000-000000000000"
	path := "/api/v1/items/" + unknown + "/playback"
	for i := 0; i < 3; i++ {
		if _, _, err := l.packages.itemPackage(context.Background(), unknown, "", true); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("%v, want ErrNotFound", err)
		}
	}
	if n := l.katalog.lookups(path); n != 1 {
		t.Errorf("%d lookups within 5 s, want 1", n)
	}
	now = now.Add(resolveNegative)
	_, _, _ = l.packages.itemPackage(context.Background(), unknown, "", true)
	if n := l.katalog.lookups(path); n != 2 {
		t.Errorf("%d lookups after 5 s, want 2", n)
	}
}

// Right after the packager's rename an NFS client may not see the new
// version's .complete yet: the version it superseded plays meanwhile, and
// the new one as soon as its marker is there.
func TestAVersionNotVisiblyCompletePlaysTheOneItSuperseded(t *testing.T) {
	l := stageLibrary(t)
	marker := filepath.Join(l.version(libNew), ".complete")
	b, _ := os.ReadFile(marker)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	h := &HLSHandler{Packages: l.packages}
	// The superseded ladder's master has a SUBTITLES group, the new one's
	// none.
	w := get(h, "/api/play/"+libItem+"/master.m3u8?caps=avc,hvc,aac")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "TYPE=SUBTITLES") {
		t.Fatalf("want the superseded version's master: %d\n%s", w.Code, w.Body)
	}
	if w := get(h, "/api/play/"+libItem+"/v0/seg-00001.m4s"); w.Code != 200 || w.Body.String() != "a1a1a1a1 seg-00001.m4s" {
		t.Errorf("its segment: %d %q", w.Code, w.Body)
	}
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := get(h, "/api/play/"+libItem+"/master.m3u8?caps=avc,hvc,aac"); w.Code != 200 || strings.Contains(w.Body.String(), "TYPE=SUBTITLES") {
		t.Errorf("the new version's marker is there, still the old master: %d\n%s", w.Code, w.Body)
	}
	if w := get(h, "/api/play/"+libItem+"/v0/seg-00001.m4s"); w.Body.String() != "b2b2b2b2 seg-00001.m4s" {
		t.Errorf("and its segment: %q", w.Body)
	}
	// Neither visibly complete: nothing packaged to play.
	for _, v := range []string{libNew, libOld} {
		if err := os.Remove(filepath.Join(l.version(v), ".complete")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		l.packages.forgetComplete(l.version(v))
	}
	if p, _, err := l.packages.itemPackage(context.Background(), libItem, "", true); p != nil || err != nil {
		t.Errorf("no complete version: %+v %v", p, err)
	}
}

// A file missing from the folder answered (its version removed after its
// grace while the answer was cached) resolves the item once more, and the
// file is served from the folder katalog-api answers now.
func TestAMissingFileResolvesTheItemOnceMore(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	h := &HLSHandler{Packages: l.packages}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libOld))})
	if w := get(h, "/api/play/"+libItem+"/v0/seg-00001.m4s"); w.Body.String() != "a1a1a1a1 seg-00001.m4s" {
		t.Fatalf("seg: %d %q", w.Code, w.Body)
	}
	// The old version is removed; katalog-api answers the new one.
	if err := os.RemoveAll(l.version(libOld)); err != nil {
		t.Fatal(err)
	}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew))})
	now = now.Add(resolveRefetchMin) // asked again once the answer is a second old
	for route, want := range map[string]string{
		"v0/seg-00001.m4s":     "b2b2b2b2 seg-00001.m4s",
		"v0/playlist.m3u8":     "#EXTM3U",
		"v0/iframes.m3u8":      "#EXTM3U",
		"v0/init.mp4":          "b2b2b2b2 init.mp4",
		"s0/seg-00001.vtt":     "WEBVTT",
		"a0/seg-00004.m4s":     "b2b2b2b2 seg-00004.m4s",
		"master.m3u8?caps=hvc": "#EXTM3U",
	} {
		if w := get(h, "/api/play/"+libItem+"/"+route); w.Code != 200 || !strings.HasPrefix(w.Body.String(), want) {
			t.Errorf("%s: %d %.40q, want %q", route, w.Code, w.Body, want)
		}
	}
	// What is in no folder is a 404, asked for at most once a second.
	before := l.katalog.lookups(itemPlayback)
	for i := 0; i < 5; i++ {
		if w := get(h, "/api/play/"+libItem+"/v0/seg-00099.m4s"); w.Code != 404 {
			t.Errorf("a segment there is not: %d", w.Code)
		}
	}
	if n := l.katalog.lookups(itemPlayback) - before; n > 1 {
		t.Errorf("%d lookups for five requests of a missing file, want at most 1", n)
	}
}

// What has no package to serve is served none: an unknown id, an item
// without a package (its master falls through to the original, which this
// handler has none of), a series; their files are 404.
func TestNoPackageIsServedForWhatHasNone(t *testing.T) {
	l := stageLibrary(t)
	l.katalog.setItem(catalog.Playback{ItemID: "5e5e5e00-0000-4000-8000-000000000003", Type: "series"})
	l.katalog.setItem(catalog.Playback{ItemID: "0a0a0a0a-0000-4000-8000-000000000004", Original: &catalog.Original{Path: "/nowhere/film.mkv"}})
	h := &HLSHandler{Packages: l.packages, Catalog: fakeKatalog(t, "/nowhere/film.mkv"), MediaRoot: "/nowhere"}
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "5e5e5e00-0000-4000-8000-000000000003", "0a0a0a0a-0000-4000-8000-000000000004"} {
		for _, route := range []string{"master.m3u8", "v0/playlist.m3u8", "v0/init.mp4", "v0/seg-00001.m4s", "trickplay/thumbnails.vtt"} {
			if w := get(h, "/api/play/"+id+"/"+route); w.Code != 404 {
				t.Errorf("%s/%s: %d %q, want 404", id[:8], route, w.Code, w.Body)
			}
		}
	}
}

// A nil resolver resolves no package.
func TestANilResolverResolvesNoPackage(t *testing.T) {
	var res *Resolver
	if p, _, err := res.itemPackage(context.Background(), libItem, "", true); p != nil || !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("%+v %v", p, err)
	}
	res.prune()
	res.forgetComplete("/x")
}

// Answers no request asked for in 10 min are dropped by the sweep.
func TestPruneDropsAnswersNobodyAsksFor(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	if _, _, err := l.packages.itemPackage(context.Background(), libItem, "", true); err != nil {
		t.Fatal(err)
	}
	l.packages.prune()
	if len(l.packages.items) != 1 {
		t.Fatalf("a fresh answer was pruned")
	}
	now = now.Add(resolveStaleMax + time.Second)
	l.packages.prune()
	if len(l.packages.items) != 0 {
		t.Errorf("%d answers after the prune, want 0", len(l.packages.items))
	}
}
