// Package auth validates Keycloak-issued JWTs. Mirrors the surface that
// chino-api's auth package exposes — the bearer either arrives in the
// Authorization header (server-to-server) or as a ?token=… query string
// (because <video src> and <img src> can't set headers).
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Verifier struct {
	v        *oidc.IDTokenVerifier
	audience string
	enabled  bool
	signer   *Signer
}

// WithStreamSigner attaches a Signer so Middleware also accepts
// `?stream=<signed-token>` minted by chino-api. Falls back to OIDC
// when ?stream= is absent or invalid.
func (v *Verifier) WithStreamSigner(s *Signer) *Verifier {
	v.signer = s
	return v
}

func New(ctx context.Context, issuer, audience string, enabled bool) (*Verifier, error) {
	if !enabled {
		return &Verifier{enabled: false}, nil
	}
	if issuer == "" {
		return nil, errors.New("oidc issuer is empty")
	}
	// Allow the provider discovery a moderate window so transient IdP
	// blips at startup don't kill the pod.
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(dctx, issuer)
	if err != nil {
		return nil, err
	}
	// SkipClientIDCheck=true because go-oidc's built-in audience check
	// requires a single ClientID, but Keycloak issues multi-aud tokens
	// (aud=["chino-web","katalog","account"]). hasAudience below
	// scans the aud claim ourselves.
	return &Verifier{
		v:        p.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		audience: audience,
		enabled:  true,
	}, nil
}

// Middleware enforces a valid JWT on every request. The bearer can be
// Authorization: Bearer <jwt>, ?token=<jwt>, or — for play routes
// proxied through chino-api — ?stream=<signed-token>. The stream token
// is preferred when present so chino-api can forward URLs unchanged
// across OIDC silent-renew cycles without renegotiating the proxy
// connection (which would kill the in-flight ffmpeg transcode).
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v.enabled {
			next.ServeHTTP(w, r)
			return
		}
		if v.signer != nil {
			if s := r.URL.Query().Get("stream"); s != "" {
				if _, err := v.signer.Verify(s); err == nil {
					next.ServeHTTP(w, r)
					return
				}
				// Invalid / expired — fall through to bearer.
			}
		}
		token := bearerOrQuery(r)
		if token == "" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		idt, err := v.v.Verify(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		if v.audience != "" && !hasAudience(idt, v.audience) {
			http.Error(w, "missing audience "+v.audience, http.StatusForbidden)
			return
		}
		_ = idt
		next.ServeHTTP(w, r)
	})
}

func bearerOrQuery(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func hasAudience(idt *oidc.IDToken, want string) bool {
	var claims struct {
		Aud any `json:"aud"`
	}
	if err := idt.Claims(&claims); err != nil {
		return false
	}
	switch a := claims.Aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
