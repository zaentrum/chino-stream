# chino-stream

Streaming origin for the **chino** product on the zaentrum platform. Serves
on-demand playback: pre-packaged CMAF byte-range delivery when an asset has
already been packaged, and on the fly what is not: an original's video and
audio copied into CMAF as they are wherever the client decodes them
([below](#what-an-original-plays-as)), and HLS transcoding (optionally NVENC-
accelerated) for what it cannot — from the original, or once that is retired
from the package itself.

It asks the catalog API (the read side of the CQRS split) where an item's
original and package are rather than touching the catalog database, and
never works a path out itself ([below](#where-a-title-plays-from)).

## Layout

```
cmd/chino-stream/main.go        # entrypoint, env config, chi router wiring
internal/http/router.go         # routes: /healthz, /readyz, /metrics, /api/play/*
internal/auth/                  # OIDC bearer verification + per-stream token checks
internal/catalog/client.go      # thin HTTP client to the catalog API
internal/mkv/                   # Matroska: header, tracks, Cues and blocks by byte range
internal/streamcopy/            # an original's tracks copied into CMAF: its index, its segments
internal/play/                  # playback core
  resolve.go                    #   where an item's or an extra's package is (katalog-api, cached)
  packaged.go                   #   pre-packaged CMAF byte-range serving
  ladder.go                     #   a packaged master as each client is served it
  extras.go                     #   a title's extras (trailers, …), packaged apart from it
  passthrough.go                #   direct-play passthrough
  hls.go / transcode.go         #   on-demand HLS + ffmpeg/NVENC encoder args
  source.go                     #   what on-the-fly reads: the original, else the package
  window.go                     #   a window's segments served as ffmpeg finishes them
  liveworker.go / live.go       #   the live worker: its calls, and the live pipeline of the on-the-fly routes
  liveshadow.go                 #   LIVE_WORKER_MODE shadow's checks
  copyplan.go                   #   what of an original plays as it is, for a client
  copyindex.go / copyserve.go   #   the stream copy's indexes; its master, playlists and segments
  remuxcopy.go / remuxaudio.go  #   the muxed stream copy and a package's audio, remuxed in Go
  webvtt.go                     #   a package's subtitle, shifted in Go
  ffprobe.go                    #   source probing
  zappool.go                    #   zap-feed warm-pool
internal/pkgmanifest/           # a package's manifest.json, and a library package.json read as one
internal/metrics/metrics.go     # Prometheus metrics
k8s/                            # Deployment, Service, PVCs, ServiceMonitor, GrafanaDashboard
Dockerfile
```

## Endpoints

| Method | Path                                         | Purpose                             |
|--------|----------------------------------------------|-------------------------------------|
| GET    | `/healthz`                                   | liveness                            |
| GET    | `/readyz`                                    | readiness: 503 while ffmpeg has no AAC encoder (not with `LIVE_WORKER_MODE` on) |
| GET    | `/metrics`                                   | Prometheus metrics                  |
| GET    | `/api/play/{itemId}`                         | playback (packaged or transcoded)   |
| GET    | `/api/play/{itemId}/info`                    | playback info / capabilities        |
| GET    | `/api/play/packaged-ids`                     | which items are already packaged    |
| GET    | `/api/play/zap-feed`                         | zap discovery feed                  |
| GET    | `/api/play/{itemId}/subtitles/{idx}.vtt`     | embedded subtitle track             |
| GET    | `/api/play/subs/{subID}.vtt` (and `.sup`…)   | sidecar subtitle files (a SubRip one as WebVTT) |
| GET    | `/api/play/{itemId}/master.m3u8`             | HLS master (packaged or on the fly) |
| POST   | `/api/play/{itemId}/prewarm`                 | warm the start of a title (202)     |
| GET    | `/api/play/{itemId}/{vN\|aN}/playlist.m3u8`   | packaged video / audio rendition    |
| GET    | `/api/play/{itemId}/{vN\|aN}/init.mp4`, `seg-{n}.m4s` | its init and media segments |
| GET    | `/api/play/{itemId}/{vN}/iframes.m3u8`       | I-frame playlist of a video rung    |
| GET    | `/api/play/{itemId}/{sN}/playlist.m3u8`, `seg-{n}.vtt` | packaged WebVTT rendition |
| GET    | `/api/play/{itemId}/{high\|medium\|low}/…`, `copy/…`, `audio/{n}/…` | on-the-fly HLS |
| GET    | `/api/play/{itemId}/video/…`, `audiocopy{n}/…`, `surround{n}/…` | an original's stream copy ([below](#what-an-original-plays-as)) |
| GET    | `/api/play/{itemId}/extras/{extraId}/master.m3u8` | an extra's HLS master ([below](#extras)) |
| GET    | `/api/play/{itemId}/extras/{extraId}/{vN\|aN\|sN}/…` | its renditions, as a packaged title's |
| GET    | `/api/play/{itemId}/extras/{extraId}/{high\|medium\|low}/…`, `audio/{n}/…` | its on-the-fly HLS |

Every `/api/play` route takes a bearer or `?stream=<token>` (chino-api's
stream token). The URIs inside a playlist carry the request's query, so the
token, `caps` and `q` ride along on every fetch — and `v`, the version a
session is pinned to, and `lw=1` on a session of the live pipeline
([below](#the-live-worker)).

## Where a title plays from

katalog-manager owns every path of the library and records each package as
it completes, so chino-stream asks katalog-api where a title plays from:
`/api/v1/items/{id}/playback` (the current package, the superseded versions
not removed yet, the original), `/api/v1/extras/{id}/playback` and
`/api/v1/packaged-ids` (the packaged movies and episodes, for the Zap
pager). In the library a package is a version folder,
`<item>/versions/<versionId>/`, read by its `package.json`; before it the
package store's folder, read by its `manifest.json`.

- Answers are cached per item: 15 s fresh, then served stale while one
  lookup revalidates them, and while katalog-api fails served for up to
  10 min; an unknown item is remembered 5 s. The packaged ids the same for
  60 s.
- A version plays once its `.complete` is visible; right after the
  packager's rename, while an NFS client may not see it yet, the version
  it superseded plays. A file missing from the folder answered (a version
  removed after its grace) resolves the title once more.
- A session is pinned to its version: a library version's master writes
  `v=<versionId>` onto its URIs, and while katalog-api lists that version
  (current or previous) the session is served from it, so one started on
  a version superseded meanwhile ends on it. A package from before the
  library has no version and no `v`.

Packages are HEVC only, and in the library an original is deleted once its
package is recorded. A client that decodes none of a package's rungs (no
HEVC, or HEVC under the package's height) gets the on-the-fly transcode:
from the original while there is one, as always, else from the package —
its top video rendition and stereo AAC renditions, each window's segments
read as one fragmented MP4 (`concat:init.mp4|seg-N.m4s|…` with
`-seek_timestamp 1`; ffmpeg's HLS demuxer cannot seek in them). A client
that decodes the package's 5.1 companions (`eac3`) gets them as well, in
the master's one audio group, each just before the stereo transcode of its
source track as rule 4 orders them. A companion is served as packaged
(`aN/playlist.m3u8`, on the timeline the transcoded video keeps) and
warmed so when the client starts on it; `/info` lists the group in that
order (`group` `aud`, `rendition` `a2` or `audio/1`). A `native` client
gets the transcode rung twice instead, with the stereo transcodes (`aud`)
and with a 5.1 group (`aud-surround`) of the same members, each played by
its companion where it has one, as rule 4 has it; `/info` lists the stereo
tracks, each with `surround` where a companion plays it. Nothing of a package
goes through ffmpeg's stream copy (`/copy/` is 404); `/info` describes the
transcode; the progressive `/api/play/{id}` answers 404 "progressive
playback needs the original"; an embedded subtitle is the package's
WebVTT of that stream. A title whose package katalog-api names but the
storage does not have complete, and without an original, answers 404 "not
playable: the original was retired and its package is missing".

A window of the on-the-fly pipeline is ten 6 s segments encoded by one
ffmpeg run; each segment is served as soon as it is finished, so the first
segment of a window comes within about a second — not when the window is
done. A window nobody asks for any more for 20 s is given up.

## What an original plays as

On the fly, what of an original is encoded is decided per client, from its
probe and `caps` (`internal/play/copyplan.go`). The container never forces
an encode, and the video is never encoded for the audio's sake:

- **The video is copied** when the client decodes it as it is: H.264
  Baseline, Main or High at 8 bits, HEVC Main or Main 10, AV1 Main (VP9 from
  an MP4 only), 4:2:0, no taller than the client's cap for its codec
  (`hvc:1080`). HDR10 and HLG are copied where the client takes the codec at
  that height, as a package's HEVC is served. Dolby Vision whose base layer
  other decoders play (8.1, 8.4) is copied as that base layer; profile 5 only
  to a client that declares Dolby Vision (`dvh`); profile 7 never — the
  file's own record decides, whatever the probe says. An MP4 of profile 5 or
  7 plays as it always did: the muxed copy where the client takes it whole,
  else the ladder (until clients declare `dvh` from their decoders and the
  ladder reshapes profile 5). A client that sends no `caps` is copied video
  up to 1080p. Any other video, and a `q` below high, takes the transcode
  ladder.
- **Each audio track is copied** when the client decodes it: AAC (beyond
  two channels with `aacmc`), AC-3, E-AC-3, multichannel included. Any other
  track is **converted** on a rendition of its own: stereo AAC
  (`audio/{n}/`, on the 6 s grid, as the ladder's audio always was), and 5.1
  AAC as well for a client with `aacmc` (`surround{n}/`, just before its
  stereo one). An original with no track converted encodes nothing.
- An MP4 original the client takes whole (its video and first audio track)
  stays the muxed stream copy (`copy/`) it always was; so does an MPEG-TS
  one, its keyframes probed in the background once (read at 32 MiB/s,
  ffprobe at the lowest CPU priority; its first session takes the ladder
  meanwhile) and kept in `copy-index/`.

The stream copy (`internal/streamcopy`, `internal/mkv`):

- **The index.** An original is indexed once: its keyframes from its
  container's own index — a Matroska file's Cues, found through its
  SeekHead (kilobytes at the end of the file, read in milliseconds), an MP4's
  `stss` and `stts` — and each track's CMAF header: its codec configuration
  as the original has it, an edit list taking the reordering delay off.
  Nothing reads a whole file on the request path: of a Matroska file its
  head (to its first keyframe and each copied audio track's first frame,
  further only for a video whose parameter sets do not say how far its
  frames are reordered; at most 16 MiB) and its Cues. Indexes are kept by path, size and mtime: in
  memory, and a Matroska one in the cache folder `copy-index/`, which the
  2 h sweep leaves alone (one unused for 30 days goes) — across restarts
  for as long as the cache volume lasts. `/info`, the master and `/prewarm`
  make it — one build per file, which the others wait for — so it is there
  before the first segment.
- **No usable index.** A Matroska file without Cues for its video, with
  keyframes more than 20 s apart in them, or with Cues its first Clusters
  show wrong (a cue on a frame that is no keyframe), is read through in the
  background, one file at a time at 32 MiB/s, for its keyframes; its
  sessions take the ladder until then.
- **Segments.** The keyframes grouped into segments of at least 6 s.
  Segment n is one read of the byte range from its keyframe's block to the
  next segment's: its video and every copied audio track, each a fragment
  of its own on the track's own timeline — decode times the presentation
  times sorted, so no gap or overlap at a segment's edge — presentation zero
  the container's start time, as every on-the-fly rendition's, so a copied
  video plays in step with converted audio; an audio frame missing in the
  file stays a gap, and a track with no frame in a segment has a fragment
  of none there. Segments are kept in memory (384 MiB, the least recently
  used dropped: they are cheap to make again), the next two made ahead of
  the play head.
- **The master.** One variant, `video/index.m3u8`: `CODECS` from the codec
  configuration (`hvc1.2.4.L150.90,ec-3`), `VIDEO-RANGE` the source's,
  `BANDWIDTH` the copy's peak; its audio group each track at
  `audiocopy{n}/` or converted. A `native` player gets Apple's shape: the
  stereo group (each track as AAC) and, when a track plays in surround, a
  5.1 group of the same members, the variant once per group. The URIs carry
  `c=`, the fingerprint of the file the session began on: another file at
  the title's path answers 404.
- **Fail safe.** A file the copy does not take (a stream feature it does not
  carry, a damaged index, a track list its probe does not match) plays the
  ladder. A segment it cannot make answers 404 — players step down onto the
  ladder on it — and the title's new sessions take the ladder until the
  file changes (a session under way goes on with the segments that can be
  made): one the file is the cause of (its keyframe not where the index
  says, found out within a keyframe's distance; frames it cannot lay out),
  at once; one the storage failed to read, 503 with `Retry-After` twice,
  then so for a minute. A damaged or cut-short file is an error, never a
  crash, and so are frames made to inflate past 64 MiB a block (256 MiB a
  segment, 16 MiB the head); a panic of the reader fails its file. Each is
  logged and counted (`katalog_stream_copy_fallbacks_total`).
- **Index version.** `IndexVersion` (`internal/streamcopy`) is part of a
  session's `c=`: raising it makes every index and plan be made anew, which
  also clears the files kept as failed — and answers 404 to the copy
  sessions under way, whose players step down onto the ladder.

`/play/info`'s `mode` is `passthrough` for the muxed copy, `remux` for the
stream copy (no quality menu, as before), `transcode` for the ladder (high,
medium, low, each by its [name](#quality-names)); its `reason` says what
happens and why, as the player shows it: "The video plays as it is in the
file. The audio is converted to stereo AAC, because this device does not
play E-AC-3."

## Quality names

chino-stream names every quality step by one fixed table of heights, so a
name means the same height on every title and every installation
(`internal/play/qualityname.go`, `qualityName`, the only source of them;
the product spec is `docs/chino/playback.md` in zaentrum/zaentrum):

| Height | Name |
|---|---|
| 2160p and up | `Ultra (2160p)` |
| 1080p | `High (1080p)` |
| 720p | `Medium (720p)` |
| 480p | `Low (480p)` |
| below 480p | `Lowest (360p)` |
| the original, played as it is | `Original` |

A frame's height for naming is its step: its own height, or the height its
width stands for at 16:9, whichever is larger — the 16:9 frame it fills, as
the transcoder's `LADDER` names its rungs. A scope film's 1920x818 is
`High (1080p)`, like a 16:9 film's 1920x1080, its 3840x1606 `Ultra
(2160p)`, a 1.85:1 film's 1920x1038 `High (1080p)`. A frame that is a
little wider or a little narrower than a standard 16:9 frame is that frame:
up to 10% wider (DCI 4096x2160 is `Ultra (2160p)`, 2048x858 `High
(1080p)`), or a step up to 3% below a standard height, whatever the frame's
shape - a standard frame cropped at the sides or a few lines short (1916x800
and 1904x1072 are `High (1080p)`, 3836x1600 `Ultra (2160p)`, 1272x716
`Medium (720p)`). A step
between two heights of the table takes the lower one's word with its own
number: 1600x900 is `Medium (900p)`, and 1666x900, wider than 16:9 and no
standard frame, `Medium (937p)`, by the height its width stands for, not by
its own. So do the steps past either end (`Ultra (4320p)`, `Lowest
(240p)`).

- **The stream copy** (`copy/`, and the `remux` copy's `video/`): `Original`.
- **The on-the-fly ladder**, on ffmpeg and on the live worker alike: each
  rung by the frame it encodes for that title and client (`caps`' height
  cap included), by its step like any frame, as a package's rung of that
  frame is named. A re-encode at the original's size is named by its step
  like any other: a 720p original's `high` is `Medium (720p)`, a 1728x720
  one's `Medium (972p)`, a 4K one's (fitted into 1080p) `High (1080p)`.
  `medium` and `low` scale to 720 and 480 lines, keeping the original's
  shape, and are named by their steps too: a 16:9 film's `medium` (1280x720)
  is `Medium (720p)`, a 1920x800 (2.39:1) film's `medium` (1728x720) `Medium
  (972p)` and its `low` (1152x480) `Low (648p)`; where the original is
  shorter they encode its own frame and are named as `high` is. Under a
  `caps` height cap below 1080 a rung wider than the 16:9 box of its height
  is fitted into it, as `high` is into the cap's: a 1920x800 film on a 720
  cap is 1280x534 on `high` and `medium`, `Medium (720p)`, and 854x356 on
  `low`, `Low (480p)`; an original no wider than 16:9 scales to the height
  as without a cap. Without such a cap no rung is wider than `high`'s 1920
  box: the `medium` of an original wider than 8:3 (and `low` of one wider
  than 4:1) is fitted to it by width as `high` is, so a 3840x1390 film's
  `medium` is `high`'s 1920x696, `High (1080p)`, not 1990x720; its files
  live under `{rung}-fit` (`medium-h1080-fit` on a 1080 cap), none of those
  made at the wider frame before. No rung is advertised (`BANDWIDTH`) or
  encoded on the live worker above a better rung's rate: one that encodes
  a better rung's frame, or one just smaller, gets that rung's rate where
  its own rate per pixel gives more — that film's `medium` at `high`'s
  4,188,888 bit/s, not 4,350,000, a 720p original's at 2,888,888, not
  3,000,000, a 720x486 original's `low` (712x480) at `high`'s 1,096,875,
  not 1,167,213.
  `/play/info` lists every `q`, as before, each with its name: two rungs
  that encode one frame carry one name (a 720p original's `high` and
  `medium` are both `Medium (720p)`, a 1728x720 one's `Medium (972p)`), and
  both are listed, as the apps step down by `q` and look a `q`'s name up in
  the list. A client is to merge such entries: one row per name, the first
  entry's (the better encode), and a step-down to the next entry with
  another name (`high` to `medium` re-encodes that original's frame once
  more), never to a `q` the list does not have, so the notice can say the
  step's name. The released phone, TV and Tizen apps do not merge yet: they
  show one row per entry, so such a name shows twice (three times for a
  360p original) until they do; deploy this naming after apps that merge.
  An original that plays as it is offers no choice at `high` (`mode`
  `passthrough` or `remux`); a client that steps down from it asks
  `/play/info` with the lower `q`, and there `high` is `Original`, the
  stream copy `q=high` serves, not the rung's re-encode (`High (1080p)`).
  Every `q` plays its rung, at the frame given above. Of an original whose
  size ffprobe does not give, each rung is named by its height when
  `LIVE_WORKER_MODE` is `on` (`high` `High (1080p)` or the cap's, `medium`
  `Medium (720p)`, `low` `Low (480p)`): the live worker, asked for a height
  alone, fits the original into the 16:9 box of it. On ffmpeg no rung has a
  name: it encodes `high` at the original's unknown size and `medium` and
  `low` in its unknown shape (a scope film's `medium` would be 1728x720,
  `Medium (972p)`, not `Medium (720p)`): an empty `label`, no `NAME`. A
  client names an empty label by itself.
- **A package's rungs**: `Original` for a rung that is the original's own
  video stream copied into the package without a re-encode, as the
  package's record says (its video rendition's `encoder` is `copy`, in
  `package.json` and `manifest.json` alike; the packager writes it since
  renditions.json: `copy` for the transcoder's copy rung or an original
  packaged with no handoff, the encoder's name for an encode). Every other
  rung by its frame (the variant's `RESOLUTION`), a re-encode at the
  original's size too (`encoder` `hevc_nvenc` at 1920x818 is `High
  (1080p)`). A package from before renditions.json records neither, and
  every rung of it is named by its frame, as before: nothing is guessed
  from a rung's codec, size or bit rate.

The name is `label` in `/play/info`'s `qualities` and `NAME` on every
`EXT-X-STREAM-INF` of a master (hls.js reads it as a level's `name`); the
I-frame playlists are not named. Clients show it as given; the step-down
notices say it. The `q` values and every URL are as they were.

## The live worker

ADR-0013 (live rungs beside the package, as a supervised safety net), phase
1: chino-stream starts no encoder; every live encode goes to the live-worker
service, a supervised pool of warm worker processes, through its own `api`
and `client` packages. `LIVE_WORKER_URL` names it and `LIVE_WORKER_MODE` turns
it on, one instance at a time:

- `off` (the default) — the ffmpeg pipelines above, byte for byte; the
  worker is never asked for anything.
- `shadow` — ffmpeg serves as with `off`; once it has served a segment, the
  worker is asked for the same one at prefetch priority (only on a slot it
  has to spare), and what comes back is checked and dropped: the grid's
  boundary, a rung's codec configuration the same in every segment, ffmpeg's
  frames. The Go remuxes below are checked against ffmpeg's segments alike.
- `on` — new on-the-fly sessions go to the live pipeline.

The live pipeline of a session:

- **Video, and an original's audio**: one worker job per segment, on the grid
  the playlists always had (`floor(d/6)` segments of 6 s and a tail over
  0.5 s), with the frame and bitrate the master advertises; `init.mp4` is
  `POST /v1/init`. Serving segment *k*, the next three are asked for at
  prefetch priority; a master's or `/prewarm`'s warm asks for the init and
  three segments from `t`. Deadlines: 12 s a playhead segment, 10 s an init,
  30 s a prefetch, 120 s a subtitle; a playhead call the worker has no slot
  for is made once more after `Retry-After`, a refused or reset connection
  once more after 200 ms. The player gets 404 for a segment the source cannot
  make (it falls back), 503 with `Retry-After`, 504, or 502. A fragment whose
  codec hash is not the rung's cached init's is asked for once more, else 502.
- **A package's audio**: the stereo rendition's own AAC frames presented in
  each segment, remuxed in Go (mp4ff) with their own timestamps; the init is
  the rendition's.
- **The muxed stream copy** of an MP4: remuxed in Go — the init from the
  original's `moov`, each segment its samples read by byte range — on the
  plan the ffmpeg path makes; a fragmented original's keyframes come from
  its `trun` flags. An original mp4ff cannot read (MPEG-TS, FLV) takes the
  transcode ladder.
- **The stream copy** ([above](#what-an-original-plays-as)) is chino-stream's
  own in Go in every mode; with `on` an audio track it converts is the
  worker's, a job per segment of the grid in its `cpu` lane, the session
  pinned with `lw=1` (the worker converts to stereo: no `surround{n}/`).
- **Embedded subtitles**: an original's are the worker's (`POST
  /v1/subtitle`; a bitmap stream is 502, as before); a package's WebVTT is
  shifted in Go.
- The progressive `/api/play/{id}` transcode answers 404 "progressive
  transcoding moved to HLS"; its byte ranges of the original stay. ffprobe's
  header probe stays.

A session keeps its pipeline: the on-the-fly master of a session sent to the
live pipeline writes `lw=1` into its URIs, as it writes `v=`, so a flip of
the flag changes new sessions only and a session in flight ends on the
pipeline it began on. The live pipeline's files are cached in folders of
their own (`high-lw1`, `audio-0-lw1`, `copy-lw1`, `sub-lw1`). With the worker
not ready (`/readyz`), a new session goes to ffmpeg. Without
`LIVE_WORKER_URL` there is no live pipeline, and `lw` means nothing.

Metrics of every pipeline: `katalog_stream_segment_production_seconds` and
`katalog_stream_segment_realtime_ratio` (the seconds a segment not in a cache
took to make, and those per second of media in it; `path` package, copy,
audio or encode), `katalog_stream_copy_fallbacks_total` (`reason` index,
plan, segment, panic), `katalog_stream_copy_indexes_total` (`result`),
`katalog_stream_copy_index_duration_seconds` (`how`: cues, moov, scan) and
`katalog_stream_copy_scan_queue`. Of the live worker:
`katalog_stream_live_worker_requests_total` and
`katalog_stream_live_worker_request_duration_seconds` (`kind`, `track`,
`priority`, `result`), `katalog_stream_live_worker_retries_total` (`kind`,
`reason`), `katalog_stream_live_worker_fallbacks_total` (`reason`: `unready`,
`refused`), `katalog_stream_live_worker_inflight`,
`katalog_stream_live_worker_shadow_checks_total` (`track`, `result`) and
`katalog_stream_remux_total` (`kind`, `result`).

## Packaged playback: what a client is served

A packaged title's master, `/info` and `/prewarm` read the same query:

- `caps` — what the client decodes, comma-separated: video `avc`, `hvc`,
  `av1`, `vp9`, each with an optional `:<height>` (the hardware decoder's
  tallest frame, e.g. `hvc:1080`); audio `aac`, `mp3`, `opus`, `vorbis`,
  `ac3`, `eac3`; `aacmc` for AAC beyond two channels; `dvh` for Dolby Vision
  (HEVC based). Without `caps` the default set applies: H.264, VP9, AV1,
  AAC, MP3, Opus, Vorbis — no HEVC.
  `native` says the client plays the master with the platform's own HLS
  player — AVPlayer (the iOS and tvOS app), Safari's native HLS (chino-web
  without MSE) — which picks between audio groups itself; it changes only
  how audio groups are served (rule 4), never what the client decodes.
- `q` — `auto` (the default) or a rung id from `/info`'s `qualities`. The
  on-the-fly pipeline's `high`, `medium` and `low` keep their meaning
  there; on a packaged title they mean `auto`.
- `t` — seconds; where a warm reads segments from.

A package from before renditions.json has one video rendition and one
audio group; its master is served as packaged. A ladder (the transcoder's
`LADDER`, the packager's `SURROUND_AUDIO` / `HLS_SUBTITLES`) lists every
rung once per audio group, and each client gets its share
(`internal/play/ladder.go`):

1. **One codec family.** The HEVC rungs when the client decodes HEVC and
   one of them fits its HEVC height cap; else the H.264 rungs that fit its
   H.264 cap; else the first other family it decodes. Players don't switch
   between HEVC and H.264 in a stream, and hls.js would start an HEVC
   browser on an H.264 rung of a mixed master and keep it there.
2. **Never above the cap.** No rung taller than the client's
   `:<height>` for its codec.
3. **`q=<rung id>`** serves that rung alone when the client decodes it at
   its height, whichever family; any other `q` serves the ladder.
4. **Audio groups it decodes, one variant per rung.** Stereo AAC always;
   `ec-3` only with `eac3`, `ac-3` only with `ac3`, AAC beyond two channels
   only with `aacmc`. A dropped group's renditions and variants go with it.
   A client that decodes a further group besides the stereo one (the 5.1
   companions, `audio-surround`, with `eac3`) gets one variant per rung, not
   two: the further group's, its `CODECS` with the stereo codecs added and
   its `BANDWIDTH` the larger of the two. That group holds every stereo
   rendition, each companion just before the stereo rendition of its source
   track (`idx` in a manifest, `sourceStreamIndex` in a record; else the
   first in its language): "English 5.1", "English", "German". A track
   without a companion (a stereo source, a commentary) stays; the default is
   the companion of the stereo default, else that default; names are unique
   in the group. Two variants of a rung differ in `CODECS`, and hls.js keeps
   to the codecs it started on (the stereo ones), so a 5.1 pick had no level
   to go to.

   A `native` client gets Apple's shape instead: each group it decodes, each
   rung once per group, and every group with the same members, alike but for
   `URI` and `CHANNELS` (RFC 8216, 4.3.4.1.1), so the player matches a track
   across groups and picks the group it decodes. The 5.1 group holds the
   stereo group's members, in their order, named and defaulted as there,
   each played by its companion where it has one and by its own stereo
   rendition where not (a stereo source, a commentary: RFC 8216 lets a
   group's members differ in sample format). Its variants then list AAC in
   `CODECS` too. A companion that is nobody's twin stays after the members,
   named apart.
5. **Subtitles as packaged.** The `SUBTITLES` group is in the master when
   the packager wrote it; I-frame playlists only for the rungs served.
6. **One default per audio group.** Each audio group served has exactly one
   `DEFAULT=YES`: the packager's, else the rendition in the language of the
   first group's default, else the first `AUTOSELECT` one. A `SUBTITLES`
   group keeps at most one.

The filter only drops lines (and sets `DEFAULT`); what is left is the
packaged master's lines in their order — but for the audio of rule 4: the
one group, its renditions in the order given there, or for a `native`
client the further groups' members as given there; and their variants'
`CODECS` and `BANDWIDTH` set so. A client that decodes none of a package's
rungs falls through to the on-the-fly pipeline, as before.

`GET /api/play/{itemId}/info` for a packaged title says what the client
starts on and what it may pick:

```json
{
  "mode": "packaged",
  "video_codec": "avc1.64001f", "width": 1280, "height": 720,
  "default_quality": "auto",
  "qualities": [
    {"name": "auto", "label": "Auto"},
    {"name": "v1", "id": "v1", "label": "Medium (720p)", "width": 1280, "height": 720,
     "codec": "avc1.64001f", "bitrate": 1505267, "video_range": "SDR"},
    {"name": "v2", "id": "v2", "label": "Low (480p)", "width": 854, "height": 480,
     "codec": "avc1.64001e", "bitrate": 706649, "video_range": "SDR"}
  ],
  "audio_tracks": [ … ], "subtitle_tracks": [ … ]
}
```

- `video_codec`, `width`, `height`: the rung the client starts on (its
  ladder's first variant, or the one `q` picked).
- `qualities`: `Auto` first, then one entry per quality name the client
  decodes at its heights, `Original` first, then tallest first, across
  families (a pick loads that rung's own master). `label` is the rung's
  [name](#quality-names) (`Original` for the original's stream copied,
  `1280x536` is `Medium (720p)`), the `NAME` of its variant in the master;
  `bitrate` is the variant's `BANDWIDTH`. Of two rungs of one name the one
  in the client's family is listed. `null` when there are fewer than two to
  pick from — every package before the ladder.
- `default_quality`: `auto`.
- `audio_tracks` lists the stereo tracks; `subtitle_tracks` the sidecars,
  each WebVTT one with its HLS rendition dir (`hls`) when it has one: the
  source's own tracks, then the subtitle files from next to it
  (`external`), whose `sN` count on past them.
- For a client served the one group of rule 4, `audio_tracks` lists that
  group's renditions instead, in the master's order, each with `codec`
  (`ec-3`, `mp4a.40.2`), `channels`, `default` as the master has it,
  `group` (its `GROUP-ID`) and `rendition` (`a2`, the folder its URI
  names); `audio_codec` is the codec of the one it starts on (`eac3`). A
  client picks a track by `name`, the master's `NAME` and unique in the
  group (hls.js: `setAudioOption({lang, name})`), or by `index`, its place
  there. Any other client's list is as it always was.
- A `native` client's list is the stereo tracks as always — the tracks its
  player lists, one per member, which the player plays from whichever
  group it picks. A track the 5.1 group plays by a companion carries
  `surround`: that group, the companion's `rendition`, `codec` and
  `channels`. Pick a track by `name` (AVPlayer's option `displayName`,
  Safari's audio track `label`); the player picks stereo or 5.1.
- Each track has a `name`: the packager's ("English", "No dialogue",
  "English · Commentary"), else - a package from before names - its
  language's name, then what its title says besides, the rule the packager
  names its renditions by. `title` is the same name, for a client that
  reads `title`; the source's title as it is (free text, often the source's
  codec: "AC3 5.1 @ 640 Kbps") is never sent. On the fly, `/info` names a
  source's tracks by the same rule, an audio track as the master's `NAME`
  does.
- A language's name says the script and the region its code names, in
  brackets, as the platforms name it in their standard form and chino-web
  names a track (`docs/chino/subtitles.md`): "Portuguese (Brazil)",
  "Chinese (Simplified)", "Spanish (Latin America)", never "Brazilian
  Portuguese". A title that names a script or a region of the track's
  language that its code does not names the track so — a code of the
  language ("PT-BR", "pt_BR", "zh-Hans", "ES-419"), a region's code in
  brackets where it is one of the language's regions ("Português (BR)",
  "Español (419)"; "Svenska (SV)" is "Swedish"), the region's or the
  script's name ("Portuguese (Brazil)", "Português (Brasil)", "Chinese
  (Simplified)"; a script's only where the language is not written in it
  anyway and CLDR does not name a language so: "Spanish (Latin)" is
  "Spanish · Latin", "English (Korean)" "English · Korean", "Hindi
  (Latin)" `hi-Latn`), the regional form's ("Brazilian Portuguese",
  "Español latino", "Flemish"), a Chinese title's script ("简体中文", "CHS")
  — and the track's `language` says them too where its code does not
  (`pt-BR` for a `por` track titled "PT-BR"; the master's `LANGUAGE` stays
  the file's code). A bracket that says only that goes wherever it stands,
  as chino-web reads every bracket of a title, and the rest stays:
  "[PT-BR]" is "Portuguese (Brazil)", "Commentary (PT-BR)" "Portuguese
  (Brazil) · Commentary", "SDH (US)" on `eng` "English (United States) ·
  SDH", "Mandarin (Simplified)" on `chi` "Chinese (Simplified) · Mandarin".
  A title that names the source's format as well says nothing else, but
  its script and region stay: "Português (Brasil) - Dolby Digital 5.1" and
  "PT-BR AC3 5.1" are "Portuguese (Brazil)". So two tracks of one language
  in two regions are two names, not "Portuguese" and "Portuguese (2)". A
  title that names another script or region than the code's stays:
  "Portuguese (Portugal) · PT-BR" on a `pt-PT` track. The names are CLDR's
  English ones (`internal/play/langtables.go`); the packager's names are
  its own.

A quality menu shows `qualities` when it has two or more entries, by
`label`, and on a pick reloads the master with `q=<name>` (`auto` back to
the ladder). Clients should send the same `caps` on the master, `/info` and
`/prewarm`.

**Warming.** Fetching a packaged master warms the variant the client starts
on — the served master's first variant and the default (else first)
rendition of its audio group, and for a `native` client the one each other
group starts on, since its player picks the group: media playlists and
init segments, and with `?t=` the segments from there. A master with a
mixed audio group — a variant naming more than one audio codec: the one
group of rule 4, a `native` client's 5.1 group that plays a member in
stereo — warms every other audio rendition's playlist and init too, since
Media3 reads each of them before it starts (a TV limited to stereo then
starts on the stereo twin); at most eight audio renditions in all. On the
fly from a package only what is a file read: the other companions'
playlists and inits. A stereo member's init is the first output of a
transcode window, which starts when a client asks for it. `/prewarm` warms
the same plus segments (from `t`, else the first ones); a Zap pool entry
warms the start of an HEVC client and of any other client at its seek
point. hls.js starts on the best level under
`min(first BANDWIDTH, 5 Mbit/s)`, which for an HEVC top rung above that is
the rung below it.

## Extras

A title's extras - its trailers, teasers, featurettes and other bonus
material - are packages of their own, laid out as an item's package without
trickplay, wherever katalog-api answers them: in the library
`<title>/extras/<extraId>/`, before it the package store's
`extras/<aa>/<extraId>/`. One is served under its title's id only:

```
GET /api/play/{itemId}/extras/{extraId}/master.m3u8            ?stream=&caps=&q=
GET /api/play/{itemId}/extras/{extraId}/{vN|aN|sN}/playlist.m3u8
GET /api/play/{itemId}/extras/{extraId}/{vN|aN}/iframes.m3u8, init.mp4, seg-{n}.m4s
GET /api/play/{itemId}/extras/{extraId}/{sN}/seg-{n}.vtt
GET /api/play/{itemId}/extras/{extraId}/{high|medium|low}/index.m3u8, init.mp4, {n}.m4s
GET /api/play/{itemId}/extras/{extraId}/audio/{n}/index.m3u8, init.mp4, {n}.m4s
```

- Every route answers `404` unless `extraId` is a UUID, katalog-api answers
  it as an extra of `itemId` (it answers only extras packaged and not
  removed), and its package is complete (`.complete`). chino-api holds a
  viewer to the rating of the title in the URL; the title check makes that
  the extra's own title.
- The master is the client's share of the ladder, by the rules above, its
  URIs carrying the request's query. Extras are packaged HEVC only too: a
  client that decodes none of an extra's rungs gets its on-the-fly master,
  transcoded from the extra's package as a title's is.
- An extra is no item: it is not among the packaged ids nor in the Zap pool,
  it has no `/info`, `/prewarm` or trickplay, and `/api/play/{extraId}/…`
  finds no package of it.

## Configuration

All config is environment-driven (defaults in parentheses):

| Variable               | Default                                              |
|------------------------|------------------------------------------------------|
| `LISTEN_ADDR`          | `:8080`                                              |
| `KATALOG_API_BASE_URL` | `http://katalog-api.zaentrum.svc.cluster.local`      |
| `MEDIA_ROOT`           | `/var/lib/katalog/media` (the library's share, `/var/lib/katalog`, in the library) |
| `HLS_CACHE_DIR`        | `/var/cache/katalog-hls`                             |
| `OIDC_ISSUER`          | `https://sso.example.com/realms/zaentrum`            |
| `OIDC_AUDIENCE`        | `chino-web`                                           |
| `AUTH_ENABLED`         | `true`                                               |
| `FFMPEG_BIN` / `FFPROBE_BIN` | `ffmpeg` / `ffprobe`                           |
| `TRANSCODE_PRESET`     | `veryfast`                                            |
| `USE_NVENC`            | `false`                                              |
| `NVENC_PRESET` / `NVENC_CQ` | `p5` / `23`                                     |
| `LIVE_WORKER_URL`      | (none) — the live worker, e.g. `http://live-worker`  |
| `LIVE_WORKER_MODE`     | `off` — `off`, `shadow` or `on` ([above](#the-live-worker)) |
| `LIVE_WORKER_TOKEN`    | (none) — the worker's bearer token                   |

## Local development

```bash
go run ./cmd/chino-stream
curl -fsS http://localhost:8080/healthz
```

`ffmpeg` and `ffprobe` must be on `$PATH` for transcoding. NVENC paths
additionally need an NVIDIA GPU plus an ffmpeg build with the NVIDIA
encoders linked in. Audio is re-encoded to AAC-LC stereo with whichever AAC
encoder the ffmpeg build has: `libfdk_aac` when it was built with it, otherwise
ffmpeg's native `aac` encoder. The choice is logged at startup.

## Build the container

```bash
docker build -t zaentrum/chino-stream .
```

The image ships `ffmpeg` and `ffprobe` from BtbN's static GPL build of
ffmpeg 7.1 (n7.1.5, the dated `autobuild-2026-07-31-14-10` archive, checked
against its sha256; the transcoder image pins the same archive): NVENC
(`h264_nvenc`, `hevc_nvenc`), the CUDA filters (`scale_cuda` with `format`),
libx264 and ffmpeg's native AAC encoder. Ubuntu 22.04's ffmpeg 4.4 could do
neither the NVENC path nor the stream-copy plan's keyframe probe. A mirror
passes `--build-arg FFMPEG_BUILD_URL=… --build-arg FFMPEG_SHA256=…`. The CUDA
base stays `nvidia/cuda:12.3.2-runtime-ubuntu22.04`; the build's NVENC SDK
wants an NVIDIA driver ≥ ~550 on the GPU node.

A private deploy mirror can pass `--build-arg BASE=<registry>/library/` to
pull base images from an internal registry. Build and push the image to your
own registry and update the image reference in the `k8s/` manifests for your
environment.

## License

[MPL-2.0](LICENSE).
