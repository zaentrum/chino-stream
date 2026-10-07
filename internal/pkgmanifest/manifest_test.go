package pkgmanifest

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A ladder package's manifest, as the packager writes it with LADDER,
// SURROUND_AUDIO and HLS_SUBTITLES on (cut to one entry per list).
const ladderManifest = `{
  "version": 2,
  "itemId": "1adde700-0000-4000-8000-000000000001",
  "durationMs": 20021,
  "renditions": {
    "video": [
      {"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080,
       "bitrateBps": 7187526, "peakBitrateBps": 7243845, "hdr": false, "videoRange": "SDR",
       "frameRate": "24000/1001", "segments": 4, "targetDuration": 7, "label": "source",
       "encoder": "copy"}
    ],
    "audio": [
      {"idx": 0, "codec": "mp4a.40.2", "language": "eng", "title": "", "channels": 2,
       "default": true, "visible": true, "id": "a0", "dir": "hls/a0", "bitrateBps": 192439,
       "segments": 4, "group": "audio", "name": "English"}
    ],
    "audioSurround": [
      {"idx": 0, "codec": "ec-3", "language": "eng", "title": "", "channels": 6,
       "default": true, "visible": true, "mode": "encode", "id": "a2", "dir": "hls/a2",
       "bitrateBps": 448982, "segments": 4, "group": "audio-surround", "name": "English 5.1"}
    ]
  },
  "subtitles": [
    {"id": "sub1", "language": "eng", "title": "Forced", "default": false, "forced": true,
     "visible": true, "path": "subs/1.vtt", "format": "webvtt", "hls": "hls/s1"}
  ],
  "hls": {"master": "hls/master.m3u8", "segmentSeconds": 6,
          "audioGroups": ["audio", "audio-surround"], "subtitleGroup": "subs"}
}`

func TestLadderManifestFields(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(ladderManifest), &m); err != nil {
		t.Fatal(err)
	}
	v := m.Renditions.Video[0]
	if v.PeakBitrateBps != 7243845 || v.VideoRange != "SDR" || v.Label != "source" || v.Encoder != "copy" {
		t.Errorf("video rendition: %+v", v)
	}
	if a := m.Renditions.Audio[0]; a.Group != "audio" || a.Name != "English" {
		t.Errorf("stereo rendition: %+v", a)
	}
	want := AudioRendition{ID: "a2", Dir: "hls/a2", Codec: "ec-3", Language: "eng", Default: true,
		Channels: 6, BitrateBps: 448982, Segments: 4, Group: "audio-surround", Name: "English 5.1", Idx: intp(0)}
	if len(m.Renditions.AudioSurround) != 1 || !reflect.DeepEqual(m.Renditions.AudioSurround[0], want) {
		t.Errorf("surround renditions: %+v", m.Renditions.AudioSurround)
	}
	if s := m.Subtitles[0]; s.HLS != "hls/s1" || !s.Forced {
		t.Errorf("subtitle: %+v", s)
	}
	wantHLS := &HLS{Master: "hls/master.m3u8", SegmentSeconds: 6,
		AudioGroups: []string{"audio", "audio-surround"}, SubtitleGroup: "subs"}
	if !reflect.DeepEqual(m.HLS, wantHLS) {
		t.Errorf("hls: %+v", m.HLS)
	}
}

// An extra's manifest is an item's, plus the title it belongs to and its
// kind, without trickplay; an item's has neither.
func TestExtraManifestFields(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(`{"version":2,"itemId":"7a11e700-0000-4000-8000-000000000009","type":"extra",
		"parentId":"57e2e0a0-0000-4000-8000-000000000008","extraKind":"trailer","title":"Trailer",
		"year":null,"tmdbId":null,"durationMs":20021,
		"renditions":{"video":[{"id":"v0","dir":"hls/v0","codec":"avc1.64001f","width":1280,"height":720}],
		              "audio":[{"id":"a0","dir":"hls/a0","codec":"mp4a.40.2","language":"eng","default":true,"channels":2}],
		              "audioSurround":[]},
		"subtitles":[],"hls":{"master":"hls/master.m3u8","segmentSeconds":6,"audioGroups":["audio"],"subtitleGroup":null}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "extra" || m.ParentID != "57e2e0a0-0000-4000-8000-000000000008" || m.ExtraKind != "trailer" ||
		m.Trickplay != nil || m.EffectiveDurationMs() != 20021 {
		t.Errorf("%+v", m)
	}
	var item Manifest
	if err := json.Unmarshal([]byte(ladderManifest), &item); err != nil {
		t.Fatal(err)
	}
	if item.ParentID != "" || item.ExtraKind != "" {
		t.Errorf("an item's manifest: parentId %q, extraKind %q", item.ParentID, item.ExtraKind)
	}
}

// The packager writes "subtitleGroup": null when the master references no
// subtitles, and a package from before renditions.json has no hls block.
func TestManifestWithoutLadderFields(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(`{"version":2,"renditions":{"video":[{"id":"v0"}],"audio":[]},
		"hls":{"master":"hls/master.m3u8","segmentSeconds":6,"audioGroups":["audio"],"subtitleGroup":null}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.HLS == nil || m.HLS.SubtitleGroup != "" || len(m.Renditions.AudioSurround) != 0 {
		t.Errorf("%+v %+v", m.HLS, m.Renditions)
	}
	var old Manifest
	if err := json.Unmarshal([]byte(`{"version":2,"renditions":{"video":[{"id":"v0","codec":"hev1.1.6.L120.B0"}],"audio":[]}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.HLS != nil || old.Renditions.Video[0].PeakBitrateBps != 0 {
		t.Errorf("%+v", old)
	}
}

func intp(n int) *int { return &n }

// A rendition names the source track it was made from: a manifest by the
// packager's audio ordinal (idx), a record by the original's stream index
// (sourceStreamIndex); a 5.1 companion names its stereo rendition's. A
// rendition naming neither says nothing.
func TestAudioRenditionSourceTrack(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(ladderManifest), &m); err != nil {
		t.Fatal(err)
	}
	stereo, _ := m.Renditions.Audio[0].SourceTrack()
	if n, ok := m.Renditions.AudioSurround[0].SourceTrack(); !ok || n != stereo || n != 0 {
		t.Errorf("the companion's source track %d %v, its stereo rendition's %d", n, ok, stereo)
	}
	if n, ok := (AudioRendition{SourceStreamIndex: intp(3), Idx: intp(1)}).SourceTrack(); !ok || n != 3 {
		t.Errorf("a record's stream index: %d %v", n, ok)
	}
	if _, ok := (AudioRendition{ID: "a0"}).SourceTrack(); ok {
		t.Error("a rendition naming no source track says one")
	}
}
