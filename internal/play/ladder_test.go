package play

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Ladder packages under testdata/packages, by the transcoder (CPU path)
// and the packager with LADDER, SURROUND_AUDIO=eac3 and, for the first
// two, HLS_SUBTITLES on. Audio: stereo AAC English (default) and German
// in "audio", E-AC-3 5.1 English in "audio-surround".
const (
	// LADDER=source,720p,480p on a 1080p HEVC source: v0 HEVC 1080p
	// 7.4 Mbit/s, v1 H.264 720p, v2 H.264 480p.
	pkgLadder = "1adde700-0000-4000-8000-000000000001"
	// LADDER=source,720p:hevc,480p: v0 HEVC 1080p, v1 HEVC 720p, v2 H.264
	// 480p.
	pkgHEVCLadder = "2e7c1add-0000-4000-8000-000000000002"
	// LADDER=source,1080p,720p on a 2160p HEVC source: v0 HEVC 2160p, v1
	// H.264 1080p, v2 H.264 720p. No subtitles in the master.
	pkgUHD = "3d4c0000-0000-4000-8000-000000000003"
)

// packagedMasterBody is a test package's master.m3u8 as it is on disk.
func packagedMasterBody(t *testing.T, id string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "packages", "*", id[:2], id, "hls", "master.m3u8"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("package %s: %v %v", id, paths, err)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// servedVariants lists a master's variants as "rung/AUDIO group".
func servedVariants(body string) []string {
	var out []string
	for _, v := range parseMaster(body).variants {
		out = append(out, v.rung+"/"+v.audio)
	}
	return out
}

// servedIFrames lists the rungs of a master's I-frame playlists.
func servedIFrames(body string) []string {
	m := parseMaster(body)
	var out []string
	for i := range m.lines {
		if r, ok := m.iframes[i]; ok {
			out = append(out, r)
		}
	}
	return out
}

// servedMedia lists a master's EXT-X-MEDIA renditions as
// "TYPE group rendition DEFAULT".
func servedMedia(body string) []string {
	var out []string
	for _, md := range parseMaster(body).media {
		out = append(out, md.typ+" "+md.group+" "+md.rend+" "+yesNo(md.isDefault))
	}
	return out
}

// checkServed holds what every served ladder must be: one codec family,
// no rung over the client's cap for its codec, every group a variant names
// listed and every listed group named, I-frame playlists only for served
// rungs, exactly one DEFAULT=YES per audio group, at most one per
// SUBTITLES group.
func checkServed(t *testing.T, body string, caps Caps) {
	t.Helper()
	m := parseMaster(body)
	families, rungs, named := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range m.variants {
		families[v.family] = true
		rungs[v.rung] = true
		if maxH := caps.VideoMaxHeight[v.family]; maxH > 0 && v.height > maxH {
			t.Errorf("%s is %dp, over the client's %s cap %d", v.rung, v.height, v.family, maxH)
		}
		named["AUDIO/"+v.audio], named["SUBTITLES/"+v.subtitles] = true, true
	}
	if len(families) > 1 {
		t.Errorf("several codec families served: %v", families)
	}
	defaults, listed := map[string]int{}, map[string]bool{}
	for _, md := range m.media {
		key := md.typ + "/" + md.group
		listed[key] = true
		if !named[key] {
			t.Errorf("%s is listed but no variant names it", key)
		}
		if md.isDefault {
			defaults[key]++
		} else if _, ok := defaults[key]; !ok {
			defaults[key] = 0
		}
	}
	for _, v := range m.variants {
		if v.audio != "" && !listed["AUDIO/"+v.audio] {
			t.Errorf("%s names audio group %q, which is not listed", v.rung, v.audio)
		}
		if v.subtitles != "" && !listed["SUBTITLES/"+v.subtitles] {
			t.Errorf("%s names subtitle group %q, which is not listed", v.rung, v.subtitles)
		}
	}
	for key, n := range defaults {
		if strings.HasPrefix(key, "AUDIO/") && n != 1 {
			t.Errorf("%s has %d DEFAULT=YES renditions, want exactly 1", key, n)
		}
		if strings.HasPrefix(key, "SUBTITLES/") && n > 1 {
			t.Errorf("%s has %d DEFAULT=YES renditions, want at most 1", key, n)
		}
	}
	for _, r := range m.iframes {
		if !rungs[r] {
			t.Errorf("I-frame playlist of %s, a rung not served", r)
		}
	}
}

// Each client is served one codec family — HEVC when an HEVC rung fits
// its HEVC cap, else H.264 — never a rung over its cap, and the 5.1 group
// only when it decodes E-AC-3.
func TestLadderServesEachClientOneCodecFamily(t *testing.T) {
	cases := []struct {
		name, pkg, caps string
		variants        []string
		iframes         []string
	}{
		{"HEVC browser: the HEVC rung, not the H.264 ones", pkgLadder, "avc,hvc,aac",
			[]string{"v0/audio"}, []string{"v0"}},
		{"browser without HEVC: the H.264 rungs", pkgLadder, "avc,aac",
			[]string{"v1/audio", "v2/audio"}, []string{"v1", "v2"}},
		{"no caps (the default set has no HEVC): H.264", pkgLadder, "",
			[]string{"v1/audio", "v2/audio"}, []string{"v1", "v2"}},
		{"E-AC-3 client gets the 5.1 group", pkgLadder, "avc,hvc,aac,eac3",
			[]string{"v0/audio", "v0/audio-surround"}, []string{"v0"}},
		{"AC-3 is not E-AC-3", pkgLadder, "avc,hvc,aac,ac3",
			[]string{"v0/audio"}, []string{"v0"}},
		{"H.264 and E-AC-3", pkgLadder, "avc,aac,ec3",
			[]string{"v1/audio", "v2/audio", "v1/audio-surround", "v2/audio-surround"}, []string{"v1", "v2"}},
		{"two HEVC rungs: ABR between them", pkgHEVCLadder, "avc,hvc,aac",
			[]string{"v0/audio", "v1/audio"}, []string{"v0", "v1"}},
		{"HEVC capped at 720: the 720p HEVC rung, not the H.264 one", pkgHEVCLadder, "avc,hvc:720,aac",
			[]string{"v1/audio"}, []string{"v1"}},
		{"no HEVC: the one H.264 rung", pkgHEVCLadder, "avc,aac",
			[]string{"v2/audio"}, []string{"v2"}},
		{"4K HEVC over a 1080 HEVC cap: the H.264 rungs", pkgUHD, "avc:1080,hvc:1080,aac",
			[]string{"v1/audio", "v2/audio"}, []string{"v1", "v2"}},
		{"4K HEVC client", pkgUHD, "avc:2160,hvc:2160,aac",
			[]string{"v0/audio"}, []string{"v0"}},
		{"H.264 capped at 720", pkgUHD, "avc:720,aac",
			[]string{"v2/audio"}, []string{"v2"}},
		{"a 4K TV decoding AC-3 and E-AC-3", pkgUHD, "avc:2160,hvc:2160,aac,mp3,ac3,eac3",
			[]string{"v0/audio", "v0/audio-surround"}, []string{"v0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := ParseCaps(tc.caps)
			s := serveLadder(packagedMasterBody(t, tc.pkg), caps, "")
			if got := servedVariants(s.body); !reflect.DeepEqual(got, tc.variants) {
				t.Errorf("variants %v, want %v", got, tc.variants)
			}
			if got := servedIFrames(s.body); !reflect.DeepEqual(got, tc.iframes) {
				t.Errorf("I-frame playlists %v, want %v", got, tc.iframes)
			}
			if s.video != tc.variants[0][:2] || s.audio != "a0" {
				t.Errorf("starts on %s/%s, want %s/a0", s.video, s.audio, tc.variants[0][:2])
			}
			checkServed(t, s.body, caps)
		})
	}
}

// What a ladder master keeps of itself: the audio renditions of the groups
// served, the SUBTITLES group as packaged, the lines in their order.
func TestLadderKeepsWhatItServesAsPackaged(t *testing.T) {
	body := packagedMasterBody(t, pkgLadder)
	s := serveLadder(body, ParseCaps("avc,aac"), "")
	want := []string{
		"AUDIO audio a0 YES", "AUDIO audio a1 NO",
		"SUBTITLES subs s0 NO", "SUBTITLES subs s1 NO", "SUBTITLES subs s2 NO",
	}
	if got := servedMedia(s.body); !reflect.DeepEqual(got, want) {
		t.Errorf("renditions %v, want %v", got, want)
	}
	// Every line served is the packaged line, in the packaged order.
	lines := strings.Split(body, "\n")
	at := 0
	for _, l := range strings.Split(s.body, "\n") {
		for at < len(lines) && lines[at] != l {
			at++
		}
		if at == len(lines) {
			t.Fatalf("served line %q is not a packaged line in order:\n%s", l, s.body)
		}
		at++
	}
	if strings.Contains(s.body, "\n\n\n") || !strings.HasSuffix(s.body, "\"v2/iframes.m3u8\"\n") {
		t.Errorf("blank lines or the end of the body are off:\n%q", s.body)
	}
	// The 5.1 group, its variants and only those go for a client without
	// E-AC-3: the stereo group and the subtitles are untouched.
	if strings.Contains(s.body, "audio-surround") || strings.Contains(s.body, "ec-3") {
		t.Errorf("the 5.1 group is still there:\n%s", s.body)
	}
	if n := strings.Count(s.body, `SUBTITLES="subs"`); n != 2 {
		t.Errorf("%d variants name the SUBTITLES group, want both", n)
	}

	// A package whose master has no SUBTITLES group gets none: the WebVTT
	// renditions on disk (hls/sN) are not added.
	uhd := serveLadder(packagedMasterBody(t, pkgUHD), ParseCaps("avc,aac"), "")
	if strings.Contains(uhd.body, "SUBTITLES") {
		t.Errorf("a SUBTITLES group appeared:\n%s", uhd.body)
	}
}

// ?q=<rung id> serves that rung alone when the client decodes it at its
// height, whatever its family; any other q serves the ladder.
func TestLadderQualityPick(t *testing.T) {
	cases := []struct {
		name, pkg, caps, q string
		variants           []string
	}{
		{"an HEVC client picks the 480p H.264 rung", pkgLadder, "avc,hvc,aac", "v2", []string{"v2/audio"}},
		{"an H.264 client picks 720p", pkgLadder, "avc,aac", "v1", []string{"v1/audio"}},
		{"the pick keeps the 5.1 group for E-AC-3", pkgLadder, "avc,aac,eac3", "v2",
			[]string{"v2/audio", "v2/audio-surround"}},
		{"a rung it can't decode: the ladder", pkgLadder, "avc,aac", "v0", []string{"v1/audio", "v2/audio"}},
		{"a rung over its cap: the ladder", pkgUHD, "avc:720,aac", "v1", []string{"v2/audio"}},
		{"a rung that isn't there: the ladder", pkgLadder, "avc,aac", "v7", []string{"v1/audio", "v2/audio"}},
		{"auto", pkgLadder, "avc,aac", "auto", []string{"v1/audio", "v2/audio"}},
		{"the on-the-fly ladder's high", pkgLadder, "avc,aac", "high", []string{"v1/audio", "v2/audio"}},
		{"the Zap pager's medium", pkgHEVCLadder, "avc,hvc,aac,opus,mp3", "medium", []string{"v0/audio", "v1/audio"}},
		{"low", pkgLadder, "avc,hvc,aac", "low", []string{"v0/audio"}},
		{"an audio rendition is not a rung", pkgLadder, "avc,aac", "a0", []string{"v1/audio", "v2/audio"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := ParseCaps(tc.caps)
			s := serveLadder(packagedMasterBody(t, tc.pkg), caps, tc.q)
			if got := servedVariants(s.body); !reflect.DeepEqual(got, tc.variants) {
				t.Errorf("variants %v, want %v", got, tc.variants)
			}
			checkServed(t, s.body, caps)
		})
	}
}

// ladderMaster is a two-rung master with the audio renditions given (one
// EXT-X-MEDIA line each), every rung once per audio group, and an extra
// line for the subtitles.
func ladderMaster(audio []string, groups map[string]string, subs ...string) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n\n#EXT-X-INDEPENDENT-SEGMENTS\n\n")
	var order []string
	seen := map[string]bool{}
	for _, a := range audio {
		sb.WriteString("#EXT-X-MEDIA:TYPE=AUDIO," + a + "\n")
		g := hlsAttributes("#X:" + a)["GROUP-ID"]
		if !seen[g] {
			seen[g] = true
			order = append(order, g)
		}
	}
	for _, s := range subs {
		sb.WriteString("#EXT-X-MEDIA:TYPE=SUBTITLES," + s + "\n")
	}
	sb.WriteString("\n")
	for _, g := range order {
		sb.WriteString(`#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS="hvc1.1.6.L120.90,` + groups[g] + `",RESOLUTION=1920x1080,AUDIO="` + g + "\"\nv0/playlist.m3u8\n")
		sb.WriteString(`#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS="avc1.64001f,` + groups[g] + `",RESOLUTION=1280x720,AUDIO="` + g + "\"\nv1/playlist.m3u8\n\n")
	}
	return sb.String()
}

