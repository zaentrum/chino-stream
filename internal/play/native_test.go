package play

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// nativeMedia lists a master's audio renditions as "<group> <NAME>
// <rendition> <CHANNELS> <DEFAULT>".
func nativeMedia(body string) []string {
	m := parseMaster(body)
	var out []string
	for _, md := range m.media {
		if md.typ == "AUDIO" {
			a := hlsAttributes(m.lines[md.line])
			out = append(out, md.group+" "+a["NAME"]+" "+md.rend+" "+a["CHANNELS"]+" "+yesNo(md.isDefault))
		}
	}
	return out
}

// sameMembers holds what RFC 8216 (4.3.4.1.1) wants of a master with
// several audio groups: each group the same members, in the same order,
// alike in every attribute but URI and CHANNELS — the first n of each group
// (all of them when n is 0).
func sameMembers(t *testing.T, body string, n int) {
	t.Helper()
	m := parseMaster(body)
	var order []string
	members := map[string][]string{}
	for _, md := range m.media {
		if md.typ != "AUDIO" {
			continue
		}
		var kv []string
		for k, v := range hlsAttributes(m.lines[md.line]) {
			if k != "URI" && k != "CHANNELS" && k != "GROUP-ID" {
				kv = append(kv, k+"="+v)
			}
		}
		sort.Strings(kv)
		if _, ok := members[md.group]; !ok {
			order = append(order, md.group)
		}
		members[md.group] = append(members[md.group], strings.Join(kv, ","))
	}
	if len(order) < 2 {
		t.Fatalf("one audio group: %v", order)
	}
	for _, g := range order[1:] {
		got, want := members[g], members[order[0]]
		if n > 0 && len(got) >= n && len(want) >= n {
			got, want = got[:n], want[:n]
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("group %s's members\n%v\nare not group %s's\n%v", g, got, order[0], want)
		}
	}
}

// variantLine is the EXT-X-STREAM-INF line of the variant of rung with audio
// group group.
func variantLine(body, rung, group string) string {
	m := parseMaster(body)
	for _, v := range m.variants {
		if v.rung == rung && v.audio == group {
			return m.lines[v.inf]
		}
	}
	return ""
}

func TestNativeIsACapsToken(t *testing.T) {
	for _, tc := range []struct {
		caps   string
		native bool
	}{
		{"avc,hvc,aac,eac3,native", true},
		{"native,avc,aac", true},
		{"avc,aac,NATIVE", true},
		{"avc,hvc,aac,eac3", false},
		{"", false},
	} {
		if got := ParseCaps(tc.caps).Native; got != tc.native {
			t.Errorf("ParseCaps(%q).Native = %v", tc.caps, got)
		}
	}
	// It says nothing of what the client decodes.
	a, b := ParseCaps("avc,hvc:2160,aac,eac3,native"), ParseCaps("avc,hvc:2160,aac,eac3")
	a.Native = false
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%+v\n%+v", a, b)
	}
}

