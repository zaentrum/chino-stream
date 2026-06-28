package http

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/zaentrum/chino-stream/internal/auth"
	"github.com/zaentrum/chino-stream/internal/catalog"
	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/play"
)

type Deps struct {
	Catalog         *catalog.Client
	MediaRoot       string
	OIDCIssuer      string
	OIDCAudience    string
	AuthEnabled     bool
	FFmpegBin       string
	FFprobeBin      string
	TranscodePreset string
	// StreamSigningKey shared with chino-api's auth.Signer. Empty falls
	// back to an ephemeral random key (tokens from chino-api won't
	// validate; OIDC bearer still works).
	StreamSigningKey string
	// HLSCacheDir is the on-disk staging area for transcoded HLS init
	// + media segments. Cleared periodically by a sweeper; survives pod
	// restarts only as long as the volume is persistent (currently
	// emptyDir, so segments warm again on each pod start).
	HLSCacheDir string
	// NVENC opt-in. Set when the pod has a nvidia.com/gpu allocated
	// and the runtime image bundles a CUDA-aware ffmpeg.
	UseNVENC    bool
	NVENCPreset string
	NVENCCQ     string
}

func NewRouter(d Deps) (http.Handler, error) {
	verifier, err := auth.New(context.Background(), d.OIDCIssuer, d.OIDCAudience, d.AuthEnabled)
	if err != nil {
		return nil, err
	}
	signer, err := auth.NewSigner(d.StreamSigningKey)
	if err != nil {
		return nil, err
	}
	verifier = verifier.WithStreamSigner(signer)

	playH := &play.Handler{
		Catalog:         d.Catalog,
		MediaRoot:       d.MediaRoot,
		FFmpegBin:       d.FFmpegBin,
		FFprobeBin:      d.FFprobeBin,
		TranscodePreset: d.TranscodePreset,
		UseNVENC:        d.UseNVENC,
		// Share the HLS cache root so subtitle .vtt extracts go
		// through the same disk budget + sweeper as HLS segments.
		CacheDir: d.HLSCacheDir,
	}
	hlsH := &play.HLSHandler{
		Catalog:         d.Catalog,
		MediaRoot:       d.MediaRoot,
		FFmpegBin:       d.FFmpegBin,
		FFprobeBin:      d.FFprobeBin,
		TranscodePreset: d.TranscodePreset,
		CacheDir:        d.HLSCacheDir,
		UseNVENC:        d.UseNVENC,
		NVENCPreset:     d.NVENCPreset,
		NVENCCQ:         d.NVENCCQ,
	}
	// Sweep cached segments older than 2 h every 10 min. Tunable later
	// when usage scales past one user.
	hlsH.StartCacheSweeper(context.Background(), 10*time.Minute, 2*time.Hour)
	// Zap warm-pool worker — pre-warms packagedCache with N speculative
	// candidates so the chino-web Zap pager's first-frame is RAM-hot
	// rather than NFS-cold. See zappool.go for the algorithm.
	play.StartZapPoolWorker(context.Background(), hlsH)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	// Prometheus scrape target — un-authed because the chino-stream
	// Service is cluster-internal; only hyperv-prometheus (grafana ns)
	// reaches it.
	r.Method("GET", "/metrics", metrics.Handler())

	r.Group(func(r chi.Router) {
		r.Use(verifier.Middleware)
		// Legacy progressive-MP4 endpoint kept around for fallback /
		// quick smoke tests; chino-web no longer uses it on the HLS
		// cutover.
		// Listing of items that have a finished CMAF package on disk.
		// Used by the Zap pager to filter its candidate pool to
		// instant-start items. Registered BEFORE /api/play/{itemId}
		// so chi's trie picks this static segment over treating
		// "packaged-ids" as an itemId parameter (it normally does
		// static-over-wildcard but explicit ordering removes the
		// surprise).
		r.Get("/api/play/packaged-ids", hlsH.PackagedIDs)
		// Zap warm-pool feed. Returns up to `limit` (default 1, max 8)
		// pre-warmed candidates with per-item seekSec. Registered
		// BEFORE the {itemId} route so chi picks the static segment
		// (chi prefers static-over-wildcard but explicit ordering
		// removes the surprise — same shape as packaged-ids above).
		r.Get("/api/play/zap-feed", hlsH.ZapFeed)
		r.Get("/api/play/{itemId}", playH.Play)
		r.Get("/api/play/{itemId}/info", playH.Info)
		// Embedded-subtitle extractor — runs ffmpeg with -map 0:s:N -c:s
		// webvtt so the browser can mount the result as a <track>. Tokens
		// from <track src> can't carry headers, so the caller appends
		// ?token=… (the auth middleware already accepts that form).
		r.Get("/api/play/{itemId}/subtitles/{streamIndex}.vtt", playH.EmbeddedSubtitle)
		// Sidecar subtitles: pre-packaged files on the packages PVC.
		// Catalog row's `format` column drives MIME (text/vtt for
		// vtt/srt, application/pgs for Bluray PGS, application/x-vobsub
		// for the .idx with sibling .sub, application/dvb-subtitles
		// for DVB). The id comes from the item-detail subtitles[]
		// list; the handler resolves to a path via katalog-api and
		// serves with byte-range, ETag, If-Modified-Since support.
		// The extension on the URL is advisory — clients use it for
		// cache keys + renderer hints; the catalog `format` is the
		// source of truth.
		r.Get("/api/play/subs/{subID}.vtt", playH.SidecarSubtitle)
		r.Get("/api/play/subs/{subID}.sup", playH.SidecarSubtitle)
		r.Get("/api/play/subs/{subID}.idx", playH.SidecarSubtitle)
		r.Get("/api/play/subs/{subID}.sub", playH.SidecarSubtitle)
		r.Get("/api/play/subs/{subID}.dvb", playH.SidecarSubtitle)
		// HLS sub-router: master.m3u8, per-quality playlists, init
		// segment, and on-demand media segments. See HLSHandler.Routes
		// for the URL shape.
		r.Route("/api/play/{itemId}", func(r chi.Router) {
			hlsH.Routes(r)
		})
	})

	return r, nil
}
