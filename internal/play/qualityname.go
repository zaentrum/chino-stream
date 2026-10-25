package play

import (
	"path"
	"strconv"
	"strings"

	"github.com/zaentrum/chino-stream/internal/pkgmanifest"
)

// What a quality step is called: the one name a client is given for it, as
// the label of its entry in /play/info's qualities and as the NAME of its
// variant in a master, on every path - a package's rungs, the on-the-fly
// ladder on ffmpeg or on the live worker, the stream copy. qualityName is the
// only source of these names. One fixed table, by height, so that a name
// means the same height on every title and every installation (the product
// spec: docs/chino, playback.md, "Quality names"):
//
//	2160p and up   Ultra (2160p)
//	1080p          High (1080p)
//	720p           Medium (720p)
//	480p           Low (480p)
//	below 480p     Lowest (360p)
//
// A frame's height for naming is its step (namedHeight): its own height, or
// the height its width stands for at 16:9, whichever is larger - a scope
// film's 1920x818 is 1080p, as a 16:9 film's 1920x1080 is. A step between
// two heights of the table takes the lower one's word with its own number
// (1600x900 is "Medium (900p)", 1666x900 "Medium (937p)"), and so do the
// steps past either end of the table ("Ultra (4320p)", "Lowest (240p)").
// The original played as it is - the stream copy, passthrough, and a
// package's rung its record says is the original's own video stream copied
// into it (originalRungs) - is "Original". A re-encode at the original's
// size is named by its step like any other. The apps show a name as it is
// given; the step-down notices say it.

// qualitySteps are the table's steps, the tallest first: a height takes the
// word of the first step it reaches.
var qualitySteps = []struct {
	height int
	word   string
}{
	{2160, "Ultra"},
	{1080, "High"},
	{720, "Medium"},
	{480, "Low"},
	{0, "Lowest"},
}

// originalQuality is the name of the original played as it is.
const originalQuality = "Original"

// qualityName is the name of a quality step: "Original" for the original
// played as it is (original), else the table's word for the height a w×h
// frame is named by (namedHeight), with that height: "High (1080p)". "" when
// the frame's height is not known - no name rather than a wrong one.
func qualityName(w, h int, original bool) string {
	if original {
		return originalQuality
	}
	p := namedHeight(w, h)
	if p <= 0 {
		return ""
	}
	word := qualitySteps[len(qualitySteps)-1].word
	for _, s := range qualitySteps {
		if p >= s.height {
			word = s.word
			break
		}
	}
	return word + " (" + strconv.Itoa(p) + "p)"
}

// sizeClasses are the standard picture heights: the transcoder's ladder
// rungs and the boxes players and sources come in.
var sizeClasses = []int{240, 360, 480, 540, 576, 720, 1080, 1440, 2160, 4320}

// namedHeight is the height a w×h frame is named by, its step: its own
// height, or the height its width stands for at 16:9, whichever is larger -
// the 16:9 frame it fills, as the ladder names its rungs ("720p" is the
// 1280x720 box). So a frame wider than 16:9 is named by the 16:9 frame as
// wide: a 2.39:1 film's 1920x800 is 1080p and its 3840x1606 2160p, as a
// 16:9 film's of that width are, and a 1.85:1 film's 1920x1038 1080p. A
// frame wider than 16:9 a little wider or a little narrower than a standard
// 16:9 frame (sizeClasses) is that frame: up to 10% wider than the 16:9
// frame of a standard height it is no taller than - DCI 4096x2160 is 2160p,
// 2048x858 and 1998x1080 1080p - or up to 3% narrower, a standard frame
// cropped at the sides (1916x800 and 1912x1036 are 1080p, 3836x1600 2160p,
// 1278x534 720p). Any other frame wider than 16:9 is named by the height its
// width stands for at 16:9, not by its own: 1666x900 is 937p, 1856x1036
// 1044p. A width of 0 (not known) leaves the height; 0 without a height.
func namedHeight(w, h int) int {
	if h <= 0 {
		return 0
	}
	full := h
	if w > 0 {
		full = max(h, (w*9+8)/16) // the 16:9 frame w wide, to the nearest pixel
	}
	if full == h {
		return h
	}
	for i := len(sizeClasses) - 1; i >= 0; i-- {
		if c := sizeClasses[i]; c >= h && c <= full && full*10 <= c*11 {
			return c
		}
	}
	for _, c := range sizeClasses { // a little narrower than a standard frame
		if c > full && full*100 >= c*97 {
			return c
		}
	}
	return full
}

