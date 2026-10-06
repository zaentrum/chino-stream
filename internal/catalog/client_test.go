package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// katalog is a katalog-api answering path with status and body, recording
// the requests it got.
func katalog(t *testing.T, answers map[string]string) (*Client, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r)
		body, ok := answers[r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if strings.HasPrefix(body, "503 ") {
			http.Error(w, strings.TrimPrefix(body, "503 "), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &got
}

// An item's playback answer, in the library and before it, as katalog-api
// writes it: a version's id, folder, record and completion time; superseded
// versions newest first; a package from before the library with a null
// version and no completion time; a retired original as null.
func TestPlayback(t *testing.T) {
	c, reqs := katalog(t, map[string]string{
		"/api/v1/items/f001aeff-9c18-4183-b51b-51403af2515e/playback": `{"itemId":"f001aeff-9c18-4183-b51b-51403af2515e","type":"movie",` +
			`"package":{"versionId":"9a2e0000-0000-4000-8000-000000000001","dir":"/var/lib/katalog/movies/f0/f001aeff-9c18-4183-b51b-51403af2515e/versions/9a2e0000-0000-4000-8000-000000000001","record":"package.json","completedAt":"2026-10-06T09:00:00.123456Z"},` +
			`"previous":[{"versionId":"77c10000-0000-4000-8000-000000000002","dir":"/var/lib/katalog/movies/f0/f001aeff-9c18-4183-b51b-51403af2515e/versions/77c10000-0000-4000-8000-000000000002","record":"package.json"}],` +
			`"original":null}`,
		"/api/v1/items/1e9ac700-0000-4000-8000-000000000005/playback": `{"itemId":"1e9ac700-0000-4000-8000-000000000005","type":"episode",` +
			`"package":{"versionId":null,"dir":"/var/lib/katalog/packages/shows/1e/1e9ac700-0000-4000-8000-000000000005","record":"manifest.json"},` +
			`"previous":[],"original":{"path":"/var/lib/katalog/media/Show/S01E01.mkv","sourceId":null}}`,
		"/api/v1/items/5e5e5e00-0000-4000-8000-000000000003/playback": `{"itemId":"5e5e5e00-0000-4000-8000-000000000003","type":"series","package":null,"previous":[],"original":null}`,
	})
	ctx := context.Background()

	p, err := c.Playback(ctx, "f001aeff-9c18-4183-b51b-51403af2515e")
	if err != nil {
		t.Fatal(err)
	}
	want := Playback{
		ItemID: "f001aeff-9c18-4183-b51b-51403af2515e", Type: "movie",
		Package: &PackageRef{
			VersionID:   "9a2e0000-0000-4000-8000-000000000001",
			Dir:         "/var/lib/katalog/movies/f0/f001aeff-9c18-4183-b51b-51403af2515e/versions/9a2e0000-0000-4000-8000-000000000001",
			Record:      "package.json",
			CompletedAt: time.Date(2026, 10, 6, 9, 0, 0, 123456000, time.UTC),
		},
		Previous: []PackageRef{{
			VersionID: "77c10000-0000-4000-8000-000000000002",
			Dir:       "/var/lib/katalog/movies/f0/f001aeff-9c18-4183-b51b-51403af2515e/versions/77c10000-0000-4000-8000-000000000002",
			Record:    "package.json",
		}},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("library answer\n got %+v\nwant %+v", p, want)
	}

	p, err = c.Playback(ctx, "1e9ac700-0000-4000-8000-000000000005")
	if err != nil {
		t.Fatal(err)
	}
	if p.Package == nil || p.Package.VersionID != "" || p.Package.Record != "manifest.json" || !p.Package.CompletedAt.IsZero() ||
		p.Package.Dir != "/var/lib/katalog/packages/shows/1e/1e9ac700-0000-4000-8000-000000000005" || len(p.Previous) != 0 ||
		p.Original == nil || p.Original.Path != "/var/lib/katalog/media/Show/S01E01.mkv" || p.Original.SourceID != "" {
		t.Errorf("legacy answer: %+v %+v %+v", p, p.Package, p.Original)
	}

	if p, err = c.Playback(ctx, "5e5e5e00-0000-4000-8000-000000000003"); err != nil || p.Package != nil || p.Type != "series" {
		t.Errorf("a series: %+v %v", p, err)
	}
	if _, err := c.Playback(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown item: %v, want ErrNotFound", err)
	}
	for _, r := range *reqs {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s sent a bearer", r.URL.Path)
		}
	}
}

func TestExtraPlayback(t *testing.T) {
	c, _ := katalog(t, map[string]string{
		"/api/v1/extras/16aa63f3-0000-4000-8000-000000000009/playback": `{"extraId":"16aa63f3-0000-4000-8000-000000000009",` +
			`"itemId":"ea886f9b-0d06-4f0f-babb-d2a1162f9b01","dir":"/var/lib/katalog/movies/ea/ea886f9b-0d06-4f0f-babb-d2a1162f9b01/extras/16aa63f3-0000-4000-8000-000000000009",` +
			`"record":"package.json","packagedAt":"2026-10-06T08:00:00Z"}`,
	})
	x, err := c.ExtraPlayback(context.Background(), "16aa63f3-0000-4000-8000-000000000009")
	if err != nil {
		t.Fatal(err)
	}
	if x.ItemID != "ea886f9b-0d06-4f0f-babb-d2a1162f9b01" || x.Record != "package.json" ||
		!strings.HasSuffix(x.Dir, "/extras/16aa63f3-0000-4000-8000-000000000009") || !x.PackagedAt.Equal(time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("%+v", x)
	}
	if _, err := c.ExtraPlayback(context.Background(), "16aa63f3-0000-4000-8000-00000000000a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an extra not packaged: %v, want ErrNotFound", err)
	}
}

func TestPackagedIDs(t *testing.T) {
	c, _ := katalog(t, map[string]string{"/api/v1/packaged-ids": `{"ids":["1a","2b"]}`})
	if ids, err := c.PackagedIDs(context.Background()); err != nil || !reflect.DeepEqual(ids, []string{"1a", "2b"}) {
		t.Errorf("%v %v", ids, err)
	}
	c, _ = katalog(t, map[string]string{"/api/v1/packaged-ids": `{"ids":[]}`})
	if ids, err := c.PackagedIDs(context.Background()); err != nil || ids == nil || len(ids) != 0 {
		t.Errorf("none: %#v %v", ids, err)
	}
	// A katalog-api without a database says so; that is an error, not "none".
	c, _ = katalog(t, map[string]string{"/api/v1/packaged-ids": "503 db not configured"})
	if _, err := c.PackagedIDs(context.Background()); err == nil || errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "503 db not configured") {
		t.Errorf("503: %v", err)
	}
}
