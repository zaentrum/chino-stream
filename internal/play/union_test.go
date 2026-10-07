package play

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// unionLines are a served master's lines as the packaged ones they come
// from, the attributes the union sets blanked out: an audio rendition's
// GROUP-ID, DEFAULT and NAME, a variant's AUDIO, CODECS, BANDWIDTH and
// AVERAGE-BANDWIDTH.
func unionLines(body string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(body, "\n") {
		for _, key := range []string{"GROUP-ID", "DEFAULT", "NAME", "AUDIO", "CODECS", "BANDWIDTH", "AVERAGE-BANDWIDTH"} {
			for _, a := range hlsAttributeSpans(l) {
				if a.key == key {
					l = l[:a.start] + l[a.end:]
					break
				}
			}
		}
		out[l] = true
	}
	return out
}

// A client that decodes the 5.1 companions (eac3) is served one variant per
// rung, not one per rung and audio group: its audio group (the 5.1 one) is
// every stereo rendition with each companion just before the stereo
// rendition of its source track — no track goes, and a player taking the
// first rendition of a language takes its 5.1. The default is the companion
// of the stereo default, else the stereo default itself. Each variant is
// the 5.1 group's, its CODECS with the stereo codec added (RFC 8216: every
// format of the group), its BANDWIDTH the larger of the rung's two. The
// lines are the packaged lines, only those attributes set.
func TestLadderServesAUnionOfTheAudioGroups(t *testing.T) {
	codecs := map[string]string{"audio": "mp4a.40.2", "audio-surround": "ec-3", "audio-ec3": "ec-3", "audio-ac3": "ac-3"}
	en := `URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`
	de := `URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`
	cases := []struct {
		name  string
		audio []string
		twins audioTwins
		want  []string // servedMedia
		start string
		group string
	}{
		{"a companion for every language: each before its stereo rendition, German's the default",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="de",NAME="German 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-surround a2 NO", "AUDIO audio-surround a0 NO", "AUDIO audio-surround a3 YES", "AUDIO audio-surround a1 NO"},
			"a3", "audio-surround"},
		{"English 5.1 only: German stays the default, in stereo",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-surround a2 NO", "AUDIO audio-surround a0 NO", "AUDIO audio-surround a1 YES"},
			"a1", "audio-surround"},
		{"companions in languages no stereo track has: after the stereo ones",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="fr",NAME="French 5.1",DEFAULT=NO,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="it",NAME="Italian 5.1",DEFAULT=NO,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-surround a0 NO", "AUDIO audio-surround a1 YES", "AUDIO audio-surround a2 NO", "AUDIO audio-surround a3 NO"},
			"a1", "audio-surround"},
		{"the manifest pairs by source track: the companion of the second English, not the first",
			[]string{
				`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English · Commentary",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`,
				`URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`},
			audioTwins{"a2": "a1"},
			[]string{"AUDIO audio-surround a0 NO", "AUDIO audio-surround a2 YES", "AUDIO audio-surround a1 NO"},
			"a2", "audio-surround"},
		{"without the manifest, by language: the first English",
			[]string{
				`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English · Commentary",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`,
				`URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-surround a2 NO", "AUDIO audio-surround a0 NO", "AUDIO audio-surround a1 YES"},
			"a1", "audio-surround"},
		{"two defaults in the stereo group: the first's companion",
			[]string{
				`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=NO,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-surround a2 YES", "AUDIO audio-surround a0 NO", "AUDIO audio-surround a1 NO"},
			"a2", "audio-surround"},
		{"two further groups it decodes: the first, the other goes",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-ec3",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-ac3",LANGUAGE="en",NAME="English 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"AUDIO audio-ec3 a2 NO", "AUDIO audio-ec3 a0 NO", "AUDIO audio-ec3 a1 YES"},
			"a1", "audio-ec3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := ladderMaster(tc.audio, codecs)
			caps := ParseCaps("avc,hvc,aac,eac3,ac3")
			s := serveLadder(body, caps, "", tc.twins)
			if got := servedMedia(s.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("renditions\n got %v\nwant %v", got, tc.want)
			}
			if s.video != "v0" || s.audio != tc.start {
				t.Errorf("starts on %s/%s, want v0/%s", s.video, s.audio, tc.start)
			}
			// The HEVC rung (the client's family), once — not once per group.
			if got := servedVariants(s.body); !reflect.DeepEqual(got, []string{"v0/" + tc.group}) {
				t.Errorf("variants %v, want v0 once", got)
			}
			surroundCodec := codecs[tc.group]
			for _, v := range parseMaster(s.body).variants {
				if want := []string{surroundCodec, "mp4a.40.2"}; !reflect.DeepEqual(v.audioCodecs, want) {
					t.Errorf("%s CODECS audio %v, want %v", v.rung, v.audioCodecs, want)
				}
			}
			checkServed(t, s.body, caps)
			packaged := unionLines(body)
			for l := range unionLines(s.body) {
				if !packaged[l] {
					t.Errorf("line changed beyond the union's attributes: %s", l)
				}
			}
		})
	}
}

