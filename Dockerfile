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
# get the userspace CUDA libs for h264_nvenc + hevc_nvenc + CUDA hwaccel
# decode; libcuda.so.1 is bind-mounted from the host by the NVIDIA device
# plugin at pod start. Pods scheduled on non-GPU nodes still get a working
# ffmpeg (just no `-c:v h264_nvenc`). The base stays at 12.3.2, the one the
# GPU nodes run today: a newer one is not verified on them.
FROM ${BASE}nvidia/cuda:12.3.2-runtime-ubuntu22.04
WORKDIR /app

ENV DEBIAN_FRONTEND=noninteractive \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility

# ffmpeg + ffprobe: BtbN's static GPL build of the ffmpeg 7.1 release
# branch, pinned to a dated archive and checked against its sha256 — the
# same archive the transcoder image pins. Not Ubuntu 22.04's ffmpeg 4.4.2:
# its scale_cuda has no `format` option, so the NVENC path
# (`scale_cuda=…:format=nv12`) cannot run on it, and its ffprobe answers
# `-show_entries frame=pts_time` with empty fields, so the stream-copy
# plan's keyframe fallback (probeKeyframes) finds none. The static build
# links h264_nvenc, hevc_nvenc and the CUDA filters, and ffmpeg's native
# aac encoder (a GPL build has no libfdk_aac; /readyz fails only when
# ffmpeg has no AAC encoder at all). Its nvenc SDK wants an NVIDIA driver
# >= ~550 on the node. Only the two binaries are installed.
#
# BtbN's rolling `latest` release drops a branch once newer ones ship;
# the month-end `autobuild-YYYY-MM-31` releases are kept, and 2026-07-31
# is the last with a 7.1 build (n7.1.5). The checksum is from that
# release's checksums.sha256. Override with --build-arg
# FFMPEG_BUILD_URL=... FFMPEG_SHA256=... to pin a mirror (an empty
# FFMPEG_SHA256 skips the check).
ARG FFMPEG_BUILD_URL=https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-07-31-14-10/ffmpeg-n7.1.5-12-g1fdbca85aa-linux64-gpl-7.1.tar.xz
ARG FFMPEG_SHA256=c1e6caf48923dd8e6bc5e54d51ba70c321175b8162ae9c414c392990e72f0e79
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "${FFMPEG_BUILD_URL}" -o /tmp/ffmpeg.tar.xz \
    && if [ -n "${FFMPEG_SHA256}" ]; then \
         echo "${FFMPEG_SHA256}  /tmp/ffmpeg.tar.xz" | sha256sum -c -; \
       fi \
    && mkdir -p /tmp/ffmpeg \
    && tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ffmpeg --strip-components=1 \
    && install -m 0755 /tmp/ffmpeg/bin/ffmpeg /usr/local/bin/ffmpeg \
    && install -m 0755 /tmp/ffmpeg/bin/ffprobe /usr/local/bin/ffprobe \
    && rm -rf /tmp/ffmpeg /tmp/ffmpeg.tar.xz

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