// onTheFlyName is the name of rung ql of the on-the-fly ladder of source p,
// for a device whose HW decoder tops out at maxHeight (0: no cap), on the
// live worker (live) or ffmpeg: qualityName of the frame the rung encodes
// from p (transcodeGeometry), which ffmpeg and the live worker both encode -
// by its step, as a package's rung of that frame is named. A 2.39:1 film's
// 1728x720 medium is "Medium (972p)" and its 1152x480 low "Low (648p)", a
// 16:9 film's 1280x720 medium "Medium (720p)". Two rungs that encode one
// frame carry one name: a 1728x720 original's high and medium are both
// "Medium (972p)", a 1152x480 original's three rungs "Low (648p)". Under a
// HW cap below 1080 a rung wider than the 16:9 box of its height is fitted
// into it (a 2.39:1 film on a 720 cap: high and medium 1280x534 "Medium
// (720p)", low 854x356 "Low (480p)").
//
// With p's size not known the frame's width is not known either. The live
// worker, asked for a height with no width, fits the source into the 16:9
// box of that height, whose step is the height: a rung is named by the
// height it asks for (medium's 720, high's rungHeight, 1080 or the cap).
// ffmpeg keeps the source's shape at medium's and low's height
// (scale=-2:720, a scope film's 1728x720, "Medium (972p)") and encodes high
// at the source's own size, so the step of none of them is known: no name
// ("") rather than a wrong one.
func onTheFlyName(p *Probe, ql Quality, maxHeight int, live bool) string {
	var w, h int
	if p != nil {
		w, h = p.Width, p.Height
	}
	g := transcodeGeometry(ql, w, h, maxHeight)
	if g.w > 0 && g.h > 0 {
		return qualityName(g.w, g.h, false)
	}
	if !live {
		return ""
	}
	if g.h <= 0 {
		g.h = rungHeight(ql, maxHeight)
	}
	return qualityName(0, g.h, false)
}

// onTheFlyRungs are the on-the-fly ladder's rungs, the best first: the q
// values a client asks for them with.
var onTheFlyRungs = []string{"high", "medium", "low"}

// onTheFlyQualities is the quality choice /play/info offers for the
// on-the-fly transcode of source p on a device capped at maxHeight, on the
// live worker (live) or ffmpeg: every rung of the ladder, the best first,
// each {"name": its q value, "label": onTheFlyName}. Two rungs that encode
// one frame - a 720p source's high and medium - carry one name, and both are
// listed: the apps step down from high to medium and low by those q values
// and look a q's label up here, so a q left out would play with no name. A
// client is to merge them, one row per name, the first's (the better
// encode), and step down to the next entry with another name; the released
// apps do not yet and show such a name twice (README, "Quality names").
func onTheFlyQualities(p *Probe, maxHeight int, live bool) []map[string]string {
	out := make([]map[string]string, 0, len(onTheFlyRungs))
	for _, q := range onTheFlyRungs {
		out = append(out, map[string]string{"name": q, "label": onTheFlyName(p, QualityLadder[q], maxHeight, live)})
	}
	return out
}

// nameAttr is the NAME attribute of a variant called name, for the end of
// its EXT-X-STREAM-INF line; nothing for no name.
func nameAttr(name string) string {
	if name == "" {
		return ""
	}
	return ",NAME=" + strconv.Quote(name)
}

// originalRungs are the rungs of the package mf that are the original played
// as it is - its own video stream, copied into the package without a
// re-encode - by the rung id the master's variant URIs name them by ("v0",
// the folder hls/v0): those whose record says what made them was "copy".
// The packager writes that record, encoder, for every video rendition of a
// package since renditions.json - "copy" for the original's stream (the
// transcoder's copy rung, or the original packaged with no handoff at all),
// the encoder's name for an encode - in manifest.json and package.json
// alike (pkgmanifest.VideoRendition.Encoder). A rung with another encoder is
// a re-encode and keeps its table name, whatever its size or codec. nil when
// mf is nil or records no copy: a package from before renditions.json has no
// record of either, and its rungs keep their table names - no guess from the
// codec, size or bit rate.
func originalRungs(mf *pkgmanifest.Manifest) map[string]bool {
	if mf == nil {
		return nil
	}
	var out map[string]bool
	for _, v := range mf.Renditions.Video {
		if v.Encoder != "copy" {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[videoRenditionID(v)] = true
	}
	return out
}

// videoRenditionID is a video rendition's id in its master - the folder its
// variants' URIs name, "v0" for hls/v0 - the manifest's id when it names no
// folder.
func videoRenditionID(v pkgmanifest.VideoRendition) string {
	if v.Dir != "" {
		return path.Base(v.Dir)
	}
	return v.ID
}

// nameVariants is a packaged master with each variant (EXT-X-STREAM-INF)
// named: "Original" for a variant of a rung in originals (originalRungs, by
// the first path segment of its URI), else by the frame of its rung
// (RESOLUTION); NAME set, or added at the end of the line. A variant of
// another rung whose RESOLUTION is not known stays as it is. Nothing else
// changes: the I-frame playlists are no quality to pick.
func nameVariants(body string, originals map[string]bool) string {
	m := parseMaster(body)
	for _, v := range m.variants {
		if name := qualityName(v.width, v.height, originals[v.rung]); name != "" {
			m.lines[v.inf] = setQuotedHLSAttribute(m.lines[v.inf], "NAME", name)
		}
	}
	return strings.Join(m.lines, "\n")
}