// A NAME the union would hold twice is made unique (RFC 8216: every
// rendition of a group its own NAME); the larger BANDWIDTH and
// AVERAGE-BANDWIDTH of a rung's two variants is its variant's.
func TestLadderUnionNamesAndBandwidth(t *testing.T) {
	body := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,URI="a1/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=5000000,AVERAGE-BANDWIDTH=4900000,CODECS="hvc1.1.6.L120.90,mp4a.40.2",RESOLUTION=1920x1080,AUDIO="audio"` + "\nv0/playlist.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=4800000,AVERAGE-BANDWIDTH=4700000,CODECS="hvc1.1.6.L120.90,ec-3",RESOLUTION=1920x1080,AUDIO="audio-surround"` + "\nv0/playlist.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720,AUDIO="audio"` + "\nv1/playlist.m3u8\n"
	s := serveLadder(body, ParseCaps("hvc,aac,eac3"), "", nil)
	m := parseMaster(s.body)
	var names []string
	for _, md := range m.media {
		names = append(names, hlsAttributes(m.lines[md.line])["NAME"])
	}
	if !reflect.DeepEqual(names, []string{"English 5.1", "English 5.1 (2)"}) {
		t.Errorf("names %v", names)
	}
	if len(m.variants) != 1 {
		t.Fatalf("variants %v:\n%s", servedVariants(s.body), s.body)
	}
	a := hlsAttributes(m.lines[m.variants[0].inf])
	if a["BANDWIDTH"] != "5000000" || a["AVERAGE-BANDWIDTH"] != "4900000" || a["CODECS"] != "hvc1.1.6.L120.90,ec-3,mp4a.40.2" {
		t.Errorf("variant %v", a)
	}
}

// A client without E-AC-3, and every client of a title without companions,
// is served the stereo group as before: one variant per rung, no union.
func TestLadderServesTheStereoGroupWithoutCompanions(t *testing.T) {
	for _, tc := range []struct{ pkg, caps string }{
		{pkgLadder, "avc,hvc,aac"}, {pkgLadder, "avc,aac,ac3"}, {pkgStereoLadder, "avc,hvc,aac,eac3"}, {pkgStereoLadder, "avc,aac,eac3"},
	} {
		body := packagedMasterBody(t, tc.pkg)
		s := serveLadder(body, ParseCaps(tc.caps), "", nil)
		for _, v := range parseMaster(s.body).variants {
			if v.audio != "audio" || !reflect.DeepEqual(v.audioCodecs, []string{"mp4a.40.2"}) {
				t.Errorf("%s caps=%s: variant %s/%s %v", tc.pkg[:8], tc.caps, v.rung, v.audio, v.audioCodecs)
			}
		}
		if strings.Contains(s.body, "audio-surround") {
			t.Errorf("%s caps=%s: the 5.1 group is served:\n%s", tc.pkg[:8], tc.caps, s.body)
		}
	}
}