// Every audio group served has exactly one DEFAULT=YES: the packager's;
// else, for a group without one (the packager marks a 5.1 group's English
// default only when English is the stereo default), the rendition in the
// stereo default's language, else the first AUTOSELECT one, else the
// first. A group with several keeps the first. A SUBTITLES group keeps at
// most one, and none stays none.
func TestLadderLeavesOneDefaultPerAudioGroup(t *testing.T) {
	codecs := map[string]string{"audio": "mp4a.40.2", "audio-surround": "ec-3"}
	stereo := []string{
		`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`,
		`URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`,
	}
	cases := []struct {
		name  string
		audio []string
		subs  []string
		want  []string
	}{
		{"no default in the 5.1 group: the stereo default's language",
			append(stereo,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="de",NAME="German 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`),
			nil,
			[]string{"AUDIO audio a0 NO", "AUDIO audio a1 YES", "AUDIO audio-surround a2 NO", "AUDIO audio-surround a3 YES"}},
		{"no rendition in that language: the first AUTOSELECT one",
			append(stereo,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="Commentary 5.1",DEFAULT=NO,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`),
			nil,
			[]string{"AUDIO audio a0 NO", "AUDIO audio a1 YES", "AUDIO audio-surround a2 NO", "AUDIO audio-surround a3 YES"}},
		{"none AUTOSELECT: the first",
			append(stereo,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="fr",NAME="French 5.1",DEFAULT=NO,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="it",NAME="Italian 5.1",DEFAULT=NO,CHANNELS="6"`),
			nil,
			[]string{"AUDIO audio a0 NO", "AUDIO audio a1 YES", "AUDIO audio-surround a2 YES", "AUDIO audio-surround a3 NO"}},
		{"two defaults: the first stays",
			[]string{
				`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
			},
			[]string{
				`URI="s0/playlist.m3u8",GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="s1/playlist.m3u8",GROUP-ID="subs",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES`,
				`URI="s2/playlist.m3u8",GROUP-ID="subs",LANGUAGE="fr",NAME="French",DEFAULT=NO`,
			},
			[]string{"AUDIO audio a0 YES", "AUDIO audio a1 NO", "AUDIO audio-surround a2 YES",
				"SUBTITLES subs s0 YES", "SUBTITLES subs s1 NO", "SUBTITLES subs s2 NO"}},
		{"no subtitle default stays none",
			stereo,
			[]string{
				`URI="s0/playlist.m3u8",GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES`,
				`URI="s1/playlist.m3u8",GROUP-ID="subs",LANGUAGE="en",NAME="English (Forced)",DEFAULT=NO,AUTOSELECT=YES,FORCED=YES`,
			},
			[]string{"AUDIO audio a0 NO", "AUDIO audio a1 YES", "SUBTITLES subs s0 NO", "SUBTITLES subs s1 NO"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := ladderMaster(tc.audio, codecs, tc.subs...)
			if len(tc.subs) > 0 {
				body = strings.ReplaceAll(body, `,AUDIO=`, `,SUBTITLES="subs",AUDIO=`)
			}
			caps := ParseCaps("avc,hvc,aac,eac3")
			s := serveLadder(body, caps, "")
			if got := servedMedia(s.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("renditions\n got %v\nwant %v", got, tc.want)
			}
			checkServed(t, s.body, caps)
			// Nothing but DEFAULT changed on those lines.
			packaged := map[string]bool{}
			for _, l := range strings.Split(body, "\n") {
				packaged[withoutDefault(l)] = true
			}
			for _, l := range strings.Split(s.body, "\n") {
				if !packaged[withoutDefault(l)] {
					t.Errorf("line changed beyond DEFAULT: %s", l)
				}
			}
			// It starts on the first variant with its group's default.
			if s.video != "v0" || s.audio != defaultOf(tc.want, "audio") {
				t.Errorf("starts on %s/%s", s.video, s.audio)
			}
		})
	}
}

// withoutDefault is line with its DEFAULT value blanked out.
func withoutDefault(line string) string {
	for _, a := range hlsAttributeSpans(line) {
		if a.key == "DEFAULT" {
			return line[:a.start] + line[a.end:]
		}
	}
	return line
}

// defaultOf is the DEFAULT=YES rendition of group in servedMedia's list.
func defaultOf(media []string, group string) string {
	for _, m := range media {
		if f := strings.Fields(m); f[0] == "AUDIO" && f[1] == group && f[3] == "YES" {
			return f[2]
		}
	}
	return ""
}

// The audio codecs a group may carry and the caps each needs. Stereo AAC
// is what every client decodes.
func TestLadderAudioCodecs(t *testing.T) {
	none := ParseCaps("avc")
	all := ParseCaps("avc,aac,aacmc,ac3,eac3,opus,mp3")
	cases := []struct {
		codec    string
		channels int
		without  bool // decoded with caps naming no audio codec
		with     bool // decoded with every audio token
	}{
		{"mp4a.40.2", 2, true, true},
		{"mp4a.40.2", 0, true, true},
		{"mp4a.40.5", 2, true, true},
		{"mp4a.40.2", 6, false, true},
		{"ec-3", 6, false, true},
		{"EC-3", 6, false, true},
		{"ac-3", 6, false, true},
		{"opus", 2, false, true},
		{"mp4a.40.34", 2, false, true},
		{"mp4a.6B", 2, false, true},
		{"fLaC", 2, false, false},
		{"dtsc", 6, false, false},
	}
	for _, tc := range cases {
		if got := audioCodecOK(tc.codec, tc.channels, none); got != tc.without {
			t.Errorf("%s %dch without audio caps: %v, want %v", tc.codec, tc.channels, got, tc.without)
		}
		if got := audioCodecOK(tc.codec, tc.channels, all); got != tc.with {
			t.Errorf("%s %dch with every audio cap: %v, want %v", tc.codec, tc.channels, got, tc.with)
		}
	}
	// One token, one codec: E-AC-3 does not open AC-3 nor the reverse.
	if audioCodecOK("ac-3", 6, ParseCaps("eac3")) || audioCodecOK("ec-3", 6, ParseCaps("ac3")) {
		t.Error("ac3 and eac3 are separate tokens")
	}
	// A 5.1 AAC group needs the aacmc opt-in.
	body := ladderMaster([]string{
		`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`,
		`URI="a1/playlist.m3u8",GROUP-ID="audio-mc",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
	}, map[string]string{"audio": "mp4a.40.2", "audio-mc": "mp4a.40.2"})
	if got := servedVariants(serveLadder(body, ParseCaps("hvc,aac"), "").body); !reflect.DeepEqual(got, []string{"v0/audio"}) {
		t.Errorf("without aacmc: %v", got)
	}
	if got := servedVariants(serveLadder(body, ParseCaps("hvc,aac,aacmc"), "").body); !reflect.DeepEqual(got, []string{"v0/audio", "v0/audio-mc"}) {
		t.Errorf("with aacmc: %v", got)
	}
}

// A master the client would get nothing from is served as packaged:
// renditions in codecs it decodes none of (PackagedPlayableBy lets an
// unknown codec through), or audio groups it decodes none of.
func TestLadderServesAsPackagedWhatItCannotChooseFrom(t *testing.T) {
	unknown := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS=\"mp4v.20.9,mp4a.40.2\",RESOLUTION=640x360\nv0/playlist.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"mp4v.20.9,mp4a.40.2\",RESOLUTION=320x180\nv1/playlist.m3u8\n"
	if s := serveLadder(unknown, ParseCaps("avc,hvc,aac"), ""); s.body != unknown || s.video != "v0" {
		t.Errorf("unknown codecs: %q (%s)", s.body, s.video)
	}
	onlySurround := ladderMaster([]string{
		`URI="a0/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
	}, map[string]string{"audio-surround": "ec-3"})
	if got := servedVariants(serveLadder(onlySurround, ParseCaps("avc,hvc,aac"), "").body); !reflect.DeepEqual(got, []string{"v0/audio-surround"}) {
		t.Errorf("an E-AC-3-only master for a client without E-AC-3: %v", got)
	}
}

// A package with one video rendition and one audio group has nothing to
// choose between: its master comes back byte for byte, even the shaka ones
// that mark no audio rendition DEFAULT and whose rung the client's caps
// would not pick.
func TestLadderLeavesASingleRenditionMasterAlone(t *testing.T) {
	for _, id := range []string{pkgLegacy, pkgLegacyHDR, pkgLegacyHDRNoRange, pkgSingle} {
		body := packagedMasterBody(t, id)
		for _, c := range []string{"avc,hvc,aac", "avc,aac", "hvc:720,aac,eac3", ""} {
			for _, q := range []string{"", "v0", "v1", "auto", "medium"} {
				s := serveLadder(body, ParseCaps(c), q)
				if s.body != body {
					t.Errorf("%s caps=%q q=%q changed:\n%s", id, c, q, s.body)
				}
				if s.video != "v0" || s.audio != "a0" {
					t.Errorf("%s starts on %s/%s, want v0/a0", id, s.video, s.audio)
				}
			}
		}
	}
	// One rendition and the 5.1 group (SURROUND_AUDIO without LADDER) is a
	// choice: the group goes for a client without E-AC-3.
	one := ladderMaster([]string{
		`URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`,
		`URI="a1/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
	}, map[string]string{"audio": "mp4a.40.2", "audio-surround": "ec-3"})
	one = strings.ReplaceAll(one, "#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720,AUDIO=\"audio\"\nv1/playlist.m3u8\n", "")
	one = strings.ReplaceAll(one, "#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS=\"avc1.64001f,ec-3\",RESOLUTION=1280x720,AUDIO=\"audio-surround\"\nv1/playlist.m3u8\n", "")
	if got := servedVariants(serveLadder(one, ParseCaps("hvc,aac"), "").body); !reflect.DeepEqual(got, []string{"v0/audio"}) {
		t.Errorf("one rendition with a 5.1 group: %v", got)
	}
}

func TestHLSAttributes(t *testing.T) {
	line := `#EXT-X-MEDIA:TYPE=AUDIO,URI="a0/playlist.m3u8?caps=avc,hvc:1080&q=v1",GROUP-ID="audio",NAME="Ton, Kommentar",DEFAULT=NO,CHANNELS="2"`
	a := hlsAttributes(line)
	want := map[string]string{"TYPE": "AUDIO", "URI": "a0/playlist.m3u8?caps=avc,hvc:1080&q=v1", "GROUP-ID": "audio",
		"NAME": "Ton, Kommentar", "DEFAULT": "NO", "CHANNELS": "2"}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("%v", a)
	}
	if got := setHLSAttribute(line, "DEFAULT", "YES"); got != strings.Replace(line, "DEFAULT=NO", "DEFAULT=YES", 1) {
		t.Errorf("set: %s", got)
	}
	if got := setHLSAttribute(`#EXT-X-MEDIA:TYPE=AUDIO,NAME="DEFAULT=NO"`, "DEFAULT", "YES"); got != `#EXT-X-MEDIA:TYPE=AUDIO,NAME="DEFAULT=NO",DEFAULT=YES` {
		t.Errorf("a look-alike in a quoted value: %s", got)
	}
	if got := setHLSAttribute("#EXT-X-MEDIA:TYPE=AUDIO,DEFAULT=NO\r", "DEFAULT", "YES"); got != "#EXT-X-MEDIA:TYPE=AUDIO,DEFAULT=YES\r" {
		t.Errorf("CRLF: %q", got)
	}
	if w, h := parseResolution("3840x1606"); w != 3840 || h != 1606 {
		t.Errorf("%dx%d", w, h)
	}
	if w, h := parseResolution("bogus"); w != 0 || h != 0 {
		t.Errorf("%dx%d", w, h)
	}
	if r := firstPathSegment("v12/playlist.m3u8?stream=x/y"); r != "v12" {
		t.Errorf("%s", r)
	}
}
