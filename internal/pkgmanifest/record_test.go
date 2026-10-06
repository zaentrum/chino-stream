package pkgmanifest

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ladderRecord is a version's package.json as the packager writes it with
// the ladder fields (S1): the ladder manifest's renditions, subtitles and
// hls block under the record's names, plus a subtitle made from a sidecar
// file, and the record's own fields a stream service does not read.
const ladderRecord = `{
  "schema": "zaentrum.library.package/2",
  "packageId": "55b9f6e7-5e52-5ffd-a732-271df876f9db",
  "createdAt": "2026-10-06T09:00:00.250Z",
  "packagedBy": "packager version v3.4.2-c819dea-release",
  "state": "complete",
  "role": "canonical",
  "durationMs": 20021,
  "renditions": {
    "video": [
      {"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080,
       "bitrateBps": 7187526, "peakBitrateBps": 7243845, "hdr": false, "videoRange": "SDR",
       "frameRate": "24000/1001", "segments": 4, "targetDuration": 7, "label": "source",
       "encoder": "copy", "dynamicRange": "sdr", "sourceStreamIndex": 0}
    ],
    "audio": [
      {"id": "a0", "dir": "hls/a0", "codec": "mp4a.40.2", "language": "eng", "title": "", "default": true,
       "channels": 2, "bitrateBps": 192439, "segments": 4, "visible": true, "sourceStreamIndex": 1,
       "sourceChannels": 6, "purpose": "main", "purposeFrom": "assumed", "variant": null, "original": true,
       "group": "audio", "name": "English"}
    ],
    "audioSurround": [
      {"id": "a1", "dir": "hls/a1", "codec": "ec-3", "language": "eng", "title": "", "default": true,
       "channels": 6, "bitrateBps": 448982, "segments": 4, "visible": true, "sourceStreamIndex": 1,
       "purpose": "main", "purposeFrom": "assumed", "group": "audio-surround", "name": "English 5.1"}
    ]
  },
  "subtitles": [
    {"id": "sub0", "path": "subs/0.vtt", "language": "eng", "title": "Forced", "default": false, "forced": true,
     "format": "webvtt", "visible": true, "sourceStreamIndex": 2, "purpose": "forced", "purposeFrom": "title",
     "name": "English · Forced", "hls": "hls/s0"},
    {"id": "sub1", "path": "subs/1.vtt", "language": "de", "default": false, "forced": false, "format": "webvtt",
     "visible": true, "purpose": "dialogue", "purposeFrom": "assumed", "name": "German", "hls": "hls/s1",
     "fromSidecar": "sources/0b6c0000-0000-4000-8000-000000000001/Film (2010).de.srt"}
  ],
  "trickplay": {"vttPath": "trickplay/thumbnails.vtt", "spritePattern": "trickplay/sprite-%04d.jpg",
                "intervalSec": 10, "thumbWidth": 320, "thumbHeight": 180, "gridCols": 10, "gridRows": 10},
  "hls": {"master": "hls/master.m3u8", "segmentSeconds": 6, "audioGroups": ["audio", "audio-surround"],
          "subtitleGroup": "subs"},
  "sizeBytes": 734000000,
  "peakBandwidthBps": 7693886,
  "fidelity": {"lossless": false, "losses": [{"kind": "audio-downmix", "detail": "6ch -> 2ch (a0)"}]},
  "essence": {"maxAudioChannels": 6, "surround": true},
  "checksums": {"file": "checksums.sha256", "algorithm": "sha256",
                "sha256": "sha256:23a13274ad8def7ca011f069738b6bfd8f6b32c6a9c517b24f877448eba93aa1", "files": 6, "bytes": 6609}
}`

