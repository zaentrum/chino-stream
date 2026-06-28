// Package play wraps ffprobe + ffmpeg. ffprobe is used to decide whether a
// file is browser-decodable (H.264 + AAC/MP3 → direct stream) or whether
// we have to transcode it on the fly (HEVC, DTS, AC3, TrueHD, …).
package play

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Caps describes what the requesting client can decode. The server
// uses these to pick a pipeline that produces bytes the client will
// accept first try instead of relying on the circuit breaker.
//
// Membership in the codec sets is intentionally narrow — Chrome's
// MediaSource.isTypeSupported lies (e.g. claims hvc1 on Windows then
// rejects the actual frames), so DefaultCaps only includes what
// every modern browser handles natively. The client signals beyond
// the default via the ?caps=<csv> querystring.
type Caps struct {
	// Set of decoded video codec names from ffprobe (h264, hevc, av1,
	// vp9, ...). nil set = use DefaultCaps.Video.
	Video map[string]bool
	// Same for audio codec names (aac, mp3, opus, ac3, eac3, ...).
	Audio map[string]bool
	// AACMultichannel is the result of the spec-extension probe
	// `isTypeSupported('audio/mp4; codecs="mp4a.40.2"; channels="6"')`.
	// When false, sources with >2 audio channels go through path C
	// (audio re-encode to stereo, video stream-copy) even if the
	// codec itself is browser-OK.
	AACMultichannel bool
	// VideoMaxHeight is the per-codec-family ceiling (in frame pixels)
	// the device HARDWARE decoder can handle for that codec. Keys match
	// codecFamily(): "h264", "hevc", "av1", "vp9". A missing entry or a
	// 0 value means "no known HW height limit" — the codec decodes at
	// any resolution the device advertises (legacy behaviour).
	//
	// Carried on the ?caps= wire as a ":<maxHeight>" suffix on a video
	// codec token, e.g. "hvc:1080" → VideoMaxHeight["hevc"]=1080. This
	// lets the server route a 4K-HEVC package away from a device whose
	// HEVC decoder tops out at 1080 (where it would otherwise fall back
	// to the software decoder and play unplayably slowly — the SM-T500
	// Zap bug) and onto the libx264 transcode ladder instead.
	VideoMaxHeight map[string]int
}

// MaxVideoHeight returns the tallest frame height the device can HW-
// decode in ANY codec family it carries a ceiling for, i.e. the largest
// value in VideoMaxHeight. Returns 0 when no ceiling is set (no cap):
// either the map is nil/empty or every entry is 0. Used as the upper
// bound on transcode OUTPUT height so the source-resolution "high" rung
// can't re-serve a 4K frame the device still can't HW-decode.
func (c Caps) MaxVideoHeight() int {
	max := 0
	for _, h := range c.VideoMaxHeight {
		if h > max {
			max = h
		}
	}
	return max
}

// DefaultCaps is the safe baseline used when the client didn't send a
// caps signal. Matches what Chrome / Edge / Firefox / Safari all
// handle without surprises in fragmented MP4.
var DefaultCaps = Caps{
	Video: map[string]bool{"h264": true, "avc1": true, "vp9": true, "av1": true},
	Audio: map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true},
	// Most desktop browsers reject multi-channel AAC in fmp4
	// segments; Safari is the exception. Default off, let clients
	// opt in.
	AACMultichannel: false,
	// No HW height ceilings in the baseline — browsers/desktops decode
	// at any resolution. An empty (non-nil) map = "no cap on anything".
	VideoMaxHeight: map[string]int{},
}

