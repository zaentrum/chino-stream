package play

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/auth"
)

// These tests serve the ladder package (pkgLadder) as the router does —
// behind the auth middleware, with chino-api's stream tokens — to sessions
// whose stream token, OIDC token and start time differ, and hold every
// answer to the one the code before the cache was shared gives the same
// request from a cache of its own: the oracle, the same handler with no
// session parameters (sessionParams empty), which keys, rewrites and serves
// each request by its whole query, as that code did.

const testStreamKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func testStreamToken(t testing.TB, user string, exp time.Time) string {
	t.Helper()
	key, _ := base64.StdEncoding.DecodeString(testStreamKey)
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + strconv.FormatInt(exp.Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// testIssuer answers OIDC discovery, so the verifier starts with auth on.
func testIssuer(t testing.TB) string {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks",
			"authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// sessionsServer serves two copies of the ladder package behind the auth
// middleware: one to the sessions (the cache shared), one to the oracle
// (another folder, so their entries never meet), each rendition given an
// init segment.
type sessionsServer struct {
	h            http.Handler
	hls          *HLSHandler
	dir, oracleD string
	item, oItem  string
}

func newSessionsServer(t *testing.T) *sessionsServer {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("testdata", "packages", "movies", pkgLadder[:2], pkgLadder)
	s := &sessionsServer{item: pkgLadder, oItem: "1adde700-0000-4000-8000-0000000000aa"}
	for id, dir := range map[string]*string{s.item: &s.dir, s.oItem: &s.oracleD} {
		*dir = filepath.Join(root, "movies", id[:2], id)
		copyTree(t, src, *dir)
		for _, r := range []string{"v0", "v1", "v2", "a0", "a1", "a2"} {
			writeFile(t, filepath.Join(*dir, "hls", r, "init.mp4"), "the init of "+r)
		}
	}
	h := &HLSHandler{Packages: usePackages(t, root), CacheDir: t.TempDir()}
	verifier, err := auth.New(context.Background(), testIssuer(t), "chino-web", true)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner(testStreamKey)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(verifier.WithStreamSigner(signer).Middleware)
	r.Route("/api/play/{itemId}", h.Routes)
	s.h, s.hls = r, h
	return s
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		writeFile(t, filepath.Join(dst, rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type answer struct {
	code int
	body string
	head http.Header
}

func (s *sessionsServer) serve(w http.ResponseWriter, method, item, path, query string, h http.Header) {
	req := httptest.NewRequest(method, "/api/play/"+item+"/"+path+"?"+query, nil)
	for k, v := range h {
		req.Header[k] = v
	}
	s.h.ServeHTTP(w, req)
}

// askRaw is ask with the query as it comes, put in the request's URL as it
// is (a byte no client may send unescaped, a '#' a listener passes on).
func (s *sessionsServer) askRaw(item, path, rawQuery string) answer {
	req := httptest.NewRequest(http.MethodGet, "/api/play/"+item+"/"+path, nil)
	req.URL.RawQuery = rawQuery
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, req)
	return answer{w.Code, w.Body.String(), w.Header()}
}

// oracleRaw is oracle for askRaw.
func (s *sessionsServer) oracleRaw(path, rawQuery string) answer {
	saved := sessionParams
	sessionParams = map[string]bool{}
	defer func() { sessionParams = saved }()
	return s.askRaw(s.oItem, path, rawQuery)
}

// ask is a session's request, served from the shared cache.
func (s *sessionsServer) ask(method, path, query string, h http.Header) answer {
	w := httptest.NewRecorder()
	s.serve(w, method, s.item, path, query, h)
	return answer{w.Code, w.Body.String(), w.Header()}
}

// oracle is the answer the code before the cache was shared gives the same
// request: no session parameters, so the request is keyed, rewritten and
// served by its whole query, from an entry its query alone made.
func (s *sessionsServer) oracle(method, path, query string, h http.Header) answer {
	saved := sessionParams
	sessionParams = map[string]bool{}
	defer func() { sessionParams = saved }()
	w := httptest.NewRecorder()
	s.serve(w, method, s.oItem, path, query, h)
	return answer{w.Code, w.Body.String(), w.Header()}
}

// session is a client session: its stream token, its OIDC token (none, or
// one riding beside the stream token, which then authorises), its start.
type session struct{ name, query string }

func sessionsOf(t *testing.T) []session {
	exp := time.Now().Add(time.Hour)
	a := testStreamToken(t, "user-1", exp)
	b := testStreamToken(t, "user-2", exp)
	c := testStreamToken(t, "user-1", exp.Add(time.Minute)) // the same user, another tab
	d := testStreamToken(t, "user-3", exp)
	return []session{
		{"A", "stream=" + a + "&caps=avc,hvc,aac,eac3&t=0"},
		{"B", "stream=" + b + "&caps=avc,hvc,aac,eac3&t=731"},
		{"C", "stream=" + c + "&caps=avc,hvc,aac,eac3&t=5"},
		{"D", "stream=" + d + "&token=opaque-oidc-D&caps=avc,hvc,aac,eac3&t=1800"},
	}
}

// valuesOf are a session's own values: what no other session may get.
func valuesOf(q string) []string {
	var out []string
	for _, kv := range strings.Split(q, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && (k == "stream" || k == "token" || k == "t") {
			out = append(out, k+"="+v)
		}
	}
	return out
}

var uriAttr = regexp.MustCompile(`URI="([^"]*)"`)

// childURIs is every URI a playlist body names: its media and variant
// lines, and the URI attributes of its tags (EXT-X-MAP, EXT-X-MEDIA,
// EXT-X-I-FRAME-STREAM-INF).
func childURIs(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			for _, m := range uriAttr.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
		default:
			out = append(out, line)
		}
	}
	return out
}

var sessionPaths = []string{"master.m3u8", "v0/playlist.m3u8", "v1/playlist.m3u8", "v2/playlist.m3u8",
	"a0/playlist.m3u8", "a2/playlist.m3u8", "s0/playlist.m3u8", "s1/playlist.m3u8", "v0/iframes.m3u8"}

// checkSession holds a session's answer to the oracle's: the bytes, the
// status, the headers but the ETag; and every URI of the body to the
// session's own values and to none of another's.
func checkSession(t *testing.T, label string, got, want answer, own session, all []session) {
	t.Helper()
	if got.code != want.code || got.body != want.body {
		t.Fatalf("%s: %d %d bytes, the code before gives %d %d bytes\n got %q\nwant %q", label, got.code, len(got.body),
			want.code, len(want.body), head(got.body), head(want.body))
	}
	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if got.head.Get(k) != want.head.Get(k) {
			t.Errorf("%s: %s %q, the code before %q", label, k, got.head.Get(k), want.head.Get(k))
		}
	}
	if got.code != http.StatusOK {
		return
	}
	uris := childURIs(got.body)
	if len(uris) == 0 {
		t.Fatalf("%s: no URI in %q", label, head(got.body))
	}
	for _, u := range uris {
		for _, v := range valuesOf(own.query) {
			if !strings.Contains(u, v) {
				t.Errorf("%s: %s lacks the session's %s", label, u, v)
			}
		}
		for _, o := range all {
			if o.name == own.name {
				continue
			}
			for _, v := range valuesOf(o.query) {
				if strings.HasPrefix(v, "stream=") && strings.Contains(u, strings.TrimPrefix(v, "stream=")) {
					t.Errorf("%s: %s carries session %s's stream token", label, u, o.name)
				}
			}
		}
	}
}

func head(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Sessions interleaved on the same warm entries — the master, the
// rendition, audio and subtitle playlists, the I-frame playlist — are each
// served the bytes the code before the cache was shared serves them from a
// cache of their own, every URI carrying the session's own stream token,
// OIDC token and start and none of another session's; HEAD as before. A
// response that failed to be written, and a playlist missing for a moment,
// change nothing of what the next sessions get.
func TestASessionIsNeverServedAnotherSessionsValues(t *testing.T) {
	s := newSessionsServer(t)
	sessions := sessionsOf(t)
	want := map[string]answer{}
	for _, se := range sessions {
		for _, p := range sessionPaths {
			want[se.name+p] = s.oracle(http.MethodGet, p, se.query, nil)
			want["HEAD"+se.name+p] = s.oracle(http.MethodHead, p, se.query, nil)
		}
	}
	order := []int{0, 1, 0, 2, 1, 3, 0, 3, 2}
	for round := 0; round < 2; round++ {
		for _, i := range order {
			se := sessions[i]
			for _, p := range sessionPaths {
				checkSession(t, fmt.Sprintf("round %d, session %s, %s", round, se.name, p), s.ask(http.MethodGet, p, se.query, nil),
					want[se.name+p], se, sessions)
				if got, w := s.ask(http.MethodHead, p, se.query, nil), want["HEAD"+se.name+p]; got.code != w.code || got.body != w.body {
					t.Errorf("session %s, HEAD %s: %d %q, the code before %d %q", se.name, p, got.code, got.body, w.code, w.body)
				}
			}
		}
	}
	// a response whose write failed (the client went away): the cached
	// bodies are as they were, and the next sessions get their own bytes
	before := cachedBodies()
	for _, p := range sessionPaths {
		s.serve(&failingWriter{header: http.Header{}}, http.MethodGet, s.item, p, sessions[0].query, nil)
	}
	if after := cachedBodies(); !equalBodies(before, after) {
		t.Errorf("a failed write changed the cached bodies")
	}
	for _, i := range []int{1, 0, 3} {
		for _, p := range sessionPaths {
			checkSession(t, "after a failed write, session "+sessions[i].name+", "+p, s.ask(http.MethodGet, p, sessions[i].query, nil),
				want[sessions[i].name+p], sessions[i], sessions)
		}
	}
	// the playlist gone for a moment: refused as before, then the next
	// sessions get their own bytes
	pl := filepath.Join(s.dir, "hls", "v1", "playlist.m3u8")
	saved, _ := os.ReadFile(pl)
	_ = os.Remove(pl)
	gone := s.ask(http.MethodGet, "v1/playlist.m3u8", sessions[2].query, nil)
	_ = os.Remove(filepath.Join(s.oracleD, "hls", "v1", "playlist.m3u8"))
	wantGone := s.oracle(http.MethodGet, "v1/playlist.m3u8", sessions[2].query, nil)
	if gone.code != wantGone.code || gone.code == http.StatusOK {
		t.Errorf("the playlist gone: %d, the code before %d", gone.code, wantGone.code)
	}
	writeFile(t, pl, string(saved))
	writeFile(t, filepath.Join(s.oracleD, "hls", "v1", "playlist.m3u8"), string(saved))
	for _, i := range []int{2, 3, 1} {
		se := sessions[i]
		checkSession(t, "after the playlist came back, session "+se.name, s.ask(http.MethodGet, "v1/playlist.m3u8", se.query, nil),
			s.oracle(http.MethodGet, "v1/playlist.m3u8", se.query, nil), se, sessions)
	}
}

type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("the client went away") }

func cachedBodies() map[playlistKey][]byte {
	out := map[playlistKey][]byte{}
	playlistCache.Range(func(k, v any) bool {
		out[k.(playlistKey)] = bytes.Clone(v.(*playlistCacheEntry).body)
		return true
	})
	return out
}

func equalBodies(a, b map[playlistKey][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// A session's values — its start, its OIDC token, its stream token — are
// never written into a cached body or a key: not when they hold the text
// of a mark (percent-encoded: a NUL is no byte of a URL), a %, an encoded
// & or # or line break, an encoded encoding; and the next session's bytes
// are what the code before gives it. A query that holds a mark's bytes
// themselves — no request can, a caller could — is served as the code
// before served it, keyed by its whole query.
func TestASessionsValuesNeverEnterTheCache(t *testing.T) {
	s := newSessionsServer(t)
	tok := testStreamToken(t, "user-1", time.Now().Add(time.Hour))
	injected := []string{"INJ%000%00", "INJ%001%00x", "INJ%25", "INJ%26caps%3Dhvc", "INJ%23frag", "INJ%0Anewline%0D",
		"INJ%250A", "INJ%E2%80%A8", "INJ+plus", "INJ%5Cslash%22quote"}
	for _, v := range injected {
		for _, q := range []string{"stream=" + tok + "&caps=avc,hvc,aac,eac3&t=" + v,
			"stream=" + tok + "&token=" + v + "&caps=avc,hvc,aac,eac3&t=" + v} {
			for _, p := range sessionPaths {
				got := s.ask(http.MethodGet, p, q, nil)
				if got.code != http.StatusOK || !strings.Contains(got.body, v) {
					t.Fatalf("%s with %q: %d %q", p, q, got.code, head(got.body))
				}
			}
		}
	}
	playlistCache.Range(func(k, v any) bool {
		if key := k.(playlistKey); keyHolds(key, "INJ") || keyHolds(key, tok) {
			t.Errorf("a key holds a session's value: %q", key)
		}
		if b := v.(*playlistCacheEntry).body; bytes.Contains(b, []byte("INJ")) || bytes.Contains(b, []byte(tok)) {
			t.Errorf("the body of %q holds a session's value", k)
		}
		return true
	})
	playlistVariants.Range(func(k, v any) bool {
		if b := k.(variantBase); strings.Contains(b.path+b.query, "INJ") || strings.Contains(b.path+b.query, tok) {
			t.Errorf("a variant's key holds a session's value: %q", b)
		}
		return true
	})
	all := sessionsOf(t)
	next := all[1]
	for _, p := range sessionPaths {
		checkSession(t, "the next session, "+p, s.ask(http.MethodGet, p, next.query, nil), s.oracle(http.MethodGet, p, next.query, nil),
			next, all)
	}
	// a mark's own bytes in the query: as the code before, by its whole query
	pl := filepath.Join(s.dir, "hls", "v0", "playlist.m3u8")
	for _, q := range []string{"stream=TOK&caps=" + sessionMark(0), "caps=" + sessionMark(0) + "&stream=TOK", "stream=" + sessionMark(1) + "&t=" + sessionMark(0)} {
		marked, values, keep := templateQuery(q)
		if marked != q || values != nil || keep {
			t.Errorf("a query holding a mark's bytes is templated or kept: %q -> %q %q %v", q, marked, values, keep)
		}
		n := cacheEntries()
		got := httptest.NewRecorder()
		servePlaylistCachedVariant(got, httptest.NewRequest(http.MethodGet, "/p", nil), pl, q, nil, "", nil)
		if want := rewriteM3U8URIs(string(mustRead(t, pl)), q); got.Body.String() != want {
			t.Errorf("a query holding a mark's bytes, %q: served\n%q\nthe code before\n%q", q, head(got.Body.String()), head(want))
		}
		if cacheEntries() != n {
			t.Errorf("a query holding a mark's bytes is kept")
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Each session gets its own ETag, the same one each time it asks, also
// once its entry was dropped and made again. Its own ETag is answered 304,
// with no body; another session's ETag sent with its token is answered
// 200 with its own bytes.
func TestEachSessionHasItsOwnETag(t *testing.T) {
	s := newSessionsServer(t)
	sessions := sessionsOf(t)
	for _, p := range []string{"master.m3u8", "v0/playlist.m3u8", "a0/playlist.m3u8", "s0/playlist.m3u8"} {
		etags := map[string]string{}
		seen := map[string]string{}
		for _, se := range sessions {
			got := s.ask(http.MethodGet, p, se.query, nil)
			e := got.head.Get("ETag")
			if e == "" {
				t.Fatalf("%s, session %s: no ETag", p, se.name)
			}
			if other, dup := seen[e]; dup {
				t.Errorf("%s: sessions %s and %s share the ETag %s", p, other, se.name, e)
			}
			seen[e], etags[se.name] = se.name, e
		}
		for _, se := range sessions {
			own := s.ask(http.MethodGet, p, se.query, http.Header{"If-None-Match": {etags[se.name]}})
			if own.code != http.StatusNotModified || own.body != "" {
				t.Errorf("%s, session %s with its own ETag: %d %d bytes", p, se.name, own.code, len(own.body))
			}
			for _, other := range sessions {
				if other.name == se.name {
					continue
				}
				got := s.ask(http.MethodGet, p, se.query, http.Header{"If-None-Match": {etags[other.name]}})
				checkSession(t, p+", session "+se.name+" with session "+other.name+"'s ETag", got, s.oracle(http.MethodGet, p, se.query, nil), se, sessions)
				if got.head.Get("ETag") != etags[se.name] {
					t.Errorf("%s, session %s with %s's ETag: given %s, its own is %s", p, se.name, other.name, got.head.Get("ETag"), etags[se.name])
				}
			}
		}
		pruneServeCaches(time.Now().Add(time.Hour))
		for _, se := range sessions {
			if again := s.ask(http.MethodGet, p, se.query, http.Header{"If-None-Match": {etags[se.name]}}); again.code != http.StatusNotModified {
				t.Errorf("%s, session %s with its own ETag once its entry was made again: %d", p, se.name, again.code)
			}
		}
	}
}

// The cache holds playlistCacheBudget bytes at most: over it, the least
// recently served entries go first, down to 80%, each with its variant;
// what was not served since the sweeper's cutoff goes with its variant and
// the init versions and EXT-X-MAP lists kept by path. Its count of bytes is
// the bodies it holds. An entry dropped and asked for again is made again,
// byte for byte, for the same session.
func TestThePlaylistCacheIsBounded(t *testing.T) {
	resetPackageCaches()
	t.Cleanup(resetPackageCaches)
	body := bytes.Repeat([]byte("x"), 10<<10)
	keyOf := func(i int) playlistKey { return playlistKey{fmt.Sprintf("/p%d", i), "q", fmt.Sprintf("init=v%d", i)} }
	saved := playlistCacheBudget
	playlistCacheBudget = 10 * keyOf(0).size(&playlistCacheEntry{body: body}) // ten entries, keys and bodies
	t.Cleanup(func() { playlistCacheBudget = saved })
	clock := time.Now().UnixNano()
	store := func(key playlistKey) *playlistCacheEntry {
		e := &playlistCacheEntry{body: body}
		storePlaylist(key, e)
		clock++
		e.lastTouch.Store(clock)
		return e
	}
	for i := 0; i < 10; i++ {
		store(keyOf(i))
		playlistVariants.Store(variantBase{keyOf(i).path, "q"}, keyOf(i).variant)
	}
	if n := cacheEntries(); n != 10 {
		t.Fatalf("%d entries within the budget, ten expected", n)
	}
	// /p0 served again: the most recent
	if v, ok := playlistCache.Load(keyOf(0)); ok {
		clock++
		v.(*playlistCacheEntry).lastTouch.Store(clock)
	}
	store(playlistKey{"/p10", "q", ""}) // over the budget: down to 80% (8 entries)
	for i, want := range map[int]bool{0: true, 1: false, 2: false, 3: false, 4: true, 9: true} {
		if _, got := playlistCache.Load(keyOf(i)); got != want {
			t.Errorf("/p%d kept %v, want %v (the least recently served go first)", i, got, want)
		}
		if _, variant := playlistVariants.Load(variantBase{keyOf(i).path, "q"}); variant != want {
			t.Errorf("/p%d's variant kept %v, want %v", i, variant, want)
		}
	}
	if held, b := heldBytes(), playlistCacheBytes.Load(); held != b || b > playlistCacheBudget*8/10 {
		t.Errorf("the entries hold %d bytes, keys and bodies, the count says %d", held, b)
	}
	// the sweeper's cutoff, over a real playlist and its init
	s := newSessionsServer(t)
	se := sessionsOf(t)[0]
	first := s.ask(http.MethodGet, "v0/playlist.m3u8", se.query, nil)
	pruneServeCaches(time.Now().Add(time.Hour))
	if n := cacheEntries(); n != 0 || playlistCacheBytes.Load() != 0 {
		t.Errorf("after a cutoff past every use: %d entries, %d bytes", n, playlistCacheBytes.Load())
	}
	for name, m := range map[string]*sync.Map{"playlistVariants": &playlistVariants, "mapURIsCache": &mapURIsCache, "initVersionCache": &initVersionCache} {
		if n := countOf(m); n != 0 {
			t.Errorf("%s keeps %d entries past the cutoff", name, n)
		}
	}
	if again := s.ask(http.MethodGet, "v0/playlist.m3u8", se.query, nil); again.body != first.body || again.head.Get("ETag") != first.head.Get("ETag") {
		t.Errorf("the entry made again differs for the same session")
	}
	dropPlaylist(onlyKey(t))
	if again := s.ask(http.MethodGet, "v0/playlist.m3u8", se.query, nil); again.body != first.body {
		t.Errorf("the entry dropped and made again differs for the same session")
	}
}

func cacheEntries() int { return countOf(&playlistCache) }

func countOf(m *sync.Map) (n int) {
	m.Range(func(_, _ any) bool { n++; return true })
	return
}

// heldBytes is what playlistCache's entries hold: their keys and bodies.
func heldBytes() (n int64) {
	playlistCache.Range(func(k, v any) bool { n += k.(playlistKey).size(v.(*playlistCacheEntry)); return true })
	return
}

func keyHolds(k playlistKey, s string) bool {
	return strings.Contains(k.path, s) || strings.Contains(k.query, s) || strings.Contains(k.variant, s)
}

func onlyKey(t *testing.T) playlistKey {
	t.Helper()
	var keys []playlistKey
	playlistCache.Range(func(k, _ any) bool { keys = append(keys, k.(playlistKey)); return true })
	if len(keys) != 1 {
		t.Fatalf("%d entries, one expected", len(keys))
	}
	return keys[0]
}

// Sessions served while entries are stored over the budget, evicted and
// swept, all at once (run with -race): every session gets the bytes the
// code before gives it, and the count of bytes is the bodies the cache
// holds when it is done.
func TestThePlaylistCacheHoldsUnderLoad(t *testing.T) {
	s := newSessionsServer(t)
	sessions := sessionsOf(t)
	saved := playlistCacheBudget
	playlistCacheBudget = 600 << 10
	t.Cleanup(func() { playlistCacheBudget = saved })
	want := map[string]answer{}
	for _, se := range sessions {
		for _, p := range sessionPaths {
			want[se.name+p] = s.oracle(http.MethodGet, p, se.query, nil)
		}
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	var wg sync.WaitGroup
	var served atomic.Int64
	errs := make(chan string, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for time.Now().Before(deadline) {
				se, p := sessions[rng.Intn(len(sessions))], sessionPaths[rng.Intn(len(sessionPaths))]
				got := s.ask(http.MethodGet, p, se.query, nil)
				served.Add(1)
				if got.code != want[se.name+p].code || got.body != want[se.name+p].body {
					select {
					case errs <- fmt.Sprintf("session %s, %s: %d %d bytes, the code before %d %d bytes", se.name, p, got.code,
						len(got.body), want[se.name+p].code, len(want[se.name+p].body)):
					default:
					}
				}
			}
		}(int64(w))
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			big := bytes.Repeat([]byte("y"), 64<<10)
			for i := 0; time.Now().Before(deadline); i++ {
				storePlaylist(playlistKey{fmt.Sprintf("/filler%d-%d", w, i%40), "q", ""}, &playlistCacheEntry{body: big})
			}
		}(w)
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				pruneServeCaches(time.Now().Add(-time.Millisecond))
				evictPlaylistCache()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if b, held := playlistCacheBytes.Load(), heldBytes(); b != held {
		t.Errorf("the count says %d bytes, the cache holds %d, keys and bodies", b, held)
	}
	if served.Load() < 100 {
		t.Errorf("only %d requests served", served.Load())
	}
}

// Pruning the init versions and EXT-X-MAP lists during a playback does not
// make it fetch another init: the next rendition playlist names the version
// it named before for the same file — also one renamed into place with the
// same bytes — and the init asked for by it is served immutable with those
// bytes; a new version only once the init's bytes changed.
func TestPruningDoesNotChangeAPlaybacksInit(t *testing.T) {
	s := newSessionsServer(t)
	se := sessionsOf(t)[0]
	versionIn := func() string {
		t.Helper()
		got := s.ask(http.MethodGet, "v0/playlist.m3u8", se.query, nil)
		m := regexp.MustCompile(`#EXT-X-MAP:URI="init\.mp4\?[^"]*init=([0-9a-f]+)"`).FindStringSubmatch(got.body)
		if got.code != http.StatusOK || m == nil {
			t.Fatalf("the rendition playlist: %d %q", got.code, head(got.body))
		}
		return m[1]
	}
	initAs := func(version string) answer {
		t.Helper()
		return s.ask(http.MethodGet, "v0/init.mp4", se.query+"&init="+version, nil)
	}
	first := versionIn()
	if first != initVersionOf([]byte("the init of v0")) {
		t.Fatalf("the init's version %s, its bytes' %s", first, initVersionOf([]byte("the init of v0")))
	}
	for i := 0; i < 3; i++ {
		pruneServeCaches(time.Now().Add(time.Hour))
		if got := versionIn(); got != first {
			t.Errorf("after a prune the playlist names %s, before %s", got, first)
		}
		pruneServeCaches(time.Now().Add(time.Hour))
		if a := initAs(first); a.code != http.StatusOK || a.body != "the init of v0" || !strings.Contains(a.head.Get("Cache-Control"), "immutable") {
			t.Errorf("the init asked for by its version after a prune: %d %q %q", a.code, a.body, a.head.Get("Cache-Control"))
		}
	}
	// the same bytes renamed into place: another file, the same version;
	// then other bytes of the same size and modification time (a store
	// whose times are coarse): a new version, and the old one no-store
	init := filepath.Join(s.dir, "hls", "v0", "init.mp4")
	st, err := os.Stat(init)
	if err != nil {
		t.Fatal(err)
	}
	place := func(body string) {
		t.Helper()
		tmp := init + ".1-a.mending"
		writeFile(t, tmp, body)
		if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, init); err != nil {
			t.Fatal(err)
		}
	}
	place("the init of v0")
	pruneServeCaches(time.Now().Add(time.Hour))
	if got := versionIn(); got != first {
		t.Errorf("the same bytes renamed into place: %s, before %s", got, first)
	}
	place("THE INIT OF V0")
	pruneServeCaches(time.Now().Add(time.Hour))
	if got := versionIn(); got != initVersionOf([]byte("THE INIT OF V0")) {
		t.Errorf("other bytes: %s, want %s", got, initVersionOf([]byte("THE INIT OF V0")))
	}
	if a := initAs(first); a.body != "THE INIT OF V0" || a.head.Get("Cache-Control") != "no-store" {
		t.Errorf("the old version after the change: %q %q", a.body, a.head.Get("Cache-Control"))
	}
}

// One cache entry serves every session of a playlist: the sessions' stream
// tokens, OIDC tokens and starts are no part of its key. Another caps or q
// is another entry.
func TestAPlaylistIsCachedOnceForEverySession(t *testing.T) {
	s := newSessionsServer(t)
	sessions := sessionsOf(t)
	for _, se := range sessions {
		for _, p := range []string{"master.m3u8", "v0/playlist.m3u8"} {
			if got := s.ask(http.MethodGet, p, se.query, nil); got.code != http.StatusOK {
				t.Fatalf("session %s, %s: %d", se.name, p, got.code)
			}
		}
	}
	// the sessions' queries differ in their values alone, but D's, which
	// has a token= besides: two keys of each path
	if n := cacheEntries(); n != 4 {
		t.Errorf("%d sessions left %d entries, four expected (two paths, two shapes of query)", len(sessions), n)
	}
	tok := testStreamToken(t, "user-9", time.Now().Add(time.Hour))
	for _, q := range []string{"stream=" + tok + "&caps=avc,aac&t=0", "stream=" + tok + "&caps=avc,hvc,aac,eac3&q=low&t=0"} {
		s.ask(http.MethodGet, "v0/playlist.m3u8", q, nil)
	}
	if n := cacheEntries(); n != 6 {
		t.Errorf("another caps and another q: %d entries, six expected", n)
	}
}

// A master is kept by the record its body was built from as well: one built
// while the record could not be read is not served once it can, and one
// built from the record before it changed is not served after; the next
// session gets the master the code before gives it from the record as it is.
func TestAMastersEntryIsKeptByItsRecord(t *testing.T) {
	for _, c := range []string{"the record unreadable at the first fill", "the record changed after the fill"} {
		t.Run(c, func(t *testing.T) {
			s := newSessionsServer(t)
			sessions := sessionsOf(t)
			a, b := sessions[0], sessions[1]
			rec, oRec := filepath.Join(s.dir, "manifest.json"), filepath.Join(s.oracleD, "manifest.json")
			good := string(mustRead(t, rec))
			changed := strings.Replace(good, `"encoder": "copy"`, `"encoder": "libx265"`, 1)
			if changed == good {
				t.Fatal("the record names no rung copied")
			}
			later := time.Now().Add(5 * time.Second)
			if strings.HasPrefix(c, "the record unreadable") {
				writeFile(t, rec, "{ not a record")
				if got := s.ask(http.MethodGet, "master.m3u8", a.query, nil); got.code != http.StatusOK {
					t.Fatalf("the master with its record unreadable: %d", got.code)
				}
				writeFile(t, rec, good)
				_ = os.Chtimes(rec, later, later)
			} else {
				if got := s.ask(http.MethodGet, "master.m3u8", a.query, nil); !strings.Contains(got.body, `NAME="Original"`) {
					t.Fatalf("the master before the change names no rung Original: %q", head(got.body))
				}
				for _, p := range []string{rec, oRec} {
					writeFile(t, p, changed)
					_ = os.Chtimes(p, later, later)
				}
			}
			want := s.oracle(http.MethodGet, "master.m3u8", b.query, nil)
			checkSession(t, c+", the next session's master", s.ask(http.MethodGet, "master.m3u8", b.query, nil), want, b, sessions)
			if named := strings.Contains(want.body, `NAME="Original"`); named == strings.HasSuffix(c, "after the fill") {
				t.Fatalf("the code before names the copied rung Original: %v", named)
			}
		})
	}
}

// A query that holds a '#' — a listener passes one on in the query — keeps
// an entry of its own: not the entry of the playlist with a variant whose
// key would read the same. The next session gets its own playlist, the
// init named by its version, no fragment in a URI.
func TestAQueryHoldingAHashKeepsAnEntryOfItsOwn(t *testing.T) {
	s := newSessionsServer(t)
	exp := time.Now().Add(time.Hour)
	tokA, tokB := testStreamToken(t, "user-1", exp), testStreamToken(t, "user-2", exp)
	version := initVersionOf([]byte("the init of v0"))
	init := filepath.Join(s.dir, "hls", "v0", "init.mp4")
	saved := mustRead(t, init)
	_ = os.Remove(init)
	if got := s.askRaw(s.item, "v0/playlist.m3u8", "stream="+tokA+"&caps=avc,hvc,aac,eac3#init=init.mp4="+version+";"); got.code != http.StatusOK {
		t.Fatalf("the query with a '#': %d", got.code)
	}
	writeFile(t, init, string(saved))
	b := session{"B", "stream=" + tokB + "&caps=avc,hvc,aac,eac3"}
	got := s.ask(http.MethodGet, "v0/playlist.m3u8", b.query, nil)
	checkSession(t, "the next session", got, s.oracle(http.MethodGet, "v0/playlist.m3u8", b.query, nil), b,
		[]session{b, {"A", "stream=" + tokA + "&caps=avc,hvc,aac,eac3"}})
	if strings.Contains(got.body, "#init=init.mp4=") || !strings.Contains(got.body, "init="+version) {
		t.Errorf("the next session's playlist: %q", head(got.body))
	}
}

// The sweeper (h.sweep) prunes the serve caches: what was not used since
// its cutoff goes from the playlist cache, its variants, the init versions,
// the EXT-X-MAP lists and the package records, and the load lock of a path
// the packaged cache holds nothing of, unless someone holds it. The next
// session is served as before.
func TestTheSweeperPrunesTheServeCaches(t *testing.T) {
	s := newSessionsServer(t)
	sessions := sessionsOf(t)
	for _, se := range sessions {
		for _, p := range sessionPaths {
			s.ask(http.MethodGet, p, se.query, nil)
		}
	}
	packagedLoadLock("/a path no longer cached")
	held := packagedLoadLock("/a path being read")
	held.Lock()
	maps := map[string]*sync.Map{"playlistCache": &playlistCache, "playlistVariants": &playlistVariants,
		"mapURIsCache": &mapURIsCache, "initVersionCache": &initVersionCache, "manifestCache": &manifestCache}
	for name, m := range maps {
		if countOf(m) == 0 {
			t.Fatalf("%s holds nothing before the sweep", name)
		}
	}
	s.hls.sweep(-time.Hour) // a cutoff past every use
	for name, m := range maps {
		if n := countOf(m); n != 0 {
			t.Errorf("%s keeps %d entries past the sweeper's cutoff", name, n)
		}
	}
	if b := playlistCacheBytes.Load(); b != 0 {
		t.Errorf("the playlist cache counts %d bytes, empty", b)
	}
	if _, ok := packagedCacheLoading.Load("/a path no longer cached"); ok {
		t.Errorf("the load lock of a path no longer cached is kept")
	}
	if _, ok := packagedCacheLoading.Load("/a path being read"); !ok {
		t.Errorf("a load lock someone holds is taken away")
	}
	held.Unlock()
	s.hls.sweep(-time.Hour)
	if _, ok := packagedCacheLoading.Load("/a path being read"); ok {
		t.Errorf("a load lock let go is kept past the next sweep")
	}
	for _, p := range sessionPaths {
		checkSession(t, "after the sweep, "+p, s.ask(http.MethodGet, p, sessions[1].query, nil),
			s.oracle(http.MethodGet, p, sessions[1].query, nil), sessions[1], sessions)
	}
}

// The budget counts an entry's key as well as its body: many long queries
// on a playlist that names no URI — a key all but its whole size — are
// counted and evicted like any other.
func TestThePlaylistCacheCountsItsKeys(t *testing.T) {
	resetPackageCaches()
	t.Cleanup(resetPackageCaches)
	saved := playlistCacheBudget
	playlistCacheBudget = 256 << 10
	t.Cleanup(func() { playlistCacheBudget = saved })
	pl := filepath.Join(t.TempDir(), "playlist.m3u8")
	writeFile(t, pl, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-ENDLIST\n")
	junk := strings.Repeat("j", 4<<10)
	const n = 400
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "/p", nil)
		req.URL.RawQuery = fmt.Sprintf("stream=TOK&x=%d%s", i, junk)
		w := httptest.NewRecorder()
		serveRenditionPlaylist(w, req, pl, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	held, counted := heldBytes(), playlistCacheBytes.Load()
	if counted != held {
		t.Errorf("the count says %d bytes, the entries hold %d, keys and bodies", counted, held)
	}
	if counted > playlistCacheBudget {
		t.Errorf("the cache holds %d bytes, over its budget of %d", counted, playlistCacheBudget)
	}
	if k := cacheEntries(); k >= n {
		t.Errorf("%d long queries left %d entries: nothing evicted", n, k)
	}
}

// A session's parameter is one by its name as the query decodes it, as the
// auth middleware reads it: a stream token under %73tream and a start
// under %74 are in no key and no cached body, the sessions share the entry,
// and each is served the bytes the code before serves it, the name as
// spelt.
func TestAnEncodedNameIsASessionParameter(t *testing.T) {
	s := newSessionsServer(t)
	var toks []string
	for i := 0; i < 5; i++ {
		tok := testStreamToken(t, fmt.Sprintf("user-%d", i), time.Now().Add(time.Hour))
		toks = append(toks, tok)
		se := session{fmt.Sprintf("S%d", i), "%73tream=" + tok + "&caps=avc,hvc,aac,eac3&%74=7"}
		got := s.ask(http.MethodGet, "v0/playlist.m3u8", se.query, nil)
		want := s.oracle(http.MethodGet, "v0/playlist.m3u8", se.query, nil)
		if got.code != http.StatusOK || got.body != want.body || !strings.Contains(got.body, "%73tream="+tok) {
			t.Fatalf("session %d under %%73tream: %d, the code before %d\n%q", i, got.code, want.code, head(got.body))
		}
	}
	shared := s.shared()
	for key, e := range shared {
		for _, tok := range toks {
			if keyHolds(key, tok) || bytes.Contains(e.body, []byte(tok)) {
				t.Errorf("a token under %%73tream is in a key or a cached body: %q", key)
			}
		}
	}
	if entries := len(shared); entries != 1 {
		t.Errorf("five sessions under %%73tream left %d entries, one expected", entries)
	}
}

// A session's value that holds another byte than a token's or a time's —
// a quote, a comma, an apostrophe, an angle bracket, a backslash, a brace,
// a semicolon, a bar — is served as the code before served it, rewritten
// whole with its ETag as before, and kept by no cache.
func TestASessionValueOfOtherBytesIsServedAsBefore(t *testing.T) {
	s := newSessionsServer(t)
	tok := testStreamToken(t, "user-1", time.Now().Add(time.Hour))
	for _, v := range []string{`1"2`, `"`, "a,b", "x'y", "<i>", `a\b`, "{x}", "a;b", "a|b", "a^b", "a`b"} {
		q := "stream=" + tok + "&caps=avc,hvc,aac,eac3&t=" + v
		before := len(s.shared())
		for _, p := range sessionPaths {
			got, want := s.askRaw(s.item, p, q), s.oracleRaw(p, q)
			if got.code != want.code || got.body != want.body || got.head.Get("ETag") != want.head.Get("ETag") {
				t.Errorf("t=%s, %s: %d %d bytes ETag %s, the code before %d %d bytes ETag %s", v, p, got.code, len(got.body),
					got.head.Get("ETag"), want.code, len(want.body), want.head.Get("ETag"))
			}
		}
		if len(s.shared()) != before {
			t.Errorf("t=%s: kept by the cache", v)
		}
	}
	for key := range s.shared() {
		if keyHolds(key, tok) {
			t.Errorf("a key holds the token: %q", key)
		}
	}
}

// shared is what playlistCache holds of the sessions' copy of the package
// (not the oracle's, which keeps every query whole, as the code before).
func (s *sessionsServer) shared() map[playlistKey]*playlistCacheEntry {
	out := map[playlistKey]*playlistCacheEntry{}
	playlistCache.Range(func(k, v any) bool {
		if key := k.(playlistKey); strings.HasPrefix(key.path, s.dir+string(filepath.Separator)) {
			out[key] = v.(*playlistCacheEntry)
		}
		return true
	})
	return out
}

// An extra's master is kept by its record as a title's is: built while the
// record could not be read, or from the record before it changed, it is
// not served after; the next request gets the master a cache of its own
// gives it from the record as it is.
func TestAnExtrasMasterIsKeptByItsRecord(t *testing.T) {
	for _, c := range []string{"the record unreadable at the first fill", "the record changed after the fill"} {
		t.Run(c, func(t *testing.T) {
			root := t.TempDir()
			copyTree(t, filepath.Join("testdata", "packages"), root)
			h := &HLSHandler{Packages: usePackages(t, root), CacheDir: t.TempDir(), FFmpegBin: fakeBin(t, "ffmpeg", "exit 1")}
			captureLog(t)
			rec := filepath.Join(root, "extras", extraTrailer[:2], extraTrailer, "manifest.json")
			good := string(mustRead(t, rec))
			changed := strings.Replace(good, `"encoder": "libx264"`, `"encoder": "copy"`, 1)
			url := extraURL(pkgStereoLadder, extraTrailer, "master.m3u8?stream=TOKA&caps=avc,aac")
			later := time.Now().Add(5 * time.Second)
			if strings.HasPrefix(c, "the record unreadable") {
				// a record that names a rung copied, so the master built
				// without it differs
				writeFile(t, rec, changed)
				writeFile(t, rec, "{ not a record")
				if w := get(h, url); w.Code != http.StatusOK {
					t.Fatalf("the master with its record unreadable: %d", w.Code)
				}
				writeFile(t, rec, changed)
			} else {
				if w := get(h, url); strings.Contains(w.Body.String(), `NAME="Original"`) {
					t.Fatalf("the master before the change names a rung Original")
				}
				writeFile(t, rec, changed)
			}
			_ = os.Chtimes(rec, later, later)
			next := extraURL(pkgStereoLadder, extraTrailer, "master.m3u8?stream=TOKB&caps=avc,aac")
			got := get(h, next)
			resetPackageCaches()
			fresh := get(h, next)
			if got.Code != fresh.Code || got.Body.String() != fresh.Body.String() {
				t.Errorf("the next request: %d\n%s\na cache of its own gives %d\n%s", got.Code, got.Body, fresh.Code, fresh.Body)
			}
			if !strings.Contains(fresh.Body.String(), `NAME="Original"`) {
				t.Errorf("the record as it is names no rung Original: %s", fresh.Body)
			}
		})
	}
}
