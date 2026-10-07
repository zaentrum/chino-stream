package play

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/chino-stream/internal/pkgmanifest"
)

// What an on-the-fly transcode reads (hls.go): the original file while there
// is one — today's path — else the package. In the library every original
// is deleted once its package is recorded, and packages are HEVC only, so a
// client that decodes none of a package's rungs (no HEVC, or HEVC under the
// package's height) is served a transcode of the package itself: its top
// video rendition and its stereo AAC renditions as ffmpeg's inputs, and a
// probe made of its manifest (packageProbe).
//
// A rendition is read straight from its segments. ffmpeg's HLS demuxer
// cannot seek in a packager's fMP4 rendition: after a seek it feeds the
// target segment to its mp4 demuxer from byte position 0 again, whose
// fragment index (keyed by byte offset) maps the segment onto the first
// one's, and every packet is dropped to the end; -ss 0 lands before the
// playlist's first timestamp (a B-frame stream's dts is negative), fails,
// and starts at whatever the probe had read past (ffmpeg 7.1 and 8.1 alike).
// So the window's segments are picked from the rendition's playlist and
// read as one fragmented MP4, concat:init.mp4|seg-N.m4s|…, whose timestamps
// are the package's own (tfdt). -seek_timestamp 1 makes -ss that timestamp,
// not one counted from the start of the concatenation. Decoded so, the
// frames of a window are those of the original at the same times, frame
// for frame (mid-GOP, on a keyframe, open-GOP HEVC with B-frames, HDR10 —
// checked against the original with framemd5), and the window's segments
// are those of the original's transcode.

// source is one item's or extra's on-the-fly input.
type source struct {
	// key names the transcode's cache (cachePath): the item id for its
	// original, a key of its own per package (sourceKey).
	key string
	// file is the original's path; "" when the package is read.
	file string
	// pkg and mf are the package read, nil for a file.
	pkg *pkgDir
	mf  *pkgmanifest.Manifest
	// probe is the file's ffprobe, or the package's (packageProbe).
	probe *Probe
}

// fromPackage reports whether s reads a package.
func (s *source) fromPackage() bool { return s.pkg != nil }

// label names s in ffmpeg's log lines: the original's file name, or the
// package's version (its folder's name before the library).
func (s *source) label() string {
	if !s.fromPackage() {
		return filepath.Base(s.file)
	}
	if s.pkg.versionID != "" {
		return "package " + s.pkg.versionID
	}
	return "package " + filepath.Base(s.pkg.dir)
}

// errNotPlayable answers a title whose original is retired and whose
// package katalog-api names but the storage does not have complete: only a
// backup can bring it back.
var errNotPlayable = errors.New("not playable: the original was retired and its package is missing")

// errNeedsOriginal answers what only the original can serve.
var errNeedsOriginal = errors.New("progressive playback needs the original")

// packageSource is the package of the item itemID as an on-the-fly source,
// the version pin selects as for its renditions; nil when there is none.
// notPlayable when katalog-api names a package none of whose folders can be
// read (missing, not complete, its record broken).
func packageSource(ctx context.Context, packages *Resolver, itemID, pin string) (src *source, notPlayable bool) {
	p, mf, a, err := packages.itemPackageManifest(ctx, itemID, pin)
	if p != nil && mf != nil && len(mf.Renditions.Video) > 0 {
		probe := packageProbe(mf)
		return &source{key: sourceKey(itemID, p), pkg: p, mf: mf, probe: &probe}, false
	}
	return nil, err == nil && (p != nil || hasPackage(a))
}

// sourceKey is the transcode cache's key of the package p of id: another
// version (or, before the library, the package written again) is another
// key, so a transcode of one is never served for the other.
func sourceKey(id string, p *pkgDir) string {
	if p.versionID != "" {
		return id + "@" + p.versionID
	}
	return id + "@package"
}