// ParseCaps turns a comma-separated capability string from the
// querystring into a Caps. Recognised tokens (any case, any order):
//
//	avc, hvc, av1, vp9                 → video
//	aac, mp3, opus, vorbis, ac3, eac3  → audio
//	aacmc                              → multi-channel AAC LC
//
// A VIDEO token MAY carry an optional ":<maxHeight>" suffix giving the
// max frame HEIGHT the device HARDWARE decoder supports for that codec,
// e.g. "hvc:1080" or "av1:2160". The base token (everything before the
// ":") parses exactly as before, so legacy suffix-less tokens are
// unchanged. The height is recorded in VideoMaxHeight under the codec
// FAMILY key (matching codecFamily(): "h264", "hevc", "av1", "vp9").
// Audio tokens never carry a height; any ":" on them is ignored.
//
// Empty / missing input → DefaultCaps. Unknown tokens are silently
// ignored so we can add new ones without breaking older clients.
func ParseCaps(s string) Caps {
	if s == "" {
		return DefaultCaps
	}
	c := Caps{Video: map[string]bool{}, Audio: map[string]bool{}, VideoMaxHeight: map[string]int{}}
	for _, raw := range strings.Split(s, ",") {
		tok := strings.ToLower(strings.TrimSpace(raw))
		// Split off an optional ":<maxHeight>" suffix. The base parses
		// with the existing switch unchanged; the trailing integer (when
		// present and valid) is recorded against the codec family below.
		t := tok
		var height int
		if c := strings.IndexByte(tok, ':'); c >= 0 {
			t = tok[:c]
			if h, err := strconv.Atoi(strings.TrimSpace(tok[c+1:])); err == nil && h > 0 {
				height = h
			}
		}
		// family is the VideoMaxHeight key for video tokens carrying a
		// height; "" for audio/unknown tokens (height then ignored).
		family := ""
		switch t {
		case "avc", "h264":
			c.Video["h264"] = true
			c.Video["avc1"] = true
			family = "h264"
		case "hvc", "hevc", "h265":
			c.Video["hevc"] = true
			c.Video["h265"] = true
			c.Video["hvc1"] = true
			family = "hevc"
		case "av1":
			c.Video["av1"] = true
			family = "av1"
		case "vp9":
			c.Video["vp9"] = true
			family = "vp9"
		case "aac":
			c.Audio["aac"] = true
		case "mp3":
			c.Audio["mp3"] = true
		case "opus":
			c.Audio["opus"] = true
		case "vorbis":
			c.Audio["vorbis"] = true
		case "ac3":
			c.Audio["ac3"] = true
		case "eac3", "ec3":
			c.Audio["eac3"] = true
			c.Audio["ec-3"] = true
		case "aacmc":
			c.AACMultichannel = true
		}
		// Record the HW height ceiling for video tokens that carried a
		// valid ":<height>" suffix. Keep the larger value if the same
		// family appears twice (e.g. "hvc:1080,hevc:2160").
		if family != "" && height > 0 {
			if cur, ok := c.VideoMaxHeight[family]; !ok || height > cur {
				c.VideoMaxHeight[family] = height
			}
		}
	}
	return c
}

// Browsers can play H.264 / VP9 / AV1 video universally. HEVC support
// is per-browser (Safari yes, Linux Chrome no, Windows Chrome lies);
// callers should use Caps.Video rather than this map.
var browserVideoOK = DefaultCaps.Video

// AAC / MP3 / Opus / Vorbis are the safe baseline. AC3/EAC3 work on
// some browsers via OS codecs but not in MSE fmp4 reliably.
var browserAudioOK = DefaultCaps.Audio

// TrackInfo is the per-stream metadata the player needs to render an
// audio / subtitle switcher: the stream index inside the source file
// (the N in `0:a:N` / `0:s:N`), the codec, the ISO-639 language tag
// extracted from the stream's `tags.language` (or "und"), an optional
// human-readable title, and the default / forced flags ffprobe pulls
// from the disposition.
type TrackInfo struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	Channels int    `json:"channels,omitempty"`
}

type Probe struct {
	Container  string
	VideoCodec string
	Width      int
	Height     int
	DurationMs int64
	AudioCodec string

	// Video color metadata. Only set for the first video stream. Empty
	// strings mean ffprobe didn't report the field (typical of SDR
	// sources muxed without explicit color tagging). The values are the
	// raw ffprobe strings, e.g. "smpte2084", "bt2020nc", "bt709".
	ColorTransfer  string
	ColorPrimaries string
	ColorSpace     string

	// Per-stream metadata the player uses for audio + subtitle switching.
	// Indexed inside their kind (audio[N] is `0:a:N` in ffmpeg).
	AudioTracks    []TrackInfo
	SubtitleTracks []TrackInfo
}

