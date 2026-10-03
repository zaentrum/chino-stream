package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/play/i1/master.m3u8?stream=dXNlcnwxNzk.c2ln&q=high",
			"/api/play/i1/master.m3u8?stream=REDACTED&q=high"},
		{"/api/play/i1/high/3.m4s?q=high&token=eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln&caps=avc,aac",
			"/api/play/i1/high/3.m4s?q=high&token=REDACTED&caps=avc,aac"},
		{"/x?access_token=a&id_token=b&refresh_token=c&code=d",
			"/x?access_token=REDACTED&id_token=REDACTED&refresh_token=REDACTED&code=REDACTED"},
		{"/x?Token=a&STREAM=b", "/x?Token=REDACTED&STREAM=REDACTED"},
		{"/x?stream=a&stream=b", "/x?stream=REDACTED&stream=REDACTED"},
		{"/x;token=a", "/x;token=REDACTED"},
		{"stream=a&q=1", "stream=REDACTED&q=1"}, // a bare query string
		{"/x?next=%2Fplay%3Fstream%3Dabc%26q%3D1", "/x?next=%2Fplay%3Fstream%3DREDACTED"},
		{`Get "http://chino-stream/api/play/i1/master.m3u8?stream=abc": dial tcp`,
			`Get "http://chino-stream/api/play/i1/master.m3u8?stream=REDACTED": dial tcp`},
		// Not credentials: other names, a name inside another, empty values.
		{"/x?upstream=a&streams=b&stream_id=c&tokens=d", "/x?upstream=a&streams=b&stream_id=c&tokens=d"},
		{"/x?q=token%20talk", "/x?q=token%20talk"},
		{"/x?token=&q=1", "/x?token=&q=1"},
		{"/api/play/i1/master.m3u8", "/api/play/i1/master.m3u8"},
	}
	for _, tc := range cases {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// The request log line carries the URI with the credentials blanked out;
// the handler still gets the real query.
func TestRequestLoggerRedactsCredentials(t *testing.T) {
	var logs bytes.Buffer
	var seen string
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(&logs))
	r.Get("/api/play/{itemId}/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("stream") + "|" + r.URL.Query().Get("token") + "|" + r.RequestURI
		w.WriteHeader(http.StatusTeapot)
	})
	const uri = "/api/play/i1/master.m3u8?stream=dXNlcnwx.c2ln&token=eyJhbGciOiJSUzI1NiJ9.e30.c2ln&q=high"
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, uri, nil))

	line := logs.String()
	if strings.Contains(line, "dXNlcnwx") || strings.Contains(line, "eyJhbGci") {
		t.Errorf("a credential reached the log: %s", line)
	}
	if !strings.Contains(line, `"GET http://example.com/api/play/i1/master.m3u8?stream=REDACTED&token=REDACTED&q=high HTTP/1.1"`) ||
		!strings.Contains(line, " - 418 ") {
		t.Errorf("log line %q", line)
	}
	if seen != "dXNlcnwx.c2ln|eyJhbGciOiJSUzI1NiJ9.e30.c2ln|"+uri {
		t.Errorf("the handler saw %q", seen)
	}
}
