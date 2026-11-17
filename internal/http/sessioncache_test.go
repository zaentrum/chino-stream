package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ladderItem is the ladder package of the play tests (internal/play
// testdata): three video rungs with their I-frame playlists, three audio
// renditions, three WebVTT renditions.
const ladderItem = "1adde700-0000-4000-8000-000000000001"

// userToken mints a stream token for user, valid until exp, as chino-api's
// Signer does.
func userToken(t *testing.T, user string, exp time.Time) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + strconv.FormatInt(exp.Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ladderRouter is the router serving a copy of the ladder package, its
// renditions given init segments, from a katalog-api that answers its folder
// as it does for a package from before the library.
func ladderRouter(t *testing.T) http.Handler {
	t.Helper()
	src := filepath.Join("..", "play", "testdata", "packages", "movies", ladderItem[:2], ladderItem)
	dir := filepath.Join(t.TempDir(), "movies", ladderItem[:2], ladderItem)
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"v0", "v1", "v2", "a0", "a1", "a2"} {
		if err := os.WriteFile(filepath.Join(dir, "hls", r, "init.mp4"), []byte("the init of "+r), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h, err := NewRouter(Deps{
		Catalog:    katalogAPI(t, ladderItem, dir),
		OIDCIssuer: oidcIssuer(t), OIDCAudience: "chino-web", AuthEnabled: true,
		StreamSigningKey: signingKey, FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// The stream token is checked on every request before a playlist is looked
// up in the cache: with the entry for the path warm — made by a valid
// session a moment before — a forged token, an expired one, none, and a
// bearer that does not verify are refused as they were before the cache was
// shared (401, no playlist) — also when they send an If-None-Match, the warm
// session's ETag, another user's or any (no ETag, no 304) — and a valid token
// of another user is served
// that user's playlist, its URIs carrying that user's token and none of the
// session's that warmed the entry. HEAD is answered as before: 405 to a
// valid token (no playlist is a HEAD route), 401 to a forged one.
func TestTheStreamTokenIsCheckedBeforeACachedPlaylist(t *testing.T) {
	h := ladderRouter(t)
	exp := time.Now().Add(time.Hour)
	tok1, tok2 := userToken(t, "user-1", exp), userToken(t, "user-2", exp)
	expired := userToken(t, "user-1", time.Now().Add(-time.Minute))
	base := "/api/play/" + ladderItem + "/"
	for _, path := range []string{"master.m3u8", "v0/playlist.m3u8", "a0/playlist.m3u8", "s0/playlist.m3u8", "v0/iframes.m3u8"} {
		rest := "&caps=avc,hvc,aac,eac3&t=12"
		warm := do(h, http.MethodGet, base+path+"?stream="+tok1+rest)
		if warm.Code != http.StatusOK || !strings.Contains(warm.Body.String(), "stream="+tok1) {
			t.Fatalf("%s, the session that warms the entry: %d %q", path, warm.Code, warm.Body)
		}
		etags := []string{warm.Header().Get("ETag"), do(h, http.MethodGet, base+path+"?stream="+tok2+rest).Header().Get("ETag"), "*"}
		for _, c := range []struct {
			name, query string
			want        int
		}{
			{"a forged token", "?stream=" + forged(tok1) + rest, http.StatusUnauthorized},
			{"an expired token", "?stream=" + expired + rest, http.StatusUnauthorized},
			{"no token", "?" + rest[1:], http.StatusUnauthorized},
			{"a bearer that does not verify", "?token=not-a-jwt" + rest, http.StatusUnauthorized},
			{"an empty token", "?stream=" + rest, http.StatusUnauthorized},
		} {
			w := do(h, http.MethodGet, base+path+c.query)
			if w.Code != c.want || strings.Contains(w.Body.String(), "#EXTM3U") || strings.Contains(w.Body.String(), tok1) {
				t.Errorf("%s, %s, the entry warm: %d %q, want %d and no playlist", path, c.name, w.Code, w.Body, c.want)
			}
			// conditional: the warm session's ETag, another user's, any
			for _, inm := range etags {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, base+path+c.query, nil)
				req.Header.Set("If-None-Match", inm)
				h.ServeHTTP(w, req)
				if w.Code != c.want || w.Header().Get("ETag") != "" || strings.Contains(w.Body.String(), "#EXTM3U") {
					t.Errorf("%s, %s, If-None-Match %s: %d ETag %q, want %d, no ETag and no playlist", path, c.name, inm,
						w.Code, w.Header().Get("ETag"), c.want)
				}
			}
		}
		w := do(h, http.MethodGet, base+path+"?stream="+tok2+rest)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "stream="+tok2) || strings.Contains(w.Body.String(), tok1) {
			t.Errorf("%s, another user's valid token, the entry warm: %d %q", path, w.Code, w.Body)
		}
		if w := do(h, http.MethodHead, base+path+"?stream="+tok1+rest); w.Code != http.StatusMethodNotAllowed || w.Body.Len() != 0 {
			t.Errorf("%s, HEAD with a valid token: %d %q", path, w.Code, w.Body)
		}
		if w := do(h, http.MethodHead, base+path+"?stream="+forged(tok1)+rest); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "#EXTM3U") {
			t.Errorf("%s, HEAD with a forged token: %d %q", path, w.Code, w.Body)
		}
	}
}