// packageProbe is the Probe of a package as its manifest describes it: the
// top video rendition's codec, size, bit rate and range, the package's
// duration, its stereo AAC renditions as the audio tracks (0:a:N is the
// N-th of them), and its WebVTT subtitles of the original's own tracks as
// the subtitle tracks, numbered as the original's streams were (sub<N>).
func packageProbe(mf *pkgmanifest.Manifest) Probe {
	p := Probe{Container: "hls", DurationMs: mf.EffectiveDurationMs(), AudioProfile: "LC"}
	if len(mf.Renditions.Video) > 0 {
		v := mf.Renditions.Video[0]
		p.VideoCodec = codecFamily(v.Codec)
		p.Width, p.Height = v.Width, v.Height
		p.BitRate = int64(v.PeakBitrateBps)
		if p.BitRate == 0 {
			p.BitRate = int64(v.BitrateBps)
		}
		switch {
		case v.VideoRange == "HLG":
			p.ColorTransfer, p.ColorPrimaries, p.ColorSpace = "arib-std-b67", "bt2020", "bt2020nc"
		case v.VideoRange == "PQ" || (v.HDR && v.VideoRange == ""):
			p.ColorTransfer, p.ColorPrimaries, p.ColorSpace = "smpte2084", "bt2020", "bt2020nc"
		}
	}
	for i, a := range mf.Renditions.Audio {
		if i == 0 {
			p.AudioCodec = "aac"
		}
		p.AudioTracks = append(p.AudioTracks, TrackInfo{Index: i, Codec: "aac", Language: normalizeLang(a.Language),
			Title: a.Title, Default: a.Default, Channels: a.Channels})
	}
	for _, s := range mf.Subtitles {
		n, ok := subtitleOrdinal(s)
		if !ok || s.External || s.Format != "webvtt" {
			continue
		}
		p.SubtitleTracks = append(p.SubtitleTracks, TrackInfo{Index: n, Codec: "webvtt", Language: normalizeLang(s.Language),
			Title: s.Title, Default: s.Default, Forced: s.Forced})
	}
	return p
}

// subtitleOrdinal is N of a package subtitle sub<N>: the original's
// subtitle stream it was made from (0:s:N), as the packager numbers them.
func subtitleOrdinal(s pkgmanifest.Subtitle) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(s.ID, "sub"))
	if err != nil || n < 0 || !strings.HasPrefix(s.ID, "sub") {
		return 0, false
	}
	return n, true
}

// packageSubtitle is the package's subtitle of the original's subtitle
// stream n, nil when it has none.
func packageSubtitle(mf *pkgmanifest.Manifest, n int) *pkgmanifest.Subtitle {
	for i, s := range mf.Subtitles {
		if k, ok := subtitleOrdinal(s); ok && k == n && !s.External {
			return &mf.Subtitles[i]
		}
	}
	return nil
}

// input is the ffmpeg input of a window of s: [start, start+dur) seconds of
// the file, or of a package's rendition dir (its video's for video, an audio
// rendition's for audio), the -ss, the input and the -map of its stream.
func (s *source) input(rendDir, stream string, start, dur int) ([]string, error) {
	if !s.fromPackage() {
		return []string{"-ss", strconv.Itoa(start), "-i", s.file}, nil
	}
	url, err := packageWindow(s.pkg.path(rendDir), float64(start), float64(dur))
	if err != nil {
		return nil, err
	}
	return []string{"-seek_timestamp", "1", "-ss", strconv.Itoa(start), "-i", url}, nil
}

// windowPreroll is how far before a window's start its first segment is
// looked for. A playlist's EXTINF timeline runs ahead of the media's by up
// to a frame reordering delay (shaka's first segment is that much shorter),
// so the segment holding the window's first frame may be the one before the
// one EXTINF names.
const windowPreroll = 1.0

// packageWindow is the concat: URL of the rendition at dir that covers
// [start, start+dur) seconds: its init segment and the media segments from
// the one holding start (less windowPreroll) to the one holding the end,
// all listed by its playlist.m3u8.
func packageWindow(dir string, start, dur float64) (string, error) {
	pl, err := readRendition(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		return "", err
	}
	if len(pl.segments) == 0 {
		return "", fmt.Errorf("rendition %s lists no segments", dir)
	}
	first, last := 0, len(pl.segments)-1
	from, to := start-windowPreroll, start+dur
	for i, sg := range pl.segments {
		if sg.start <= from {
			first = i
		}
		if sg.start < to {
			last = i
		}
	}
	files := []string{filepath.Join(dir, pl.init)}
	for _, sg := range pl.segments[first : last+1] {
		files = append(files, filepath.Join(dir, sg.uri))
	}
	for _, f := range files {
		if strings.ContainsRune(f, '|') {
			return "", fmt.Errorf("rendition file %q cannot be read through concat:", f)
		}
	}
	return "concat:" + strings.Join(files, "|"), nil
}

