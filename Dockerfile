# Base-image registry prefix. Empty default = public Docker Hub; a private
# deploy mirror passes --build-arg BASE=registry.example/library/ .
ARG BASE=
# Stage 1: build the Go binary.
FROM ${BASE}golang:1.24-alpine AS build
WORKDIR /build

# Pull the module graph first so Docker layer caching survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# Strip + trimpath so the image stays small and binaries don't leak paths.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/chino-stream ./cmd/chino-stream

# Stage 2: runtime. Built on nvidia/cuda:12.3.2-runtime-ubuntu22.04 so we
# get h264_nvenc + hevc_nvenc + CUDA hwaccel decode out of the box. The
# userspace CUDA libs ship with the image; libcuda.so.1 is bind-mounted
# from the host by the NVIDIA device plugin at pod start. Pods scheduled
# on non-GPU nodes still get a working ffmpeg (just no `-c:v h264_nvenc`).
FROM ${BASE}nvidia/cuda:12.3.2-runtime-ubuntu22.04
WORKDIR /app

ENV DEBIAN_FRONTEND=noninteractive \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility

# Install ffmpeg + ffprobe. The distro package works everywhere; for the
# NVENC-accelerated transcode path you can swap in a static ffmpeg build
# that links the NVIDIA encoders (set --build-arg to point at your own
# mirror) — the Go service shells out to `ffmpeg`/`ffprobe` on $PATH and
# does not care which build provides them, only that NVENC is present
# when `-c:v h264_nvenc` is requested.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# OpenShift runs containers with an arbitrary UID that belongs to GID 0.
# Create a placeholder user so /tmp etc. work; OpenShift overrides UID anyway.
RUN useradd --system --uid 1001 --gid 0 --home-dir /app --no-create-home --shell /usr/sbin/nologin appuser \
    && mkdir -p /var/lib/katalog/media \
    && chown -R 1001:0 /app

COPY --from=build --chown=1001:0 /out/chino-stream /app/chino-stream

USER 1001
EXPOSE 8080
CMD ["/app/chino-stream"]

LABEL org.opencontainers.image.source="https://github.com/zaentrum/chino-stream"
LABEL org.opencontainers.image.title="chino-stream"
