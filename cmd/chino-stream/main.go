// chino-stream serves the heavy-lifting playback endpoints — byte-range
// + HLS-on-demand with optional ffmpeg transcoding for codecs the browser
// can't decode (HEVC / DTS / AC3 / TrueHD). It consumes catalog metadata
// via the catalog API (the read side of the CQRS split)
// rather than touching the Postgres catalog directly.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
	streamhttp "github.com/zaentrum/chino-stream/internal/http"
)

func main() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cat := catalog.New(cfg.KatalogAPIBaseURL)

	handler, err := streamhttp.NewRouter(streamhttp.Deps{
		Catalog:          cat,
		MediaRoot:        cfg.MediaRoot,
		OIDCIssuer:       cfg.OIDCIssuer,
		OIDCAudience:     cfg.OIDCAudience,
		AuthEnabled:      cfg.AuthEnabled,
		FFmpegBin:        cfg.FFmpegBin,
		FFprobeBin:       cfg.FFprobeBin,
		TranscodePreset:  cfg.TranscodePreset,
		StreamSigningKey: cfg.StreamSigningKey,
		HLSCacheDir:      cfg.HLSCacheDir,
		UseNVENC:         cfg.UseNVENC,
		NVENCPreset:      cfg.NVENCPreset,
		NVENCCQ:          cfg.NVENCCQ,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("chino-stream listening on %s (media=%s, katalog=%s, auth=%v)", cfg.ListenAddr, cfg.MediaRoot, cfg.KatalogAPIBaseURL, cfg.AuthEnabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown initiated")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

type Config struct {
	ListenAddr        string
	KatalogAPIBaseURL string
	MediaRoot         string
	OIDCIssuer        string
	OIDCAudience      string
	AuthEnabled       bool
	FFmpegBin         string
	FFprobeBin        string
	TranscodePreset   string
	// StreamSigningKey must match chino-api's value so `?stream=<token>`
	// minted there validates here. Base64. Empty → ephemeral random key
	// (works only single-pod).
	StreamSigningKey string
	// HLSCacheDir is where on-demand HLS segments + init files are
	// cached. Default /var/cache/katalog-hls — overridable for local
	// dev. Files are tagged by item+quality+segment-index so cache
	// hits skip the ffmpeg run entirely.
	HLSCacheDir string
	// UseNVENC enables CUDA hwaccel decode + h264_nvenc encode on the
	// /high/ ladder. Off by default — non-GPU pods fall back to
	// libx264. NVENCPreset / NVENCCQ tune the encoder (p1=fastest,
	// p7=slowest/best; CQ 18-28 typical range).
	UseNVENC    bool
	NVENCPreset string
	NVENCCQ     string
}

func loadConfig() Config {
	c := Config{
		ListenAddr:        envOr("LISTEN_ADDR", ":8080"),
		KatalogAPIBaseURL: envOr("KATALOG_API_BASE_URL", "http://katalog-api.zaentrum.svc.cluster.local"),
		MediaRoot:         envOr("MEDIA_ROOT", "/var/lib/katalog/media"),
		OIDCIssuer:        envOr("OIDC_ISSUER", "https://sso.example.com/realms/zaentrum"),
		OIDCAudience:      envOr("OIDC_AUDIENCE", "chino-web"),
		AuthEnabled:       envOr("AUTH_ENABLED", "true") == "true",
		FFmpegBin:         envOr("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:        envOr("FFPROBE_BIN", "ffprobe"),
		TranscodePreset:   envOr("TRANSCODE_PRESET", "veryfast"),
		StreamSigningKey:  os.Getenv("STREAM_SIGNING_KEY"),
		HLSCacheDir:       envOr("HLS_CACHE_DIR", "/var/cache/katalog-hls"),
		UseNVENC:          envOr("USE_NVENC", "false") == "true",
		NVENCPreset:       envOr("NVENC_PRESET", "p5"),
		NVENCCQ:           envOr("NVENC_CQ", "23"),
	}
	if c.KatalogAPIBaseURL == "" {
		log.Fatal("KATALOG_API_BASE_URL is required")
	}
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