// A package.json reads as the manifest of the same package: the packaging
// time and packager under the manifest's names, every rendition (the
// surround group too) with the fields the master is served by, the
// subtitles with their names and HLS dirs — the one made from a sidecar
// file external, as the manifest marked it — trickplay and the hls block.
func TestFromPackageRecord(t *testing.T) {
	m, err := FromPackageRecord([]byte(ladderRecord))
	if err != nil {
		t.Fatal(err)
	}
	if !m.PackagedAt.Equal(time.Date(2026, 10, 6, 9, 0, 0, 250_000_000, time.UTC)) ||
		m.Packager != "packager version v3.4.2-c819dea-release" || m.EffectiveDurationMs() != 20021 {
		t.Errorf("packaged %v by %q, %d ms", m.PackagedAt, m.Packager, m.EffectiveDurationMs())
	}
	wantVideo := VideoRendition{ID: "v0", Dir: "hls/v0", Codec: "hvc1.1.6.L120.90", Width: 1920, Height: 1080,
		BitrateBps: 7187526, FrameRate: "24000/1001", Segments: 4, TargetDuration: 7,
		PeakBitrateBps: 7243845, VideoRange: "SDR", Label: "source", Encoder: "copy"}
	if !reflect.DeepEqual(m.Renditions.Video, []VideoRendition{wantVideo}) {
		t.Errorf("video\n got %+v\nwant %+v", m.Renditions.Video, wantVideo)
	}
	wantAudio := AudioRendition{ID: "a0", Dir: "hls/a0", Codec: "mp4a.40.2", Language: "eng", Default: true,
		Channels: 2, BitrateBps: 192439, Segments: 4, Group: "audio", Name: "English"}
	if !reflect.DeepEqual(m.Renditions.Audio, []AudioRendition{wantAudio}) {
		t.Errorf("audio\n got %+v\nwant %+v", m.Renditions.Audio, wantAudio)
	}
	if s := m.Renditions.AudioSurround; len(s) != 1 || s[0].ID != "a1" || s[0].Channels != 6 || s[0].Group != "audio-surround" || s[0].Name != "English 5.1" {
		t.Errorf("surround %+v", s)
	}
	wantSubs := []Subtitle{
		{ID: "sub0", Path: "subs/0.vtt", Language: "eng", Title: "Forced", Name: "English · Forced", Forced: true, Format: "webvtt", HLS: "hls/s0"},
		{ID: "sub1", Path: "subs/1.vtt", Language: "de", Name: "German", Format: "webvtt", HLS: "hls/s1", External: true},
	}
	if !reflect.DeepEqual(m.Subtitles, wantSubs) {
		t.Errorf("subtitles\n got %+v\nwant %+v", m.Subtitles, wantSubs)
	}
	if m.Trickplay == nil || m.Trickplay.VTTPath != "trickplay/thumbnails.vtt" || m.Trickplay.IntervalSec != 10 {
		t.Errorf("trickplay %+v", m.Trickplay)
	}
	wantHLS := &HLS{Master: "hls/master.m3u8", SegmentSeconds: 6, AudioGroups: []string{"audio", "audio-surround"}, SubtitleGroup: "subs"}
	if !reflect.DeepEqual(m.HLS, wantHLS) {
		t.Errorf("hls %+v", m.HLS)
	}
	// The record names no item; the caller fills the identity in.
	if m.ItemID != "" || m.Title != "" || m.Type != "" || m.Source != nil {
		t.Errorf("identity %q %q %q %+v", m.ItemID, m.Title, m.Type, m.Source)
	}
}

// A record from before the ladder fields (the schema's own example: no
// videoRange, no groups, no hls block, no subtitles) still reads: the video
// range from the record's dynamicRange, an HDR10 or Dolby Vision rendition
// PQ, HLG as HLG, no surround group, no hls block.
func TestFromPackageRecordWithoutTheLadderFields(t *testing.T) {
	rec := func(dynamicRange string) string {
		return `{"schema":"zaentrum.library.package/2","packageId":"55b9f6e7-5e52-5ffd-a732-271df876f9db",
		  "createdAt":"2026-09-18T11:30:00Z","packagedBy":"packager example","state":"complete","role":"derived",
		  "durationMs":734000,
		  "renditions":{"video":[{"id":"v0","dir":"hls/v0","codec":"hev1.1.6.L120.B0","width":1920,"height":800,
		    "bitrateBps":8000000,"hdr":true,"frameRate":"24/1","segments":10,"targetDuration":6,"dynamicRange":"` + dynamicRange + `"}],
		    "audio":[{"id":"a0","dir":"hls/a0","codec":"mp4a.40.2","language":"eng","title":"","default":true,"channels":2,
		      "bitrateBps":192000,"segments":15,"visible":true,"purpose":"main","purposeFrom":"assumed"}]},
		  "subtitles":[],"trickplay":null,"trailers":[],"sizeBytes":734000000,"peakBandwidthBps":8192000,
		  "fidelity":{"lossless":true,"losses":[]},"essence":{},"checksums":{}}`
	}
	for dr, want := range map[string]string{"hdr10": "PQ", "hdr10plus": "PQ", "dolby-vision": "PQ", "hlg": "HLG", "sdr": "SDR", "unknown": ""} {
		m, err := FromPackageRecord([]byte(rec(dr)))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Renditions.Video[0].VideoRange; got != want {
			t.Errorf("%s: video range %q, want %q", dr, got, want)
		}
		if !m.Renditions.Video[0].HDR || m.HLS != nil || m.Renditions.AudioSurround != nil || len(m.Subtitles) != 0 || m.Trickplay != nil ||
			m.EffectiveDurationMs() != 734000 || len(m.Renditions.Audio) != 1 {
			t.Errorf("%s: %+v", dr, m)
		}
	}
}

// What is not a package record is an error, never a guess: another schema
// (a v1 manifest, a version record), a record without one, broken JSON.
func TestFromPackageRecordRefusesWhatIsNoPackageRecord(t *testing.T) {
	for name, raw := range map[string]string{
		"a manifest":       `{"version":2,"itemId":"1adde700-0000-4000-8000-000000000001","renditions":{"video":[],"audio":[]}}`,
		"a version record": `{"schema":"zaentrum.library.version/2","versionId":"x"}`,
		"a later schema":   `{"schema":"zaentrum.library.package/3","renditions":{"video":[],"audio":[]}}`,
		"broken":           `{"schema":"zaentrum.library.package/2",`,
	} {
		if m, err := FromPackageRecord([]byte(raw)); err == nil {
			t.Errorf("%s: read as %+v", name, m)
		} else if name != "broken" && !strings.Contains(err.Error(), RecordSchema) {
			t.Errorf("%s: %v does not say which schema it reads", name, err)
		}
	}
}
