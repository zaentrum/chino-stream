# chino-stream

chino-stream is chino's streaming origin: it serves a title's playback. A
packaged title plays from its package (CMAF segments by byte range, an HLS
master shaped for each client); what is not packaged, or what a client cannot
decode, plays on the fly: the original's tracks copied into CMAF where the
client decodes them, else transcoded by ffmpeg (with NVENC on a GPU node) or,
with `LIVE_WORKER_MODE`, made by live-worker. It asks katalog-api where a
title's package and original are, and reads them from the library's storage.
The apps reach it through chino-api, which proxies its play routes, the
viewer's stream token with them.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
go build ./...
docker build -t chino-stream .
```

It needs Go 1.24 (`go.mod`). The image builds the binary in
`golang:1.24-alpine` and runs it on `nvidia/cuda:12.3.2-runtime-ubuntu22.04`
with `ffmpeg` and `ffprobe` from BtbN's static 7.1.5 build, pinned with its
sha256 (`FFMPEG_BUILD_URL`, `FFMPEG_SHA256`), as user 1001 on port 8080.
`--build-arg BASE=<registry>/library/` pulls the base images from a mirror.

## Test

```sh
go vet ./...
go test ./...
```

The workflow `.github/workflows/build.yml` runs on every pull request and on
main: `go build ./...` and `go vet ./...`, then it installs the ffmpeg the
image ships, by the same URL and sha256, and then `go test ./...`. The tests
that remux, probe and transcode need `ffmpeg` and `ffprobe` on the `PATH` and
skip without them (52 tests in `internal/mkv`, `internal/play` and
`internal/streamcopy`); some also skip when the ffmpeg has no libx264,
libx265, aac, ac3 or eac3. They compare what chino-stream serves with what
ffmpeg makes, so their results depend on the ffmpeg build: the image's is the
static 7.1.5 build the Dockerfile pins. The other tests run on fakes: shell
scripts standing in for ffmpeg and ffprobe, an `httptest` katalog-api and
live worker, and the packages under `internal/play/testdata`, as the packager
writes them.

## Run it

```sh
KATALOG_API_BASE_URL=http://localhost:8081 AUTH_ENABLED=false \
  MEDIA_ROOT=/srv/media HLS_CACHE_DIR=/tmp/chino-hls go run ./cmd/chino-stream
curl -fsS http://localhost:8080/readyz
```

It listens on `LISTEN_ADDR` (`:8080`) and asks katalog-api
(`KATALOG_API_BASE_URL`) where each title is; its default is an in-cluster
address of one namespace, so a run anywhere else sets it, as above. A file
katalog-api names
outside `MEDIA_ROOT`, as mounted on this machine, is refused. `HLS_CACHE_DIR`
keeps the segments made on the fly. `AUTH_ENABLED=false` lets every request
through; with it on, a request carries a bearer of `OIDC_ISSUER` for
`OIDC_AUDIENCE`, or chino-api's stream token, signed with
`STREAM_SIGNING_KEY`, chino-api's key. `FFMPEG_BIN` and `FFPROBE_BIN` name the
binaries, `USE_NVENC=true` encodes on the GPU, and `LIVE_WORKER_URL`,
`LIVE_WORKER_MODE` and `LIVE_WORKER_TOKEN` name the live worker. `/readyz` is
503 while ffmpeg has no AAC encoder, unless the live worker is on
(`LIVE_WORKER_URL` set and `LIVE_WORKER_MODE=on`): then it is 200. The reference demo runs it inside its
cluster with no route out
([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md));
there is no recorded catalog to start it against.

## What the tests leave unchecked

- The GPU: NVENC and the CUDA filters run in no test; of NVENC only the
  encoder arguments are checked.
- Real originals: `TestRealCues`, `TestRealFiles` and `TestStreamCopyOfRealFiles`
  run only with `MKV_SAMPLES` naming a folder of sparse samples
  (`<name>/size`, `<name>/<offset>.part`). None are committed.
- On a runner without ffmpeg, everything that remuxes, probes or transcodes
  is skipped.
- live-worker itself: the tests talk to a fake worker
  (`internal/play/livefake_test.go`).
- `cmd/chino-stream` and `internal/metrics` have no tests.
- The fuzz targets (`FuzzReader`, `FuzzUnlace`, `FuzzMatroskaCopy`) run
  their seed inputs only; fuzzing needs `go test -fuzz`.