// A player that plays the master natively (native) and decodes a further
// audio group is served Apple's shape: each group it decodes, each rung once
// per group, and every further group holding the stereo group's members in
// their order, as they are named and with its default — each played by its
// companion (by source track, else the first of its language) where it has
// one, else by its own stereo rendition, CODECS then naming AAC too. A
// further rendition that is nobody's companion stays after them. Warmed:
// where each group starts.
func TestLadderServesANativePlayerEachGroup(t *testing.T) {
	codecs := map[string]string{"audio": "mp4a.40.2", "audio-surround": "ec-3", "audio-ec3": "ec-3", "audio-ac3": "ac-3"}
	en := `URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`
	de := `URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="de",NAME="German",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`
	commentary := `URI="a0/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English · Commentary",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2"`
	english := `URI="a1/playlist.m3u8",GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"`
	english51 := `URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`
	cases := []struct {
		name     string
		audio    []string
		twins    audioTwins
		want     []string // nativeMedia
		variants []string
		codecs   string // the further group's variant's CODECS
		members  int    // of each group alike (0: all)
		more     []string
	}{
		{"a companion for every language",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="de",NAME="German 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"audio English a0 2 NO", "audio German a1 2 YES", "audio-surround English a2 6 NO", "audio-surround German a3 6 YES"},
			[]string{"v0/audio", "v0/audio-surround"}, "hvc1.1.6.L120.90,ec-3", 0, []string{"a3"}},
		{"English 5.1 only: German plays stereo in the 5.1 group too",
			[]string{en, de, `URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"audio English a0 2 NO", "audio German a1 2 YES", "audio-surround English a2 6 NO", "audio-surround German a1 2 YES"},
			[]string{"v0/audio", "v0/audio-surround"}, "hvc1.1.6.L120.90,ec-3,mp4a.40.2", 0, nil},
		{"the manifest pairs by source track: the second English's companion",
			[]string{commentary, english, english51},
			audioTwins{"a2": "a1"},
			[]string{"audio English · Commentary a0 2 NO", "audio English a1 2 YES",
				"audio-surround English · Commentary a0 2 NO", "audio-surround English a2 6 YES"},
			[]string{"v0/audio", "v0/audio-surround"}, "hvc1.1.6.L120.90,ec-3,mp4a.40.2", 0, []string{"a2"}},
		{"without the manifest, by language: the first English's",
			[]string{commentary, english, english51},
			nil,
			[]string{"audio English · Commentary a0 2 NO", "audio English a1 2 YES",
				"audio-surround English · Commentary a2 6 NO", "audio-surround English a1 2 YES"},
			[]string{"v0/audio", "v0/audio-surround"}, "hvc1.1.6.L120.90,ec-3,mp4a.40.2", 0, nil},
		{"companions in languages no stereo track has: after the members",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="fr",NAME="French 5.1",DEFAULT=NO,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-surround",LANGUAGE="it",NAME="English",DEFAULT=NO,CHANNELS="6"`},
			nil,
			[]string{"audio English a0 2 NO", "audio German a1 2 YES", "audio-surround English a0 2 NO", "audio-surround German a1 2 YES",
				"audio-surround French 5.1 a2 6 NO", "audio-surround English (2) a3 6 NO"},
			[]string{"v0/audio", "v0/audio-surround"}, "hvc1.1.6.L120.90,ec-3,mp4a.40.2", 2, nil},
		{"two further groups it decodes: both, each mirrored",
			[]string{en, de,
				`URI="a2/playlist.m3u8",GROUP-ID="audio-ec3",LANGUAGE="en",NAME="English 5.1",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6"`,
				`URI="a3/playlist.m3u8",GROUP-ID="audio-ac3",LANGUAGE="en",NAME="English 5.1",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="6"`},
			nil,
			[]string{"audio English a0 2 NO", "audio German a1 2 YES", "audio-ec3 English a2 6 NO", "audio-ec3 German a1 2 YES",
				"audio-ac3 English a3 6 NO", "audio-ac3 German a1 2 YES"},
			[]string{"v0/audio", "v0/audio-ec3", "v0/audio-ac3"}, "hvc1.1.6.L120.90,ec-3,mp4a.40.2", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := ladderMaster(tc.audio, codecs)
			caps := ParseCaps("avc,hvc,aac,eac3,ac3,native")
			s := serveLadder(body, caps, "", tc.twins)
			if got := nativeMedia(s.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("audio\n%v, want\n%v\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"), s.body)
			}
			if got := servedVariants(s.body); !reflect.DeepEqual(got, tc.variants) {
				t.Errorf("variants %v, want %v", got, tc.variants)
			}
			further := strings.TrimPrefix(tc.variants[1], "v0/")
			if got := hlsAttributes(variantLine(s.body, "v0", further))["CODECS"]; got != tc.codecs {
				t.Errorf("the %s variant's CODECS %q, want %q", further, got, tc.codecs)
			}
			sameMembers(t, s.body, tc.members)
			checkServed(t, s.body, caps)
			if s.video != "v0" || s.audio != map[bool]string{true: "a1", false: "a0"}[strings.Contains(tc.want[1], "YES")] || !reflect.DeepEqual(s.more, tc.more) {
				t.Errorf("start %s/%s, more %v; want more %v", s.video, s.audio, s.more, tc.more)
			}
			// A client that decodes the stereo group alone: as for any client.
			stereo := ParseCaps("avc,hvc,aac,native")
			if got, want := serveLadder(body, stereo, "", tc.twins).body, serveLadder(body, ParseCaps("avc,hvc,aac"), "", tc.twins).body; got != want {
				t.Errorf("a native player without E-AC-3:\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// The packaged ladder, for native players: HEVC with E-AC-3 its top rung
// once per group, H.264 its two, a quality pick that rung; without E-AC-3
// as any client. The further group's variant keeps its line but for CODECS,
// which adds AAC (German plays stereo there).
func TestLadderServesANativePlayerTheLadderPerGroup(t *testing.T) {
	body := packagedMasterBody(t, pkgLadder)
	stereo := []string{"audio English a0 2 YES", "audio German a1 2 NO"}
	mirrored := append(stereo, "audio-surround English a2 6 YES", "audio-surround German a1 2 NO")
	for _, tc := range []struct {
		caps, q  string
		media    []string
		variants []string
	}{
		{"avc,hvc,aac,eac3,native", "", mirrored, []string{"v0/audio", "v0/audio-surround"}},
		{"avc,aac,eac3,native", "", mirrored, []string{"v1/audio", "v2/audio", "v1/audio-surround", "v2/audio-surround"}},
		{"avc,hvc,aac,eac3,native", "v2", mirrored, []string{"v2/audio", "v2/audio-surround"}},
		{"avc,hvc,aac,native", "", stereo, []string{"v0/audio"}},
		{"avc,aac,ac3,native", "", stereo, []string{"v1/audio", "v2/audio"}},
	} {
		caps := ParseCaps(tc.caps)
		s := serveLadder(body, caps, tc.q, audioTwins{"a2": "a0"})
		if got := nativeMedia(s.body); !reflect.DeepEqual(got, tc.media) {
			t.Errorf("caps=%s q=%s: audio %v, want %v", tc.caps, tc.q, got, tc.media)
		}
		if got := servedVariants(s.body); !reflect.DeepEqual(got, tc.variants) {
			t.Errorf("caps=%s q=%s: variants %v, want %v", tc.caps, tc.q, got, tc.variants)
		}
		checkServed(t, s.body, caps)
		if len(tc.media) > 2 {
			sameMembers(t, s.body, 0)
		} else {
			nonNative := ParseCaps(strings.TrimSuffix(tc.caps, ",native"))
			if want := serveLadder(body, nonNative, tc.q, audioTwins{"a2": "a0"}).body; s.body != want {
				t.Errorf("caps=%s: not as for any client:\n%s", tc.caps, s.body)
			}
		}
	}
	s := serveLadder(body, ParseCaps("avc,hvc,aac,eac3,native"), "", audioTwins{"a2": "a0"})
	if got, want := variantLine(s.body, "v0", "audio-surround"),
		`#EXT-X-STREAM-INF:BANDWIDTH=7693886,AVERAGE-BANDWIDTH=7636693,CODECS="hvc1.1.6.L120.90,ec-3,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=23.976,VIDEO-RANGE=SDR,AUDIO="audio-surround",SUBTITLES="subs",CLOSED-CAPTIONS=NONE`; got != want {
		t.Errorf("the 5.1 variant\n%s\nwant\n%s", got, want)
	}
	if got, want := variantLine(s.body, "v0", "audio"),
		`#EXT-X-STREAM-INF:BANDWIDTH=7436924,AVERAGE-BANDWIDTH=7380150,CODECS="hvc1.1.6.L120.90,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=23.976,VIDEO-RANGE=SDR,AUDIO="audio",SUBTITLES="subs",CLOSED-CAPTIONS=NONE`; got != want {
		t.Errorf("the stereo variant\n%s\nwant\n%s", got, want)
	}
	if s.audio != "a0" || !reflect.DeepEqual(s.more, []string{"a2"}) {
		t.Errorf("start %s, more %v", s.audio, s.more)
	}
}

// Through the handlers, for a package in either layout: a native player
// with E-AC-3 gets each group, the 5.1 group's members those of the stereo
// group, the commentary playing stereo there; both groups' starts warmed;
// /play/info its tracks as any client's, the one the 5.1 group plays by a
// companion marked "surround". Without E-AC-3 as any client.
func TestAPackagedMasterServesANativePlayerEachGroup(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(map[bool]string{false: "manifest.json", true: "package.json"}[record], func(t *testing.T) {
			h, ph, dir := unionPackage(t, record, commentaryFirst.stereo, commentaryFirst.surround)
			w := get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac,eac3,native")
			body := w.Body.String()
			if got, want := nativeMedia(body), []string{"audio English · Commentary a0 2 NO", "audio English a1 2 YES",
				"audio-surround English · Commentary a0 2 NO", "audio-surround English a2 6 YES"}; w.Code != 200 || !reflect.DeepEqual(got, want) {
				t.Fatalf("%d audio %v, want %v:\n%s", w.Code, got, want, body)
			}
			if got := servedVariants(body); !reflect.DeepEqual(got, []string{"v0/audio", "v0/audio-surround"}) {
				t.Errorf("variants %v", got)
			}
			if a := hlsAttributes(variantLine(body, "v0", "audio-surround")); a["CODECS"] != "hvc1.1.6.L120.90,ec-3,mp4a.40.2" ||
				a["BANDWIDTH"] != "7650000" || a["AVERAGE-BANDWIDTH"] != "7550000" {
				t.Errorf("the 5.1 variant: %v", a)
			}
			sameMembers(t, body, 0)
			for _, rend := range []string{"a1", "a2"} {
				if _, ok := packagedCache.Load(filepath.Join(dir, "hls", rend, "init.mp4")); !ok {
					t.Errorf("%s, where a group starts, was not warmed", rend)
				}
			}
			tracks, codec := infoAudio(t, ph, unionItem, "avc,hvc,aac,eac3,native")
			want := []map[string]any{
				{"index": 0.0, "codec": "mp4a.40.2", "language": "eng", "name": "English · Commentary", "title": "English · Commentary",
					"default": false, "channels": 2.0},
				{"index": 1.0, "codec": "mp4a.40.2", "language": "eng", "name": "English", "title": "English", "default": true, "channels": 2.0,
					"surround": map[string]any{"group": "audio-surround", "rendition": "a2", "codec": "ec-3", "channels": 6.0}},
			}
			if !reflect.DeepEqual(tracks, want) || codec != "aac" {
				t.Errorf("/play/info\n got %v %s\nwant %v aac", tracks, codec, want)
			}

			a := get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac,native").Body.String()
			b := get(h, "/api/play/"+unionItem+"/master.m3u8?caps=avc,hvc,aac").Body.String()
			if strings.ReplaceAll(a, "caps=avc,hvc,aac,native", "caps=avc,hvc,aac") != b {
				t.Errorf("a native player without E-AC-3:\n%s\nwant\n%s", a, b)
			}
			plain, _ := infoAudio(t, ph, unionItem, "avc,hvc,aac")
			if got, _ := infoAudio(t, ph, unionItem, "avc,hvc,aac,native"); !reflect.DeepEqual(got, plain) {
				t.Errorf("/play/info without E-AC-3: %v, want %v", got, plain)
			}
		})
	}
}
