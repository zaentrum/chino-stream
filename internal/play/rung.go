package play

import (
	"fmt"
	"strconv"
)

// What a transcode rung produces from a given source, computed once and used
// by both the encoder (videoEncoderArgs) and the master playlist (Master).
// The master used to advertise a fixed 1920x1080 @ 6.5 Mbps for every source
// and rung — a 720p title, a 480p medium rung, a capped device all claimed
// 1080p — and the medium/low rungs upscaled sources smaller than the rung.

// rungGeometry is the frame a transcode rung encodes from a source.
type rungGeometry struct {
	// scaleH > 0: scale to this height, width aspect-kept and even
	// (ffmpeg's "-2").
	scaleH int
	// fitW, fitH > 0: scale to exactly this size (the source fitted into
	// the level/device box).
	fitW, fitH int
	// w, h is the frame size produced; 0 when it can't be known (the
	// source size is unknown).
	w, h int
}

// transcodeGeometry is the frame the rung ql encodes from a srcW×srcH source
// (0×0 = unknown) for a device whose HW decoder tops out at maxHeight (0 =
// no cap). It never upscales.
//
// Frames larger than 1080p — 4K, 1440p, ultrawide 21:9 — must be downscaled
// into a 1920×1080 box or the encoder rejects them with "Invalid Level"
// (-level 4.0). On the explicit ladder rungs (medium/low) ql.Scale targets a
// fixed height (720/480); the "high" rung's empty Scale means source size,
// which only works up to 1080p.
//
// maxHeight is the device HW decoder ceiling threaded from the request's
// ?caps=. The fit box height is the SMALLER of the 1080 level limit and
// maxHeight, so a device that can only HW-decode 1080 never gets a taller
// H.264 frame than it can play — otherwise the source-resolution "high" rung
// re-serves the same 4K frame the device couldn't HW-decode in the first
// place (the reason it fell through from packaged to transcode). The width
// companion (16:9 of the box height) keeps us inside Level 4.0's pixel
// budget. A rung's own height is tightened to the ceiling the same way.
func transcodeGeometry(ql Quality, srcW, srcH, maxHeight int) rungGeometry {
	if rungH := scaleTargetHeight(ql.Scale); rungH > 0 {
		if maxHeight > 0 && rungH > maxHeight {
			rungH = maxHeight
		}
		if srcH > 0 && rungH > srcH {
			// No upscaling: a 360p source on the 720p rung stays 360p
			// (made even — h264 requires even dimensions).
			rungH = srcH &^ 1
		}
		g := rungGeometry{scaleH: rungH, h: rungH}
		if srcW > 0 && srcH > 0 {
			g.w = evenScaledWidth(srcW, srcH, rungH)
		}
		return g
	}
	capH := 1080
	if maxHeight > 0 && maxHeight < capH {
		capH = maxHeight
	}
	capW := capH * 16 / 9 // 16:9 companion of the box height
	if srcW <= 0 || srcH <= 0 || (srcW <= capW && srcH <= capH) {
		return rungGeometry{w: srcW, h: srcH} // source size, as it comes
	}
	// Aspect-preserving "fit within capW×capH":
	//   - width-limited (srcW/srcH > 16/9): w=capW, h=srcH*capW/srcW
	//   - height-limited:                   h=capH, w=srcW*capH/srcH
	// Round both to even pixels — h264 requires even dims, scale_cuda
	// errors on odd numbers.
	g := rungGeometry{}
	if srcW*capH > srcH*capW {
		// Width-limited (e.g. 21:9 ultrawide). 3840×1606 → capW×….
		g.fitW = capW
		g.fitH = (srcH*capW/srcW + 1) &^ 1
	} else {
		g.fitH = capH
		g.fitW = (srcW*capH/srcH + 1) &^ 1
	}
	g.w, g.h = g.fitW, g.fitH
	return g
}

