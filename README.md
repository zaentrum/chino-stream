# chino-stream

Streaming origin for the **chino** product on the zaentrum platform. Serves
on-demand playback: pre-packaged CMAF byte-range delivery when an asset has
already been packaged, and on-the-fly HLS transcoding (optionally NVENC-
accelerated) for codecs a client can't decode (HEVC / DTS / AC3 / TrueHD) —
from the original, or once that is retired from the package itself.

It asks the catalog API (the read side of the CQRS split) where an item's
original and package are rather than touching the catalog database, and
never works a path out itself ([below](#where-a-title-plays-from)).

## Layout

```
cmd/chino-stream/main.go        # entrypoint, env config, chi router wiring
internal/http/router.go         # routes: /healthz, /readyz, /metrics, /api/play/*
internal/auth/                  # OIDC bearer verification + per-stream token checks
internal/catalog/client.go      # thin HTTP client to the catalog API
internal/play/                  # playback core
  resolve.go                    #   where an item's or an extra's package is (katalog-api, cached)
  packaged.go                   #   pre-packaged CMAF byte-range serving
  ladder.go                     #   a packaged master as each client is served it
  extras.go                     #   a title's extras (trailers, …), packaged apart from it
  passthrough.go                #   direct-play passthrough
  hls.go / transcode.go         #   on-demand HLS + ffmpeg/NVENC encoder args
  source.go                     #   what on-the-fly reads: the original, else the package
  window.go                     #   a window's segments served as ffmpeg finishes them
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
| GET    | `/readyz`                                    | readiness: 503 while ffmpeg has no AAC encoder |
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
| GET    | `/api/play/{itemId}/extras/{extraId}/master.m3u8` | an extra's HLS master ([below](#extras)) |
| GET    | `/api/play/{itemId}/extras/{extraId}/{vN\|aN\|sN}/…` | its renditions, as a packaged title's |
| GET    | `/api/play/{itemId}/extras/{extraId}/{high\|medium\|low}/…`, `audio/{n}/…` | its on-the-fly HLS |

Every `/api/play` route takes a bearer or `?stream=<token>` (chino-api's
stream token). The URIs inside a playlist carry the request's query, so the
token, `caps` and `q` ride along on every fetch — and `v`, the version a
session is pinned to.

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

## Packaged playback: what a client is served

A packaged title's master, `/info` and `/prewarm` read the same query:

- `caps` — what the client decodes, comma-separated: video `avc`, `hvc`,
  `av1`, `vp9`, each with an optional `:<height>` (the hardware decoder's
  tallest frame, e.g. `hvc:1080`); audio `aac`, `mp3`, `opus`, `vorbis`,
  `ac3`, `eac3`; `aacmc` for AAC beyond two channels. Without `caps` the
  default set applies: H.264, VP9, AV1, AAC, MP3, Opus, Vorbis — no HEVC.
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
    {"name": "v1", "id": "v1", "label": "720p", "width": 1280, "height": 720,
     "codec": "avc1.64001f", "bitrate": 1505267, "video_range": "SDR"},
    {"name": "v2", "id": "v2", "label": "480p", "width": 854, "height": 480,
     "codec": "avc1.64001e", "bitrate": 706649, "video_range": "SDR"}
  ],
  "audio_tracks": [ … ], "subtitle_tracks": [ … ]
}
```

- `video_codec`, `width`, `height`: the rung the client starts on (its
  ladder's first variant, or the one `q` picked).
- `qualities`: `Auto` first, then one entry per picture size the client
  decodes at its heights, tallest first, across families (a pick loads that
  rung's own master). `label` names the 16:9 box the frame fits
  (`1280x536` is `720p`), `bitrate` is the variant's `BANDWIDTH`. Of two
  rungs of one size the one in the client's family is listed. `null` when
  there are fewer than two to pick from — every package before the ladder.
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

A quality menu shows `qualities` when it has two or more entries, by
`label`, and on a pick reloads the master with `q=<name>` (`auto` back to
the ladder). Clients should send the same `caps` on the master, `/info` and
`/prewarm`.

**Warming.** Fetching a packaged master warms the variant the client starts
on — the served master's first variant and the default (else first)
rendition of its audio group, and for a `native` client the one each other
group starts on, since its player picks the group: media playlists and
init segments, and with `?t=` the segments from there. `/prewarm` warms the same plus segments
(from `t`, else the first ones); a Zap pool entry warms the start of an
HEVC client and of any other client at its seek point. hls.js starts on the
best level under `min(first BANDWIDTH, 5 Mbit/s)`, which for an HEVC top
rung above that is the rung below it.

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