// IsHDR reports whether the source carries an HDR transfer function
// (PQ / HLG). The video filter chain only needs the zscale tonemap
// stage when this is true — SDR sources (or sources with no transfer
// tag at all) fail with "no path between colorspaces" if forced
// through the tonemap chain, so they get a straight downscale instead.
func (p Probe) IsHDR() bool {
	switch strings.ToLower(p.ColorTransfer) {
	case "smpte2084", "arib-std-b67":
		return true
	}
	return false
}

// PrimaryAudioChannels returns the channel count of the default
// audio track (the one that maps to ffmpeg `0:a:0` and that the
// client implicitly hears). 0 when ffprobe couldn't determine it.
func (p Probe) PrimaryAudioChannels() int {
	if len(p.AudioTracks) == 0 {
		return 0
	}
	idx := defaultAudioIndex(p.AudioTracks)
	for _, t := range p.AudioTracks {
		if t.Index == idx {
			return t.Channels
		}
	}
	return p.AudioTracks[0].Channels
}

// NeedTranscode is true when at least one stream is in a codec the browser
// won't decode under DefaultCaps. Kept for legacy callers; pipeline
// dispatch should use DecideWith(caps) instead.
func (p Probe) NeedTranscode() bool {
	return !browserVideoOK[p.VideoCodec] || !browserAudioOK[p.AudioCodec]
}

// containerNeedsRepackaging is true when the source format isn't
// fragmented-MP4-friendly and must be repackaged before MSE will
// accept it. Pure container check — codec compatibility is handled
// separately by DecideWith so this stays correct regardless of caps.
func (p Probe) containerNeedsRepackaging() bool {
	return strings.EqualFold(p.Container, "matroska,webm") ||
		strings.EqualFold(p.Container, "matroska") ||
		strings.EqualFold(p.Container, "avi")
}

// NeedRemux preserved for callers that still want the "does this
// need ANY pipeline work" answer using DefaultCaps. New code should
// use DecideWith(caps) which considers client capabilities.
func (p Probe) NeedRemux() bool {
	if p.NeedTranscode() {
		return false
	}
	return p.containerNeedsRepackaging()
}

// Decide returns the dispatch mode using DefaultCaps. Use
// DecideWith(caps) when client capabilities are available.
func (p Probe) Decide() (mode, reason string) {
	return p.DecideWith(DefaultCaps)
}

// DecideWith returns the dispatch mode tailored to a specific
// client's capabilities. Four modes, ordered by cost:
//
//	"passthrough" — video + audio + container all client-OK.
//	                Server runs `-c copy` on both tracks.
//	"remux"       — video client-OK but audio or container isn't.
//	                Server stream-copies video, re-encodes audio to
//	                stereo AAC LC, repackages as fmp4.
//	"transcode"   — video codec not client-OK (HEVC on most desktop
//	                Chromes, or anything exotic). libx264/NVENC +
//	                audio re-encode at the requested quality rung.
//
// Decision order matters: video drives the pipeline (transcode is
// the expensive part), audio just sets the audio sub-mode. The
// resulting `mode` is what hls.go::Master uses to choose between
// /copy/ and /{q}/ routes.
func (p Probe) DecideWith(caps Caps) (mode, reason string) {
	videoOK := caps.Video[p.VideoCodec]
	audioCodecOK := caps.Audio[p.AudioCodec]
	channels := p.PrimaryAudioChannels()
	// Multi-channel AAC LC in fmp4: Chrome desktop rejects it via
	// bufferAppendError unless the client explicitly opts in via
	// AACMultichannel (Safari, some Chromebooks).
	audioOK := audioCodecOK
	if audioOK && p.AudioCodec == "aac" && channels > 2 && !caps.AACMultichannel {
		audioOK = false
	}

	if !videoOK {
		switch {
		case !audioCodecOK:
			return "transcode", "video codec " + q(p.VideoCodec) + " and audio codec " + q(p.AudioCodec) + " both need re-encoding"
		default:
			return "transcode", "video codec " + q(p.VideoCodec) + " is not in the client's decoder set"
		}
	}
	// Video is client-OK from here.
	if !audioOK {
		why := "audio codec " + q(p.AudioCodec) + " is not in the client's decoder set"
		if audioCodecOK && p.AudioCodec == "aac" && channels > 2 {
			why = fmt.Sprintf("source audio is %d-channel AAC and the client didn't signal multichannel support — downmixing to stereo", channels)
		}
		return "remux", why + " — video stream-copied, audio re-encoded to stereo AAC LC"
	}
	if p.containerNeedsRepackaging() {
		return "remux", "container (" + p.Container + ") needs repackaging to fragmented MP4; codecs stream-copied"
	}
	return "passthrough", "codecs, channels and container are all client-compatible — file streamed directly with byte-range support"
}

