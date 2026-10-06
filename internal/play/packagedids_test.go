package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// waitPackagedIDs waits until res's packaged-ids lookup is done.
func waitPackagedIDs(t *testing.T, res *Resolver) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res.ids.mu.Lock()
		busy := res.ids.refreshing
		res.ids.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the packaged-ids lookup is still in flight")
}

// The packaged ids are katalog-api's, never a walk of the folders: none
// before the first answer (a pod just started; the Zap pager falls back to
// its cold pool) while one lookup runs, then the answer for 60 s; past that
// the ids it had, at once, while one lookup refreshes them; a failed lookup
// keeps them.
func TestPackagedIDsAreTheCatalogs(t *testing.T) {
	f := newFakeLibrary(t)
	pkg := &catalog.PackageRef{Dir: "/x", Record: "manifest.json"}
	f.setItem(catalog.Playback{ItemID: "b-movie", Package: pkg})
	f.setItem(catalog.Playback{ItemID: "a-episode", Type: "episode", Package: pkg})
	f.setItem(catalog.Playback{ItemID: "no-package"})
	res := f.resolver()
	now := time.Now()
	res.now = func() time.Time { return now }

	if ids := res.PackagedIDs(); len(ids) != 0 {
		t.Errorf("cold: %v, want none", ids)
	}
	waitPackagedIDs(t, res)
	if ids := res.PackagedIDs(); !reflect.DeepEqual(ids, []string{"a-episode", "b-movie"}) {
		t.Errorf("%v", ids)
	}
	f.setItem(catalog.Playback{ItemID: "c-new", Package: pkg})
	now = now.Add(packagedIDsTTL - time.Second)
	if ids := res.PackagedIDs(); len(ids) != 2 || f.lookups("/api/v1/packaged-ids") != 1 {
		t.Errorf("within 60 s: %v after %d lookups", ids, f.lookups("/api/v1/packaged-ids"))
	}
	now = now.Add(2 * time.Second)
	if ids := res.PackagedIDs(); len(ids) != 2 {
		t.Errorf("stale: %v, want the ids it had at once", ids)
	}
	waitPackagedIDs(t, res)
	if ids := res.PackagedIDs(); len(ids) != 3 {
		t.Errorf("refreshed: %v", ids)
	}
	captureLog(t)
	f.setDown(true)
	now = now.Add(packagedIDsTTL)
	res.PackagedIDs()
	waitPackagedIDs(t, res)
	if ids := res.PackagedIDs(); len(ids) != 3 {
		t.Errorf("katalog-api down: %v, want the ids it had", ids)
	}

	// The endpoint answers them.
	h := &HLSHandler{Packages: res}
	w := httptest.NewRecorder()
	h.PackagedIDs(w, httptest.NewRequest(http.MethodGet, "/api/play/packaged-ids", nil))
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !reflect.DeepEqual(body.IDs, []string{"a-episode", "b-movie", "c-new"}) {
		t.Errorf("GET /api/play/packaged-ids: %d %q %v", w.Code, w.Body, err)
	}
	if (&HLSHandler{}).Packages.PackagedIDs() == nil {
		t.Error("no resolver: nil, want none")
	}
}

// The Zap pool is filled from the catalog's packaged ids, a library
// version's entry named as the item's records name it (its metadata.json's
// title and release year) and typed as katalog-api answers it; a package
// from before the library as its manifest says. An id without a playable
// package is not an entry.
func TestTheZapPoolFillsFromTheCatalogsPackagedIDs(t *testing.T) {
	l := stageLibrary(t)
	// Long enough for a Zap midpoint.
	rec := filepath.Join(l.version(libNew), "package.json")
	var m map[string]any
	b, _ := os.ReadFile(rec)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["durationMs"] = 600_000
	b, _ = json.Marshal(m)
	if err := os.WriteFile(rec, b, 0o644); err != nil {
		t.Fatal(err)
	}
	stagePackages(t, func(m map[string]any) { m["durationMs"] = 600_000 }, pkgLadder)
	l.katalog.setItem(catalog.Playback{ItemID: pkgLadder, Package: &catalog.PackageRef{Dir: legacyDir(t, pkgLadder), Record: "manifest.json"}})
	l.katalog.setItem(catalog.Playback{ItemID: "0d0d0d0d-0000-4000-8000-00000000000d", Package: &catalog.PackageRef{Dir: filepath.Join(t.TempDir(), "gone"), Record: "manifest.json"}})
	h := &HLSHandler{Packages: l.packages}
	t.Cleanup(func() {
		zapPoolMu.Lock()
		for _, e := range zapPool {
			UnpinPaths(e.Paths)
		}
		zapPool, zapRecently = nil, map[string]time.Time{}
		zapPoolMu.Unlock()
	})
	l.packages.PackagedIDs()
	waitPackagedIDs(t, l.packages)
	refillZapPool(h)

	zapPoolMu.Lock()
	entries := map[string]zapPoolEntry{}
	for _, e := range zapPool {
		entries[e.ItemID] = *e
	}
	zapPoolMu.Unlock()
	var ids []string
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if want := []string{libItem, pkgLadder}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("pool %v, want %v", ids, want)
	}
	if e := entries[libItem]; e.Title != "Clip (Remastered)" || e.Year != 2026 || e.Type != "movie" || e.DurationMs != 600_000 || len(e.Paths) == 0 {
		t.Errorf("library entry %+v", e)
	}
	if e := entries[pkgLadder]; e.Title != "Clip" || e.Year != 2026 || e.Type != "movie" {
		t.Errorf("package store entry %+v", e)
	}
}

// libraryTitle falls back from metadata.json to item.json, and to nothing.
func TestLibraryTitle(t *testing.T) {
	l := stageLibrary(t)
	if title, year := libraryTitle(l.version(libNew)); title != "Clip (Remastered)" || year != 2026 {
		t.Errorf("%q %d", title, year)
	}
	if err := os.Remove(filepath.Join(l.item, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	if title, year := libraryTitle(l.version(libNew)); title != "Clip" || year != 0 {
		t.Errorf("item.json: %q %d", title, year)
	}
	if title, _ := libraryTitle(filepath.Join(t.TempDir(), "versions", "x")); title != "" {
		t.Errorf("no records: %q", title)
	}
}
