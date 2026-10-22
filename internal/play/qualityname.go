package play

import (
	"strconv"
	"strings"
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
// A height between two steps takes the lower step's word with its own height
// ("Medium (900p)"), and so do the heights past either end of the table
// ("Ultra (4320p)", "Lowest (240p)"). The original played as it is - the
// stream copy, passthrough - is "Original". A re-encode at the original's
// size is named by its height like any other step. The apps show a name as
// it is given; the step-down notices say it.

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

// namedHeight is the height a w×h frame is named by: that of the 16:9 frame
// it fills, as the ladder names its rungs ("720p" is the 1280x720 box). It is
// the frame's own height, unless the frame is wider than 16:9: then the
// height of the 16:9 frame as wide, so a 2.39:1 film's 1920x800 is 1080p and
// its 3840x1606 2160p, as a 16:9 film's of that width are, and a 1.85:1
// film's 1920x1038 1080p. A frame up to 10% wider than that counts as the
// 16:9 frame of a standard height (sizeClasses) it is no taller than - DCI
// 4096x2160 is 2160p, 2048x858 and 1998x1080 1080p - else as the 16:9 frame
// of its own height (1666x900 is 900p). A width of 0 (not known) leaves the
// height; 0 without a height.
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
	if full*10 <= h*11 {
		return h
	}
	return full
}

// onTheFlyName is the name of rung ql of the on-the-fly ladder of source p,
// for a device whose HW decoder tops out at maxHeight (0: no cap): by the
// frame the rung encodes from p (transcodeGeometry), which ffmpeg and the
// live worker both encode, and with p's size not known by the height the
// live worker then encodes (rungHeight).
//
// A rung scaled to a height (medium, low: scale=-2:720 under the cap) is
// named by that height, the lines it produces, whatever the source's shape:
// a 2.39:1 film's 1728x720 medium is "Medium (720p)" as a 16:9 film's
// 1280x720 is, not the 16:9 frame as wide (972p). One the source is shorter
// than encodes the source's own frame, high's, and is named as that is
// (a 1280x536 source's medium is "Medium (720p)", as its high is).
func onTheFlyName(p *Probe, ql Quality, maxHeight int) string {
	var w, h int
	if p != nil {
		w, h = p.Width, p.Height
	}
	g := transcodeGeometry(ql, w, h, maxHeight)
	if t := rungHeight(ql, maxHeight); g.scaleH > 0 && g.h >= t {
		return qualityName(0, t, false)
	}
	if g.h <= 0 {
		g.w, g.h = 0, rungHeight(ql, maxHeight)
	}
	return qualityName(g.w, g.h, false)
}

// onTheFlyRungs are the on-the-fly ladder's rungs, the best first: the q
// values a client asks for them with.
var onTheFlyRungs = []string{"high", "medium", "low"}

// onTheFlyQualities is the quality choice /play/info offers for the
// on-the-fly transcode of source p on a device capped at maxHeight: the
// ladder's rungs, the best first, each {"name": its q value, "label":
// onTheFlyName}. A rung that would be named what a rung above it is - the
// same frame, as a 720p source's high and medium are - is left out: one entry
// per name, the better encode's.
func onTheFlyQualities(p *Probe, maxHeight int) []map[string]string {
	var out []map[string]string
	seen := map[string]bool{}
	for _, q := range onTheFlyRungs {
		label := onTheFlyName(p, QualityLadder[q], maxHeight)
		if seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, map[string]string{"name": q, "label": label})
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

// nameVariants is a packaged master with each variant (EXT-X-STREAM-INF)
// named by the frame of its rung (RESOLUTION): NAME set, or added at the end
// of the line. A variant whose RESOLUTION is not known stays as it is.
// Nothing else changes: the I-frame playlists are no quality to pick.
func nameVariants(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		w, h := parseResolution(hlsAttributes(line)["RESOLUTION"])
		if name := qualityName(w, h, false); name != "" {
			lines[i] = setQuotedHLSAttribute(line, "NAME", name)
		}
	}
	return strings.Join(lines, "\n")
}