func q(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return "'" + s + "'"
}

// RunFFprobe inspects the streams and the format. Pulls per-stream
// language / title / disposition for every audio + subtitle stream so
// the player can render switchers.
func RunFFprobe(ctx context.Context, ffprobeBin, path string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// -select_streams only accepts a single specifier (v:0 OR a:0, never
	// both). Pull all streams and filter by codec_type in Go.
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	var raw struct {
		Streams []struct {
			CodecType      string `json:"codec_type"`
			CodecName      string `json:"codec_name"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
			Channels       int    `json:"channels"`
			ColorTransfer  string `json:"color_transfer"`
			ColorPrimaries string `json:"color_primaries"`
			ColorSpace     string `json:"color_space"`
			Tags           struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
				Forced  int `json:"forced"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Probe{}, fmt.Errorf("ffprobe parse: %w", err)
	}

	p := Probe{Container: raw.Format.FormatName}
	// Track the running per-kind index. ffmpeg's `-map 0:a:N` uses the
	// audio-stream ordinal (0-based within audio streams), not the global
	// stream index — so we maintain separate counters.
	audioIdx, subIdx := 0, 0
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if p.VideoCodec == "" {
				p.VideoCodec = s.CodecName
				p.Width, p.Height = s.Width, s.Height
				p.ColorTransfer = s.ColorTransfer
				p.ColorPrimaries = s.ColorPrimaries
				p.ColorSpace = s.ColorSpace
			}
		case "audio":
			if p.AudioCodec == "" {
				p.AudioCodec = s.CodecName
			}
			p.AudioTracks = append(p.AudioTracks, TrackInfo{
				Index:    audioIdx,
				Codec:    s.CodecName,
				Language: normalizeLang(s.Tags.Language),
				Title:    s.Tags.Title,
				Default:  s.Disposition.Default == 1,
				Forced:   s.Disposition.Forced == 1,
				Channels: s.Channels,
			})
			audioIdx++
		case "subtitle":
			p.SubtitleTracks = append(p.SubtitleTracks, TrackInfo{
				Index:    subIdx,
				Codec:    s.CodecName,
				Language: normalizeLang(s.Tags.Language),
				Title:    s.Tags.Title,
				Default:  s.Disposition.Default == 1,
				Forced:   s.Disposition.Forced == 1,
			})
			subIdx++
		}
	}
	// Duration in milliseconds; ffprobe emits seconds with decimals.
	if raw.Format.Duration != "" {
		var secs float64
		_, _ = fmt.Sscanf(raw.Format.Duration, "%f", &secs)
		p.DurationMs = int64(secs * 1000)
	}
	return p, nil
}

// normalizeLang folds the various MKV/MP4 language conventions into a
// consistent lower-case ISO-639 code so the client can pattern-match.
// Empty / missing values become "und" (undetermined), matching what
// most muxers emit when the source file has no tag.
func normalizeLang(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "und"
	}
	// Common 2-letter → 3-letter mappings the browser typically sends
	// back the short form; ffprobe usually emits the long form. Map
	// both into the long form so they compare equal.
	switch s {
	case "en":
		return "eng"
	case "de":
		return "deu"
	case "fr":
		return "fra"
	case "es":
		return "spa"
	case "it":
		return "ita"
	case "ja":
		return "jpn"
	case "zh":
		return "zho"
	case "pt":
		return "por"
	case "ru":
		return "rus"
	case "nl":
		return "nld"
	}
	return s
}
