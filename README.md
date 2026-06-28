# chino-stream

Streaming origin for the **chino** product on the zaentrum platform. Serves
on-demand playback: pre-packaged CMAF byte-range delivery when an asset has
already been packaged, and on-the-fly HLS transcoding (optionally NVENC-
accelerated) for codecs a client can't decode (HEVC / DTS / AC3 / TrueHD).

It reads item-to-file mappings from the catalog API (the read side of the
CQRS split) rather than touching the catalog database directly.

## Layout

```
cmd/chino-stream/main.go        # entrypoint, env config, chi router wiring
internal/http/router.go         # routes: /healthz, /readyz, /metrics, /api/play/*
internal/auth/                  # OIDC bearer verification + per-stream token checks
internal/catalog/client.go      # thin HTTP client to the catalog API
internal/play/                  # playback core
  packaged.go                   #   pre-packaged CMAF byte-range serving
  passthrough.go                #   direct-play passthrough
  hls.go / transcode.go         #   on-demand HLS + ffmpeg/NVENC encoder args
  ffprobe.go                    #   source probing
  zappool.go                    #   zap-feed warm-pool
internal/pkgmanifest/manifest.go # package manifest schema (CMAF, subtitles, trickplay)
internal/metrics/metrics.go     # Prometheus metrics
k8s/                            # Deployment, Service, PVCs, ServiceMonitor, GrafanaDashboard
Dockerfile
```

## Endpoints

| Method | Path                                         | Purpose                             |
|--------|----------------------------------------------|-------------------------------------|
| GET    | `/healthz`, `/readyz`                        | liveness / readiness                |
| GET    | `/metrics`                                   | Prometheus metrics                  |
| GET    | `/api/play/{itemId}`                         | playback (packaged or transcoded)   |
| GET    | `/api/play/{itemId}/info`                    | playback info / capabilities        |
| GET    | `/api/play/packaged-ids`                     | which items are already packaged    |
| GET    | `/api/play/zap-feed`                         | zap discovery feed                  |
| GET    | `/api/play/{itemId}/subtitles/{idx}.vtt`     | embedded subtitle track             |
| GET    | `/api/play/subs/{subID}.vtt` (and `.sup`…)   | sidecar subtitle files              |

## Configuration

All config is environment-driven (defaults in parentheses):

| Variable               | Default                                              |
|------------------------|------------------------------------------------------|
| `LISTEN_ADDR`          | `:8080`                                              |
| `KATALOG_API_BASE_URL` | `http://katalog-api.zaentrum.svc.cluster.local`      |
| `MEDIA_ROOT`           | `/var/lib/katalog/media`                             |
| `HLS_CACHE_DIR`        | `/var/cache/katalog-hls`                             |
| `OIDC_ISSUER`          | `https://sso.example.com/realms/zaentrum`            |
| `OIDC_AUDIENCE`        | `chino-web`                                           |
| `AUTH_ENABLED`         | `true`                                               |
| `FFMPEG_BIN` / `FFPROBE_BIN` | `ffmpeg` / `ffprobe`                           |
| `TRANSCODE_PRESET`     | `veryfast`                                            |
| `USE_NVENC`            | `false`                                              |
| `NVENC_PRESET` / `NVENC_CQ` | `p5` / `23`                                     |

## Local development

```bash
go run ./cmd/chino-stream
curl -fsS http://localhost:8080/healthz
```

`ffmpeg` and `ffprobe` must be on `$PATH` for transcoding. NVENC paths
additionally need an NVIDIA GPU plus an ffmpeg build with the NVIDIA
encoders linked in.

## Build the container

```bash
docker build -t zaentrum/chino-stream .
```

A private deploy mirror can pass `--build-arg BASE=<registry>/library/` to
pull base images from an internal registry. Build and push the image to your
own registry and update the image reference in the `k8s/` manifests for your
environment.

## License

[MPL-2.0](LICENSE).
