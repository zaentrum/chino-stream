// Package pkgmanifest is the on-disk schema for a packaged item under
// /var/lib/katalog/packages/{itemUUID}/manifest.json.
//
// The package is the source of truth for everything chino-stream
// needs to serve a pre-packaged item without ever spawning ffmpeg:
// the rendition inventory (codecs, languages, segment counts), the
// source-file fingerprint (so analyzer knows when to re-package), and
// pointers to optional adjuncts (trickplay, trailers).
//
// Wire format is JSON. Field names match the JSON exactly so the
// Python analyzer can produce these files without a translation layer.
// Backwards-compat policy: Version is incremented for any breaking
// change; readers reject anything with a Version they don't recognise
// rather than guessing.
package pkgmanifest

import "time"

// CurrentVersion is the schema version this package reads + writes.
// Bump on every breaking change.
//
// v2: drops the Source block (source files won't be retained long
// term), lifts the catalog identity onto the manifest itself so each
// package directory self-describes the item even if the catalog DB
// is lost (title, type, year, TMDB ID; for episodes: SeriesTitle,
// SeasonNumber, EpisodeNumber, EpisodeCode). DurationMs moves to
// the top level since the stream service needs it for HLS.
//
// v1 packages stay readable: Source is now optional, and the v2
// fields are also optional so a v1 manifest unmarshals cleanly with
// zero-values for what it didn't have.
const CurrentVersion = 2

// Manifest is the top-level shape of a per-item manifest.json. v2
// fields are at the top level; the Source block is retained as
// optional for backwards compatibility with v1 packages still on
// disk from before the rewrite.
type Manifest struct {
	Version    int        `json:"version"`
	ItemID     string     `json:"itemId"`
	PackagedAt time.Time  `json:"packagedAt"`
	Packager   string     `json:"packager"`

	// v2 catalog identity. All optional in v1 (zero-valued); always
	// populated in v2.
	Type       string `json:"type,omitempty"`
	Title      string `json:"title,omitempty"`
	Year       *int   `json:"year,omitempty"`
	TmdbID     string `json:"tmdbId,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	// Episode-only — empty/nil for movies.
	SeriesTitle   string `json:"seriesTitle,omitempty"`
	SeasonNumber  *int   `json:"seasonNumber,omitempty"`
	EpisodeNumber *int   `json:"episodeNumber,omitempty"`
	EpisodeCode   string `json:"episodeCode,omitempty"` // e.g. "S01E03"

	// Renditions + adjuncts are version-independent.
	Renditions Renditions `json:"renditions"`
	Subtitles  []Subtitle `json:"subtitles,omitempty"`
	Trickplay  *Trickplay `json:"trickplay,omitempty"`
	Trailers   []Trailer  `json:"trailers,omitempty"`

	// v1-only. Kept optional so old packages on disk still parse;
	// new code should read DurationMs / Title at the top level.
	Source *Source `json:"source,omitempty"`
}

// Source fingerprinted the input file in v1 manifests (path, mtime,
// size, durationMs). Retained only for backwards-compat reading;
// v2 packages omit this block.
type Source struct {
	Path       string    `json:"path,omitempty"`
	MTime      time.Time `json:"mtime,omitempty"`
	Size       int64     `json:"size,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`
}

// EffectiveDurationMs returns DurationMs, falling back to the v1
// Source.DurationMs when the top-level field is zero (i.e. reading
// an old package).
func (m Manifest) EffectiveDurationMs() int64 {
	if m.DurationMs != 0 {
		return m.DurationMs
	}
	if m.Source != nil {
		return m.Source.DurationMs
	}
	return 0
}

// Renditions enumerates the streamable tracks. Video and audio are
// kept separate so the player can switch audio language independently
// of video quality. Today we have one video rendition (HEVC
// passthrough); the slice shape leaves room to add fallback renditions
// per quality later without a schema change.
type Renditions struct {
	Video []VideoRendition `json:"video"`
	Audio []AudioRendition `json:"audio"`
}

// VideoRendition describes one packaged video track. Dir is the
// directory under the item root holding init.mp4 / playlist.m3u8 /
// seg-*.m4s for this rendition.
type VideoRendition struct {
	ID              string `json:"id"`              // e.g. "v0"
	Dir             string `json:"dir"`             // relative to item root, e.g. "hls/v0"
	Codec           string `json:"codec"`           // e.g. "hev1.1.6.L120.B0"
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	BitrateBps      int    `json:"bitrateBps"`
	HDR             bool   `json:"hdr"`
	FrameRate       string `json:"frameRate"`       // e.g. "24000/1001"
	Segments        int    `json:"segments"`
	TargetDuration  int    `json:"targetDuration"`  // seconds, for #EXT-X-TARGETDURATION
}

// AudioRendition describes one packaged audio track.
type AudioRendition struct {
	ID         string `json:"id"`         // e.g. "a0"
	Dir        string `json:"dir"`        // e.g. "hls/a0"
	Codec      string `json:"codec"`      // e.g. "mp4a.40.2"
	Language   string `json:"language"`   // ISO 639-2/3
	Title      string `json:"title,omitempty"`
	Default    bool   `json:"default"`
	Channels   int    `json:"channels"`
	BitrateBps int    `json:"bitrateBps"`
	Segments   int    `json:"segments"`
}

// Subtitle describes one extracted subtitle track. Format is always
// "webvtt" in v1 (extracted via ffmpeg -c:s webvtt at package time).
type Subtitle struct {
	ID       string `json:"id"`
	Path     string `json:"path"`     // relative, e.g. "subs/0.vtt"
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	Format   string `json:"format"`   // "webvtt"
}

// Trickplay holds scrub-preview thumbnail metadata. The player loads
// the VTT, which maps each timestamp to a rectangle inside one of the
// sprite sheets — the standard trickplay sprite-sheet convention.
type Trickplay struct {
	VTTPath       string `json:"vttPath"`       // relative, e.g. "trickplay/thumbnails.vtt"
	SpritePattern string `json:"spritePattern"` // relative, e.g. "trickplay/sprite-%04d.jpg"
	IntervalSec   int    `json:"intervalSec"`   // seconds between consecutive thumbnails
	ThumbWidth    int    `json:"thumbWidth"`
	ThumbHeight   int    `json:"thumbHeight"`
	GridCols      int    `json:"gridCols"`      // thumbnails per sprite-sheet row
	GridRows      int    `json:"gridRows"`      // rows per sprite sheet
}

// Trailer is one packaged trailer (typically TMDB-sourced YouTube
// trailer, downloaded once at package time). Each trailer has its own
// mini-manifest at ManifestPath so the player can play it via the
// same HLS pipeline as the main feature.
type Trailer struct {
	ID           string `json:"id"`
	Source       string `json:"source"`       // e.g. "tmdb:603692" or "local:/path/to/trailer.mp4"
	DurationMs   int64  `json:"durationMs"`
	ManifestPath string `json:"manifestPath"` // relative, e.g. "trailers/0/manifest.json"
}