// commentaryFirst is a package whose first English track is a stereo
// commentary and whose second, the default, has a 5.1 companion: by
// language the companion would pair with the commentary, by source track
// (the manifest's idx, the record's sourceStreamIndex) with its own track.
type unionTrack struct {
	id, lang, hlsLang, name, title, codec string
	channels, src                         int
	def                                   bool
}

var commentaryFirst = struct{ stereo, surround []unionTrack }{
	stereo: []unionTrack{
		{id: "a0", lang: "eng", hlsLang: "en", name: "English · Commentary", title: "Commentary", codec: "mp4a.40.2", channels: 2, src: 0},
		{id: "a1", lang: "eng", hlsLang: "en", name: "English", codec: "mp4a.40.2", channels: 2, src: 1, def: true},
	},
	surround: []unionTrack{
		{id: "a2", lang: "eng", hlsLang: "en", name: "English 5.1", codec: "ec-3", channels: 6, src: 1, def: true},
	},
}

// writeUnionPackage writes a package of one HEVC rendition and the tracks
// given into dir — its master with one variant per audio group as the
// packager writes it, every media playlist with stand-ins for its init and
// segments ("<rendition> <file>"), and its record: a library package.json
// (sourceStreamIndex = 1 + the track's place) with record, else a
// manifest.json (idx).
func writeUnionPackage(t *testing.T, dir string, record bool, stereo, surround []unionTrack) {
	t.Helper()
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS\n\n")
	media := func(group string, tracks []unionTrack) {
		for _, a := range tracks {
			master.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,URI="` + a.id + `/playlist.m3u8",GROUP-ID="` + group + `",LANGUAGE="` + a.hlsLang +
				`",NAME="` + a.name + `",DEFAULT=` + yesNo(a.def) + `,AUTOSELECT=YES,CHANNELS="` + itoa(a.channels) + "\"\n")
		}
	}
	media("audio", stereo)
	media("audio-surround", surround)
	master.WriteString("\n#EXT-X-STREAM-INF:BANDWIDTH=7400000,AVERAGE-BANDWIDTH=7300000,CODECS=\"hvc1.1.6.L120.90,mp4a.40.2\",RESOLUTION=1920x1080,FRAME-RATE=23.976,VIDEO-RANGE=SDR,AUDIO=\"audio\"\nv0/playlist.m3u8\n")
	if len(surround) > 0 {
		master.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=7650000,AVERAGE-BANDWIDTH=7550000,CODECS=\"hvc1.1.6.L120.90,ec-3\",RESOLUTION=1920x1080,FRAME-RATE=23.976,VIDEO-RANGE=SDR,AUDIO=\"audio-surround\"\nv0/playlist.m3u8\n")
	}
	files := map[string]string{"hls/master.m3u8": master.String(), ".complete": "sha256:x\n"}
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.000,\nseg-00001.m4s\n#EXTINF:6.000,\nseg-00002.m4s\n#EXT-X-ENDLIST\n"
	for _, rend := range []string{"v0"} {
		files["hls/"+rend+"/playlist.m3u8"] = playlist
	}
	rendition := func(a unionTrack, group string) map[string]any {
		e := map[string]any{"id": a.id, "dir": "hls/" + a.id, "codec": a.codec, "language": a.lang, "title": a.title,
			"default": a.def, "channels": a.channels, "bitrateBps": map[bool]int{true: 448000, false: 192000}[a.channels > 2],
			"segments": 2, "group": group, "name": a.name}
		if record {
			e["sourceStreamIndex"] = 1 + a.src
			e["visible"], e["purpose"], e["purposeFrom"] = true, "main", "assumed"
		} else {
			e["idx"] = a.src
		}
		files["hls/"+a.id+"/playlist.m3u8"] = playlist
		return e
	}
	var stereoJSON, surroundJSON []any
	for _, a := range stereo {
		stereoJSON = append(stereoJSON, rendition(a, "audio"))
	}
	for _, a := range surround {
		surroundJSON = append(surroundJSON, rendition(a, "audio-surround"))
	}
	video := map[string]any{"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080,
		"bitrateBps": 7200000, "hdr": false, "frameRate": "24000/1001", "segments": 2, "targetDuration": 6}
	renditions := map[string]any{"video": []any{video}, "audio": stereoJSON}
	if len(surroundJSON) > 0 {
		renditions["audioSurround"] = surroundJSON
	}
	var doc map[string]any
	name := "manifest.json"
	if record {
		name = "package.json"
		doc = map[string]any{"schema": "zaentrum.library.package/2", "packageId": "7a7a7a7a-0000-4000-8000-0000000000a7",
			"createdAt": "2026-10-07T09:00:00Z", "packagedBy": "packager test", "state": "complete", "role": "canonical",
			"durationMs": 600000, "renditions": renditions, "subtitles": []any{}, "trickplay": nil,
			"hls":      map[string]any{"master": "hls/master.m3u8", "segmentSeconds": 6, "audioGroups": []string{"audio", "audio-surround"}},
			"fidelity": map[string]any{"lossless": true, "losses": []any{}}, "essence": map[string]any{}, "checksums": map[string]any{}}
	} else {
		doc = map[string]any{"version": 2, "itemId": "x", "type": "movie", "title": "Union", "durationMs": 600000,
			"renditions": renditions, "subtitles": []any{}}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	files[name] = string(raw)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, "playlist.m3u8") {
			rend := filepath.Base(filepath.Dir(p))
			for _, f := range playlistMedia(body) {
				if err := os.WriteFile(filepath.Join(filepath.Dir(p), f), []byte(rend+" "+f), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

const unionItem = "9b9b9b9b-0000-4000-8000-00000000009b"

// unionPackage serves a writeUnionPackage package of tracks as the item
// unionItem: in the library as a version (record) or in the package store.
// Its original is retired.
func unionPackage(t *testing.T, record bool, stereo, surround []unionTrack) (*HLSHandler, *Handler, string) {
	t.Helper()
	usePackages(t, t.TempDir())
	dir := filepath.Join(t.TempDir(), "movies", unionItem[:2], unionItem)
	ref := &catalog.PackageRef{Dir: dir, Record: "manifest.json"}
	if record {
		dir = filepath.Join(dir, "versions", "8c8c8c8c-0000-4000-8000-00000000008c")
		ref = &catalog.PackageRef{VersionID: "8c8c8c8c-0000-4000-8000-00000000008c", Dir: dir, Record: "package.json"}
	}
	writeUnionPackage(t, dir, record, stereo, surround)
	f := newFakeLibrary(t)
	f.setItem(catalog.Playback{ItemID: unionItem, Package: ref})
	res := f.resolver()
	state := t.TempDir()
	ffmpeg := windowFFmpeg(t, state, 0, 4, "")
	h := &HLSHandler{Catalog: noAsset(t), Packages: res, FFmpegBin: ffmpeg, CacheDir: t.TempDir(), TranscodePreset: "veryfast"}
	return h, &Handler{Catalog: h.Catalog, Packages: res, FFmpegBin: ffmpeg, CacheDir: h.CacheDir}, dir
}

// infoAudio is GET /play/info's audio_tracks and audio_codec for caps.
func infoAudio(t *testing.T, h *Handler, item, caps string) ([]map[string]any, string) {
	t.Helper()
	w := getPlay(h, "/api/play/"+item+"/info?caps="+caps)
	if w.Code != 200 {
		t.Fatalf("info: %d %q", w.Code, w.Body)
	}
	var info struct {
		AudioCodec string           `json:"audio_codec"`
		Audio      []map[string]any `json:"audio_tracks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return info.Audio, info.AudioCodec
}

// Through the handlers, for a package in either layout: the companion
// pairs with the stereo rendition of its source track, as the manifest
// (idx) or the record (sourceStreamIndex) says, not with the first English
// one; the master serves it and /play/info lists it so, in that order.
func TestAPackagedMasterPairsCompanionsByTheirSourceTrack(t *testing.T) {
	for _, record := range []bool{false, true} {
		h, ph, _ := unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
		w := get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac,eac3")
		if got, want := servedMedia(w.Body.String()), []string{"AUDIO audio-surround a0 NO", "AUDIO audio-surround a2 YES",
			"AUDIO audio-surround a1 NO"}; !reflect.DeepEqual(got, want) {
			t.Errorf("record=%v: renditions %v, want %v\n%s", record, got, want, w.Body)
		}
		tracks, codec := infoAudio(t, ph, unionItem, "avc,hvc,aac,eac3")
		var got []string
		for _, tr := range tracks {
			got = append(got, tr["rendition"].(string)+" "+tr["name"].(string))
		}
		if want := []string{"a0 English · Commentary", "a2 English 5.1", "a1 English"}; !reflect.DeepEqual(got, want) || codec != "eac3" {
			t.Errorf("record=%v: /play/info %v %s, want %v eac3", record, got, codec, want)
		}
	}
}

// /play/info lists the audio a client is served, in its master's order:
// for an E-AC-3 client of a title with companions every rendition of its
// one group — each with its NAME there, codec, language, channels, default,
// group and rendition id, to pick it by — and audio_codec the one it starts
// on; for any other client the stereo tracks exactly as before, without
// those fields. A legacy manifest.json and a library package.json alike.
func TestPlayInfoListsTheCompanionsAClientIsServed(t *testing.T) {
	stereoTracks := []map[string]any{
		{"index": 0.0, "codec": "mp4a.40.2", "language": "eng", "name": "English", "title": "English", "default": true, "channels": 2.0},
		{"index": 1.0, "codec": "mp4a.40.2", "language": "ger", "name": "German", "title": "German", "default": false, "channels": 2.0},
	}
	unionTracks := []map[string]any{
		{"index": 0.0, "codec": "ec-3", "language": "eng", "name": "English 5.1", "title": "English 5.1", "default": true, "channels": 6.0,
			"group": "audio-surround", "rendition": "a2"},
		{"index": 1.0, "codec": "mp4a.40.2", "language": "eng", "name": "English", "title": "English", "default": false, "channels": 2.0,
			"group": "audio-surround", "rendition": "a0"},
		{"index": 2.0, "codec": "mp4a.40.2", "language": "ger", "name": "German", "title": "German", "default": false, "channels": 2.0,
			"group": "audio-surround", "rendition": "a1"},
	}
	check := func(t *testing.T, ph *Handler, item string) {
		t.Helper()
		for _, tc := range []struct {
			caps  string
			want  []map[string]any
			codec string
		}{
			{"avc,hvc,aac,eac3", unionTracks, "eac3"},
			{"avc,aac,eac3", unionTracks, "eac3"},
			{"avc,hvc,aac", stereoTracks, "aac"},
			{"avc,hvc,aac,ac3", stereoTracks, "aac"},
		} {
			got, codec := infoAudio(t, ph, item, tc.caps)
			if !reflect.DeepEqual(got, tc.want) || codec != tc.codec {
				t.Errorf("caps=%s:\n got %v %s\nwant %v %s", tc.caps, got, codec, tc.want, tc.codec)
			}
		}
	}
	t.Run("manifest.json", func(t *testing.T) {
		packages := usePackages(t, filepath.Join("testdata", "packages"))
		root, src := mediaFile(t, "film.mkv")
		check(t, &Handler{Catalog: fakeKatalog(t, src), Packages: packages, MediaRoot: root}, pkgLadder)
	})
	t.Run("package.json", func(t *testing.T) {
		l := stageLibrary(t)
		l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libOld))})
		check(t, &Handler{Catalog: noAsset(t), Packages: l.packages}, libItem)
	})
}
