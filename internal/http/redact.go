package http

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"

	"github.com/go-chi/chi/v5/middleware"
)

// Credentials ride in request URLs: ?stream=<signed stream token> on every
// play URL chino-api proxies (the token authorises a user's media for
// hours), ?token=<OIDC access token> from clients that cannot set a header.
// chi's request logger wrote them out verbatim, one line per segment.
// requestLogger logs the same line with their values replaced.

// credentialParam matches a credential-carrying query parameter and its
// value, in a URL or in text quoting one: the names the play routes accept
// plus the usual OAuth ones, case-insensitive, also percent-encoded inside
// another parameter (…%3Fstream%3D…).
var credentialParam = regexp.MustCompile(`(?i)((?:^|[?&;]|%3f|%26)` +
	`(?:token|stream|access_token|id_token|refresh_token|code|client_secret|password|api_key|apikey)` +
	`(?:=|%3d))[^&#\s"'\\]+`)

// RedactURL returns s — a URL, a request URI or text quoting one — with the
// value of every credential query parameter replaced by REDACTED.
func RedactURL(s string) string {
	return credentialParam.ReplaceAllString(s, "${1}REDACTED")
}

// redactingLogFormatter is chi's DefaultLogFormatter logging the request
// URI with RedactURL applied.
type redactingLogFormatter struct {
	middleware.DefaultLogFormatter
}

func (f *redactingLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	if red := RedactURL(r.RequestURI); red != r.RequestURI {
		// A copy for the log line only: the handlers keep the real URI.
		r = r.WithContext(r.Context())
		r.RequestURI = red
	}
	return f.DefaultLogFormatter.NewLogEntry(r)
}

// requestLogger is middleware.Logger (same line, same colour rule) writing
// to out, minus the credentials.
func requestLogger(out io.Writer) func(http.Handler) http.Handler {
	return middleware.RequestLogger(&redactingLogFormatter{middleware.DefaultLogFormatter{
		Logger:  log.New(out, "", log.LstdFlags),
		NoColor: runtime.GOOS == "windows",
	}})
}