// rendition is a rendition's media playlist as packageWindow reads it.
type rendition struct {
	init     string // its EXT-X-MAP
	segments []renditionSegment
}

type renditionSegment struct {
	uri   string
	start float64 // seconds, summed from the playlist's EXTINFs
}

// renditions caches readRendition per path, by mtime.
var renditions sync.Map // map[string]renditionEntry

type renditionEntry struct {
	r     *rendition
	mtime time.Time
}

// readRendition reads the media playlist at path (through packagedCache,
// the bytes a packaged client is served). Its URIs must be file names in
// its own folder, as the packager writes them.
func readRendition(path string) (*rendition, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if v, ok := renditions.Load(path); ok {
		if e := v.(renditionEntry); e.mtime.Equal(st.ModTime()) {
			return e.r, nil
		}
	}
	raw, err := readPackagedBytes(path, st.ModTime())
	if err != nil {
		return nil, err
	}
	r := &rendition{}
	var at float64
	pending := -1.0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			r.init = hlsAttributes(line)["URI"]
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if c := strings.IndexByte(v, ','); c >= 0 {
				v = v[:c]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				pending = d
			}
		case line == "" || strings.HasPrefix(line, "#"):
		case pending >= 0:
			r.segments = append(r.segments, renditionSegment{uri: line, start: at})
			at += pending
			pending = -1
		}
	}
	if r.init == "" {
		return nil, fmt.Errorf("rendition %s has no init segment (EXT-X-MAP)", path)
	}
	for _, name := range append([]string{r.init}, segmentURIs(r)...) {
		if name == "" || name != filepath.Base(name) || name == ".." || strings.ContainsAny(name, "?#|\\") {
			return nil, fmt.Errorf("rendition %s names %q, not a file of its folder", path, name)
		}
	}
	renditions.Store(path, renditionEntry{r: r, mtime: st.ModTime()})
	return r, nil
}

func segmentURIs(r *rendition) []string {
	out := make([]string, len(r.segments))
	for i, s := range r.segments {
		out[i] = s.uri
	}
	return out
}

// decideSource is the on-the-fly mode and its reason for s and the client:
// the original's, as DecideWith says (passthrough, remux, transcode); a
// package's is always a transcode — the client decodes none of its rungs at
// its height, and nothing of a package is stream-copied.
func decideSource(s *source, caps Caps) (mode, reason string) {
	mode, reason = s.probe.DecideWith(caps)
	if !s.fromPackage() {
		return mode, reason
	}
	if mode != "transcode" {
		reason = fmt.Sprintf("the package's %s video at %dp is over the client's height for it", s.probe.VideoCodec, s.probe.Height)
	}
	return "transcode", reason + "; the original is retired, so the package's video is transcoded on the fly"
}

// audioChoice is one rendition of the audio group an on-the-fly master of a
// package serves a client that decodes its 5.1 companions (onTheFlyUnion).
type audioChoice struct {
	// uri is the rendition's media playlist, relative to the master, and
	// rendition the folder it names: a companion is served as packaged
	// ("a2/playlist.m3u8", the package's own rendition, which the on-the-fly
	// video is on the timeline of), a stereo track transcoded on the fly
	// ("audio/1/index.m3u8").
	uri, rendition string
	name, language string
	codec          string // its CODECS entry: ec-3, mp4a.40.2
	channels       int
	bitrate        int // bit/s
	isDefault      bool
}

// companion says whether c is a package's 5.1 companion, served as
// packaged, rather than a stereo track transcoded on the fly.
func (c audioChoice) companion() bool { return !strings.HasPrefix(c.rendition, "audio/") }

// surroundBitrate is the BANDWIDTH counted for a companion whose manifest
// gives no bit rate: the packager's SURROUND_BITRATE default.
const surroundBitrate = 448_000

