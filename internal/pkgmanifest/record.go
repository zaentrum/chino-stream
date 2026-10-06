package pkgmanifest

import (
	"encoding/json"
	"fmt"
	"time"
)

// RecordSchema is the schema of the record a library version's or extra's
// package is read by: versions/<versionId>/package.json and
// extras/<extraId>/package.json, written once by the packager when the
// package completes (zaentrum's library record, contract platform-library/1).
const RecordSchema = "zaentrum.library.package/2"

// packageRecord is a package.json as far as serving reads it: the v1
// manifest's playback fields under their record names, plus the additive
// fields the packager writes for the master it assembles (S1: the surround
// group, rendition groups and names, the video's peak, range, label and
// encoder, the subtitles' names, HLS dirs and sidecar origin, the hls
// block). The record's fidelity, essence and checksums say nothing a stream
// service serves by and are not read.
type packageRecord struct {
	Schema     string    `json:"schema"`
	PackageID  string    `json:"packageId"`
	CreatedAt  time.Time `json:"createdAt"`
	PackagedBy string    `json:"packagedBy"`
	DurationMs int64     `json:"durationMs"`
	Renditions struct {
		Video         []recordVideo    `json:"video"`
		Audio         []AudioRendition `json:"audio"`
		AudioSurround []AudioRendition `json:"audioSurround"`
	} `json:"renditions"`
	Subtitles []recordSubtitle `json:"subtitles"`
	Trickplay *Trickplay       `json:"trickplay"`
	Trailers  []Trailer        `json:"trailers"`
	HLS       *HLS             `json:"hls"`
}

// recordVideo is a video rendition of the record: the manifest's, and the
// dynamic range the record names in its own terms.
type recordVideo struct {
	VideoRendition
	DynamicRange string `json:"dynamicRange"`
}

// recordSubtitle is a subtitle rendition of the record. FromSidecar names
// the copy of the subtitle file next to the original it was made from (a
// path from the item folder); the manifest marked such a track external.
type recordSubtitle struct {
	Subtitle
	FromSidecar string `json:"fromSidecar"`
}

// FromPackageRecord reads a package.json (RecordSchema) as the Manifest a
// package from before the library is read by, so everything that serves a
// package serves both alike: createdAt is PackagedAt, packagedBy Packager,
// the renditions (the surround group included), subtitles, trickplay and the
// hls block are the manifest's. A video rendition without videoRange takes
// it from the record's dynamicRange; a subtitle made from a sidecar file is
// External, as the manifest marked one.
//
// The record names no item: its folder does (versions/<id>/ of an item's,
// extras/<id>/ of an extra's), and the item's own records hold its title.
// ItemID, Type, Title and the rest of the catalog identity are left for the
// caller. A record of another schema is an error: a reader never guesses.
func FromPackageRecord(raw []byte) (*Manifest, error) {
	var r packageRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Schema != RecordSchema {
		return nil, fmt.Errorf("package record of schema %q, want %q", r.Schema, RecordSchema)
	}
	m := &Manifest{
		PackagedAt: r.CreatedAt,
		Packager:   r.PackagedBy,
		DurationMs: r.DurationMs,
		Trickplay:  r.Trickplay,
		Trailers:   r.Trailers,
		HLS:        r.HLS,
	}
	m.Renditions.Video = make([]VideoRendition, 0, len(r.Renditions.Video))
	for _, v := range r.Renditions.Video {
		if v.VideoRange == "" {
			v.VideoRange = videoRangeOf(v.DynamicRange)
		}
		m.Renditions.Video = append(m.Renditions.Video, v.VideoRendition)
	}
	m.Renditions.Audio = r.Renditions.Audio
	if m.Renditions.Audio == nil {
		m.Renditions.Audio = []AudioRendition{}
	}
	m.Renditions.AudioSurround = r.Renditions.AudioSurround
	for _, s := range r.Subtitles {
		if s.FromSidecar != "" {
			s.External = true
		}
		m.Subtitles = append(m.Subtitles, s.Subtitle)
	}
	return m, nil
}

// videoRangeOf is the HLS VIDEO-RANGE of a record's dynamicRange: PQ for the
// HDR10 family and Dolby Vision (whose base layer is PQ), HLG, SDR; "" when
// the record does not say.
func videoRangeOf(dynamicRange string) string {
	switch dynamicRange {
	case "sdr":
		return "SDR"
	case "hdr10", "hdr10plus", "dolby-vision":
		return "PQ"
	case "hlg":
		return "HLG"
	}
	return ""
}