// evenScaledWidth is the width ffmpeg's scale=-2:h gives a srcW×srcH frame:
// the aspect-kept width rounded to the nearest multiple of 2 (av_rescale,
// halves away from zero).
func evenScaledWidth(srcW, srcH, h int) int {
	return (h*srcW + srcH) / (2 * srcH) * 2
}

// nominalRungs is each rung's video bitrate at its nominal frame size — the
// ladder's long-standing bandwidth hints. A rung's advertised bitrate is this
// scaled by the frame area the source really gets.
var nominalRungs = map[string]struct{ bps, w, h int }{
	"high":   {6_500_000, 1920, 1080},
	"medium": {3_000_000, 1280, 720},
	"low":    {1_400_000, 854, 480},
}

// transcodeBandwidth is the BANDWIDTH of a transcode variant that encodes
// w×h on rung: the rung's nominal video bitrate scaled by frame area (the
// nominal bitrate when the size is unknown), plus the audio rendition the
// variant plays with, if any.
func transcodeBandwidth(rung string, w, h int, withAudio bool) int {
	n, ok := nominalRungs[rung]
	if !ok {
		n = nominalRungs["high"]
	}
	bps := n.bps
	if w > 0 && h > 0 {
		bps = int(int64(n.bps) * int64(w) * int64(h) / int64(n.w*n.h))
	}
	if withAudio {
		bps += windowAudioBitrateBps
	}
	return bps
}

// copyBandwidth is the BANDWIDTH of the stream-copy variant: the source's
// own bitrate (the bytes are the source's), the high rung's nominal one when
// ffprobe could not tell.
func copyBandwidth(p *Probe) int {
	if p != nil && p.BitRate > 0 {
		return int(p.BitRate)
	}
	return nominalRungs["high"].bps
}

// resolutionAttr is the RESOLUTION attribute for a w×h variant, or nothing
// when the size is unknown — better absent than wrong.
func resolutionAttr(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	return ",RESOLUTION=" + strconv.Itoa(w) + "x" + strconv.Itoa(h)
}

// fallbackStreamInf is the #EXT-X-STREAM-INF line of the on-the-fly master's
// single variant: the stream copy (useCopy) at the source's size and
// bitrate, or rung ql of the transcode ladder at the size it really encodes
// (maxHeight is the device cap from ?caps=). audioGroup names the
// EXT-X-MEDIA audio group of a transcode variant ("" = none).
func fallbackStreamInf(p *Probe, ql Quality, maxHeight int, useCopy bool, audioGroup string) string {
	if useCopy {
		// CODECS switches between avc1 (H.264 source) and hvc1 (HEVC
		// source); permissive profile/level values, since the actual
		// config comes from the source's avcC/hvcC verbatim and browsers
		// tolerate looser advertised levels than what they can decode.
		// Main, Level 4.0 covers 1080p remuxes and most BD HEVC; 4K Main10
		// sources (hvc1.2.4.L153.B0) still play because the hvcC is
		// authoritative — the master string is just gating.
		codecs := "avc1.640028"
		if p.VideoCodec == "hevc" || p.VideoCodec == "h265" {
			codecs = "hvc1.1.6.L120.B0"
		}
		// The audio is the source's, copied: declare its real codec.
		if a := copyAudioCodec(p); a != "" {
			codecs += "," + a
		}
		return fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d%s,CODECS=\"%s\",VIDEO-RANGE=%s",
			copyBandwidth(p), resolutionAttr(p.Width, p.Height), codecs, videoRange(p))
	}
	g := transcodeGeometry(ql, p.Width, p.Height, maxHeight)
	audioAttr := ""
	if audioGroup != "" {
		audioAttr = fmt.Sprintf(",AUDIO=%q", audioGroup)
	}
	return fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d%s,CODECS=\"avc1.640028,mp4a.40.2\",VIDEO-RANGE=SDR%s",
		transcodeBandwidth(ql.Name, g.w, g.h, audioGroup != ""), resolutionAttr(g.w, g.h), audioAttr)
}