// onTheFlyUnion is the audio group of the on-the-fly master of the package
// src for a client with caps that decodes its 5.1 companions, as
// serveUnion makes a packaged master's: per stereo rendition, in order, its
// companion (paired by source track, else by language) just before its
// on-the-fly stereo transcode, the companions nobody's twin after them, the
// default track's companion DEFAULT (else that track), names unique. nil —
// the stereo group as before — when src is no package, it has no
// companions, or the client decodes them not.
func onTheFlyUnion(src *source, caps Caps) []audioChoice {
	if src == nil || !src.fromPackage() || src.mf == nil || src.probe == nil || len(src.mf.Renditions.AudioSurround) == 0 {
		return nil
	}
	surround := src.mf.Renditions.AudioSurround
	for _, a := range surround {
		if !audioCodecOK(a.Codec, a.Channels, caps) {
			return nil
		}
	}
	stereo := src.mf.Renditions.Audio
	twins := twinsOf(src.mf)
	companion := make([]int, len(stereo)) // stereo index → surround index + 1 (0: none)
	used := make([]bool, len(surround))
	for ci, c := range surround {
		if twin, ok := twins[renditionID(c)]; ok {
			for si, a := range stereo {
				if renditionID(a) == twin && companion[si] == 0 {
					companion[si], used[ci] = ci+1, true
					break
				}
			}
		}
	}
	for ci, c := range surround {
		if _, ok := twins[renditionID(c)]; ok || used[ci] {
			continue
		}
		for si, a := range stereo {
			if companion[si] == 0 && normalizeLang(a.Language) == normalizeLang(c.Language) {
				companion[si], used[ci] = ci+1, true
				break
			}
		}
	}
	tracks := src.probe.AudioTracks
	names := audioRenditionNames(tracks)
	def := defaultAudioIndex(tracks)
	surroundChoice := func(c pkgmanifest.AudioRendition, isDefault bool) audioChoice {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = trackDisplayName(c.Language, c.Title) + " 5.1"
		}
		ch := audioChoice{uri: renditionID(c) + "/playlist.m3u8", rendition: renditionID(c), name: name,
			language: normalizeLang(c.Language), codec: c.Codec, channels: c.Channels, bitrate: c.BitrateBps, isDefault: isDefault}
		if ch.channels == 0 {
			ch.channels = 6
		}
		if ch.bitrate == 0 {
			ch.bitrate = surroundBitrate
		}
		return ch
	}
	var out []audioChoice
	for i, t := range tracks {
		hasCompanion := i < len(companion) && companion[i] > 0
		if hasCompanion {
			out = append(out, surroundChoice(surround[companion[i]-1], t.Index == def))
		}
		lang := t.Language
		if lang == "" {
			lang = "und"
		}
		out = append(out, audioChoice{uri: fmt.Sprintf("audio/%d/index.m3u8", t.Index), rendition: fmt.Sprintf("audio/%d", t.Index),
			name: names[i], language: lang, codec: "mp4a.40.2", channels: 2, bitrate: windowAudioBitrateBps,
			isDefault: t.Index == def && !hasCompanion})
	}
	for ci, c := range surround {
		if !used[ci] {
			out = append(out, surroundChoice(c, false))
		}
	}
	listed := make([]string, len(out))
	for i, c := range out {
		listed[i] = c.name
	}
	for i, name := range uniqueNames(listed) {
		out[i].name = name
	}
	return out
}

// unionDefault is the choice of an onTheFlyUnion list a player starts on.
func unionDefault(choices []audioChoice) *audioChoice {
	for i := range choices {
		if choices[i].isDefault {
			return &choices[i]
		}
	}
	if len(choices) > 0 {
		return &choices[0]
	}
	return nil
}

// onTheFlyInfoTracks are /play/info's audio_tracks for an onTheFlyUnion
// list, in its order: index (its place), codec, language, name and title
// (its NAME in the master), default (only when it is), channels, and group
// and rendition, which a client may pick it by.
func onTheFlyInfoTracks(choices []audioChoice) []map[string]any {
	out := make([]map[string]any, 0, len(choices))
	for i, c := range choices {
		e := map[string]any{"index": i, "codec": codecName(c.codec), "language": c.language, "name": c.name, "title": c.name,
			"channels": c.channels, "group": onTheFlyAudioGroup, "rendition": c.rendition}
		if c.isDefault {
			e["default"] = true
		}
		out = append(out, e)
	}
	return out
}

// onTheFlyAudioGroup is the GROUP-ID of an on-the-fly master's audio.
const onTheFlyAudioGroup = "aud"
