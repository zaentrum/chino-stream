package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/play"
)

// signingKey is a STREAM_SIGNING_KEY (32 bytes, base64).
const signingKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// streamToken mints a stream token as chino-api's Signer does:
// base64url(user|exp) "." base64url(HMAC-SHA256(key, payload)).
func streamToken(t *testing.T, ttl time.Duration) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte("user-1|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// oidcIssuer answers OIDC discovery, so the router's verifier starts with
// auth on. No bearer is ever verified against it.
func oidcIssuer(t *testing.T) string {
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

// A package's WebVTT renditions sit behind the same auth as its other
// renditions: a stream token in ?stream= (what chino-api's proxy carries
// on every URL) or a bearer; nothing or a forged token is a 401.
func TestSubtitleRenditionsTakeTheStreamToken(t *testing.T) {
	const item = "5ab5ab00-0000-4000-8000-000000000001"
	root := t.TempDir()
	dir := filepath.Join(root, "movies", item[:2], item)
	for name, body := range map[string]string{
		".complete":            "done\n",
		"manifest.json":        `{"version":2,"itemId":"` + item + `","renditions":{"video":[],"audio":[]}}`,
		"hls/s0/playlist.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.000,\nseg-00001.vtt\n#EXT-X-ENDLIST\n",
		"hls/s0/seg-00001.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello.\n",
		"hls/v0/playlist.m3u8": "#EXTM3U\n#EXT-X-ENDLIST\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := play.PackagesRoot
	play.PackagesRoot = root
	t.Cleanup(func() { play.PackagesRoot = old })

	h, err := NewRouter(Deps{
		OIDCIssuer: oidcIssuer(t), OIDCAudience: "chino-web", AuthEnabled: true,
		StreamSigningKey: signingKey, FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := streamToken(t, time.Hour)
	cases := []struct {
		name, query string
		want        int
	}{
		{"stream token", "?stream=" + tok, http.StatusOK},
		{"nothing", "", http.StatusUnauthorized},
		{"a forged token", "?stream=" + tok[:len(tok)-2] + "xx", http.StatusUnauthorized},
		{"an expired token", "?stream=" + streamToken(t, -time.Minute), http.StatusUnauthorized},
	}
	for _, path := range []string{"/s0/playlist.m3u8", "/s0/seg-00001.vtt", "/v0/playlist.m3u8"} {
		for _, tc := range cases {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/play/"+item+path+tc.query, nil))
			if w.Code != tc.want {
				t.Errorf("%s, %s: %d %q, want %d", path, tc.name, w.Code, w.Body, tc.want)
			}
		}
	}
}

// A title's extras sit behind the same auth as its own renditions: a stream
// token in ?stream= (what chino-api's proxy carries on every URL) or a
// bearer; nothing or a forged token is a 401, before the extra is looked up.
func TestExtrasTakeTheStreamToken(t *testing.T) {
	const (
		item  = "5ab5ab00-0000-4000-8000-000000000001"
		extra = "e8e8e800-0000-4000-8000-000000000002"
	)
	root := t.TempDir()
	dir := filepath.Join(root, "extras", extra[:2], extra)
	for name, body := range map[string]string{
		".complete":            "done\n",
		"manifest.json":        `{"version":2,"itemId":"` + extra + `","type":"extra","parentId":"` + item + `","extraKind":"trailer","renditions":{"video":[],"audio":[]}}`,
		"hls/master.m3u8":      "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720\nv0/playlist.m3u8\n",
		"hls/v0/playlist.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.000,\nseg-00001.m4s\n#EXT-X-ENDLIST\n",
		"hls/v0/init.mp4":      "init",
		"hls/v0/seg-00001.m4s": "moof",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := play.PackagesRoot
	play.PackagesRoot = root
	t.Cleanup(func() { play.PackagesRoot = old })

	h, err := NewRouter(Deps{
		OIDCIssuer: oidcIssuer(t), OIDCAudience: "chino-web", AuthEnabled: true,
		StreamSigningKey: signingKey, FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", HLSCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := streamToken(t, time.Hour)
	cases := []struct {
		name, query string
		want        int
	}{
		{"stream token", "?stream=" + tok, http.StatusOK},
		{"nothing", "", http.StatusUnauthorized},
		{"a forged token", "?stream=" + tok[:len(tok)-2] + "xx", http.StatusUnauthorized},
		{"an expired token", "?stream=" + streamToken(t, -time.Minute), http.StatusUnauthorized},
	}
	for _, path := range []string{"/master.m3u8", "/v0/playlist.m3u8", "/v0/init.mp4", "/v0/seg-00001.m4s"} {
		for _, tc := range cases {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/play/"+item+"/extras/"+extra+path+tc.query, nil))
			if w.Code != tc.want {
				t.Errorf("%s, %s: %d %q, want %d", path, tc.name, w.Code, w.Body, tc.want)
			}
		}
	}
	// The master's URIs carry the token on.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/play/"+item+"/extras/"+extra+"/master.m3u8?stream="+tok, nil))
	if !strings.Contains(w.Body.String(), "\nv0/playlist.m3u8?stream="+tok+"\n") {
		t.Errorf("master: %q", w.Body)
	}
}
