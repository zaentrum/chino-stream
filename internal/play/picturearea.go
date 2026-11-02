package play

import (
	"math"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// Where the picture is in a frame with black bars. Many sources keep a full
// 16:9 frame with a wider picture letterboxed inside it, the bars part of the
// video (a 1920x1080 frame whose picture is 1920x804 at y=138), or a narrower
// one pillarboxed; on a screen wider than 16:9 a player that fits the whole
// frame shows the picture in black on all four sides. The analyzer measures
// the picture's area once - the union of the picture over the whole title -
// katalog-manager keeps it, and katalog-api answers it with the playback
// lookups (catalog.PictureArea, in pixels of the frame it measured).
// /play/info passes it on as pictureArea, fractions of the coded frame the
// client is served, and the players fit that rectangle to the screen.

// pictureArea is /play/info's pictureArea: the rectangle of the coded frame
// the picture fills, each value a fraction (0-1) of the frame's width or
// height.
type pictureArea struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// pictureShapeApart is the most the aspect ratios of the frame measured and
// the frame served may be apart, the wider over the narrower, for the area
// to be said of the frame served: 1%, as an 854x480 rung's is from a
// 1920x1080 frame's (0.08%).
const pictureShapeApart = 1.01

// infoPictureArea is /play/info's pictureArea of katalog-api's picture area a
// for a client served a frame of width x height: each edge of a's rectangle a
// fraction of the frame it was measured in, rounded to four decimals, and its
// width and height the distances between the edges, rounded again to drop
// the float noise (1920x804 at y=138 of 1920x1080 is left 0, top 0.1278,
// width 1, height 0.7444). A fraction holds for every size of a frame - a
// 3840x2160 measurement for a 1920x1080 rung - but not for a frame of
// another shape, such as a package whose frame the transcoder cropped to the
// picture. nil, and no pictureArea, when a is nil, no rectangle inside its
// frame or the whole frame, when the frame served has no size, or when its
// aspect ratio and the frame measured's are more than 1% apart: a crop that
// cuts the picture is far worse than a frame shown whole.
func infoPictureArea(a *catalog.PictureArea, width, height int) *pictureArea {
	if a == nil || width <= 0 || height <= 0 || a.FrameWidth <= 0 || a.FrameHeight <= 0 ||
		a.X < 0 || a.Y < 0 || a.Width <= 0 || a.Height <= 0 || a.X+a.Width > a.FrameWidth || a.Y+a.Height > a.FrameHeight {
		return nil
	}
	fw, fh := float64(a.FrameWidth), float64(a.FrameHeight)
	if shape := fw / fh / (float64(width) / float64(height)); shape > pictureShapeApart || shape < 1/pictureShapeApart {
		return nil
	}
	left, top := round4(float64(a.X)/fw), round4(float64(a.Y)/fh)
	right, bottom := round4(float64(a.X+a.Width)/fw), round4(float64(a.Y+a.Height)/fh)
	pa := &pictureArea{Left: left, Top: top, Width: round4(right - left), Height: round4(bottom - top)}
	if pa.Width <= 0 || pa.Height <= 0 || *pa == (pictureArea{Width: 1, Height: 1}) {
		return nil
	}
	return pa
}

// round4 is v rounded to four decimals.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }
