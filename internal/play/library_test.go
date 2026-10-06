package play

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// fakeLibrary is a katalog-api answering the stream services' playback
// lookups for what it holds — /api/v1/items/{id}/playback,
// /api/v1/extras/{id}/playback, /api/v1/packaged-ids, "not found" for the
// rest, as katalog-api does — and counting what it is asked.
type fakeLibrary struct {
	mu     sync.Mutex
	items  map[string]catalog.Playback
	extras map[string]catalog.ExtraPlayback
	asked  map[string]int
	// down answers every lookup 503, as a katalog-api without its database.
	down bool
	srv  *httptest.Server
}

func newFakeLibrary(t *testing.T) *fakeLibrary {
	t.Helper()
	f := &fakeLibrary{items: map[string]catalog.Playback{}, extras: map[string]catalog.ExtraPlayback{}, asked: map[string]int{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLibrary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked[r.URL.Path]++
	if f.down {
		http.Error(w, "db not configured", http.StatusServiceUnavailable)
		return
	}
	var answer any
	switch p := r.URL.Path; {
	case p == "/api/v1/packaged-ids":
		ids := []string{}
		for id, a := range f.items {
			if a.Package != nil && (a.Type == "movie" || a.Type == "episode") {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		answer = map[string]any{"ids": ids}
	case strings.HasPrefix(p, "/api/v1/items/") && strings.HasSuffix(p, "/playback"):
		a, ok := f.items[strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/items/"), "/playback")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if a.Previous == nil {
			a.Previous = []catalog.PackageRef{}
		}
		answer = wirePlayback(a)
	case strings.HasPrefix(p, "/api/v1/extras/") && strings.HasSuffix(p, "/playback"):
		x, ok := f.extras[strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/extras/"), "/playback")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		answer = x
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// wirePlayback is a as katalog-api writes it: a version id "" is null, a
// zero completedAt omitted.
func wirePlayback(a catalog.Playback) map[string]any {
	ref := func(r catalog.PackageRef) map[string]any {
		m := map[string]any{"versionId": nil, "dir": r.Dir, "record": r.Record}
		if r.VersionID != "" {
			m["versionId"] = r.VersionID
		}
		if !r.CompletedAt.IsZero() {
			m["completedAt"] = r.CompletedAt
		}
		return m
	}
	out := map[string]any{"itemId": a.ItemID, "type": a.Type, "package": nil, "previous": []any{}, "original": nil}
	if a.Package != nil {
		out["package"] = ref(*a.Package)
	}
	prev := []any{}
	for _, r := range a.Previous {
		prev = append(prev, ref(r))
	}
	out["previous"] = prev
	if a.Original != nil {
		o := map[string]any{"path": a.Original.Path, "sourceId": nil}
		if a.Original.SourceID != "" {
			o["sourceId"] = a.Original.SourceID
		}
		out["original"] = o
	}
	return out
}

// resolver is a Resolver asking f.
func (f *fakeLibrary) resolver() *Resolver { return NewResolver(catalog.New(f.srv.URL)) }

// setItem makes f answer a for its item.
func (f *fakeLibrary) setItem(a catalog.Playback) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.Type == "" {
		a.Type = "movie"
	}
	f.items[a.ItemID] = a
}

// setExtra makes f answer x for its extra.
func (f *fakeLibrary) setExtra(x catalog.ExtraPlayback) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extras[x.ExtraID] = x
}

// setDown makes f fail (true) or answer again.
func (f *fakeLibrary) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// lookups is how often f was asked for path.
func (f *fakeLibrary) lookups(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[path]
}

// legacyLibrary is a fakeLibrary answering what katalog-api answers before
// the library for the package store at root: each item folder
// {category}/{aa}/{id} the item's package, read by its manifest.json, no
// version, no previous ones, no original; each extra under extras/ its
// folder and the title its manifest names.
func legacyLibrary(t *testing.T, root string) *fakeLibrary {
	t.Helper()
	f := newFakeLibrary(t)
	dirs, _ := filepath.Glob(filepath.Join(root, "*", "*", "*"))
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		id, category := filepath.Base(dir), filepath.Base(filepath.Dir(filepath.Dir(dir)))
		if category == "extras" {
			var m struct {
				ParentID string `json:"parentId"`
			}
			if b, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
				_ = json.Unmarshal(b, &m)
			}
			f.setExtra(catalog.ExtraPlayback{ExtraID: id, ItemID: m.ParentID, Dir: dir, Record: "manifest.json"})
			continue
		}
		f.setItem(catalog.Playback{ItemID: id, Package: &catalog.PackageRef{Dir: dir, Record: "manifest.json"}})
	}
	return f
}

// legacyDir is the folder of the test package id in the package store the
// test uses (usePackages).
func legacyDir(t *testing.T, id string) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(PackagesRoot, "*", id[:2], id))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("package %s under %s: %v %v", id, PackagesRoot, dirs, err)
	}
	return dirs[0]
}
