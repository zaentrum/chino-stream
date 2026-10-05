package play

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// A rendition's NAME is its title where the title says what the track is,
// else its language by name: "No dialogue" for zxx, "Unknown" for und.
func TestAudioRenditionName(t *testing.T) {
	cases := []struct {
		lang, title, want string
	}{
		{"eng", "", "English"},
		{"ger", "", "German"},
		{"por", "", "Portuguese"},
		{"zxx", "", "No dialogue"},
		{"ZXX", "", "No dialogue"},
		{"und", "", "Unknown"},
		{"", "", "Unknown"},
		{"mul", "", "Multiple languages"},
		{"MUL", "mul", "Multiple languages"},
		{"mis", "", "Other language"},
		{"en-US", "", "English"},
		{"fil", "", "fil"}, // no name for it: the code as it came
		// A title that names the track.
		{"eng", "Commentary", "Commentary"},
		{"ger", "Deutsch", "Deutsch"},
		{"und", "Director's Commentary", "Director's Commentary"},
		// A title that describes the source's format says nothing of the
		// rendition (stereo AAC).
		{"eng", "AC3 5.1 @ 640 Kbps", "English"},
		{"eng", "DTS-HD Master Audio / 5.1 / 48 kHz / 2618 kbps / 24-bit", "English"},
		{"ger", "Dolby Digital Plus", "German"},
		{"fre", "E-AC-3", "French"},
		{"und", "TrueHD Atmos 7.1", "Unknown"},
		{"zxx", "AAC", "No dialogue"},
		// A layout goes; what is left names the track.
		{"eng", "Commentary 5.1", "Commentary"},
		{"eng", "Stereo", "English"},
		{"eng", "English (5.1)", "English"},
		{"spa", "Surround 7.1", "Spanish"},
		// A number or a code is no title.
		{"und", "Track 1", "Unknown"},
		{"eng", "Audio Track 2", "English"},
		{"fre", "fre", "French"},
		{"zxx", "zxx", "No dialogue"},
		// Words that hold a format word are words.
		{"eng", "Monologue", "Monologue"},
		{"eng", "The Hobbit", "The Hobbit"},
	}
	for _, tc := range cases {
		if got := audioRenditionName(TrackInfo{Language: tc.lang, Title: tc.title}); got != tc.want {
			t.Errorf("language %q, title %q: NAME %q, want %q", tc.lang, tc.title, got, tc.want)
		}
	}
}

// The NAMEs of one group are unique (RFC 8216): a second of the same is
// numbered.
func TestAudioRenditionNamesAreUnique(t *testing.T) {
	got := audioRenditionNames([]TrackInfo{
		{Index: 0, Language: "eng"},
		{Index: 1, Language: "eng", Title: "AC3 5.1"},
		{Index: 2, Language: "und"},
		{Index: 3, Language: "und", Title: "Track 4"},
		{Index: 4, Language: "zxx"},
		{Index: 5, Language: "eng", Title: "Commentary"},
	})
	want := []string{"English", "English (2)", "Unknown", "Unknown (2)", "No dialogue", "Commentary"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NAMEs %q, want %q", got, want)
	}
}

// The on-the-fly master names each audio rendition by its title or its
// language and tags it with its language code: a film without dialogue
// "No dialogue" (zxx), an untagged track "Unknown" (und).
func TestMasterNamesTheAudioRenditions(t *testing.T) {
	captureLog(t)
	probe := `{"streams":[` +
		`{"codec_type":"video","codec_name":"h264","width":1280,"height":720},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":6,"tags":{"language":"eng","title":"AC3 5.1 @ 640 Kbps"},"disposition":{"default":1}},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":2,"tags":{"language":"zxx"}},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":2}` +
		`],"format":{"format_name":"matroska,webm","duration":"600.0","bit_rate":"4000000"}}`
	root, src := mediaFile(t, "film.mkv")
	cache, err := os.MkdirTemp("", "chino-stream-master-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	h := &HLSHandler{
		Catalog:    fakeKatalog(t, src),
		MediaRoot:  root,
		FFmpegBin:  fakeBin(t, "ffmpeg", "exit 1"),
		FFprobeBin: probeFFprobe(t, probe),
		CacheDir:   cache,
	}
	w := get(h, "/api/play/i1/master.m3u8")
	if w.Code != 200 {
		t.Fatalf("master: %d %q", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, s := range []string{
		`NAME="English",LANGUAGE="eng",DEFAULT=YES`,
		`NAME="No dialogue",LANGUAGE="zxx",DEFAULT=NO`,
		`NAME="Unknown",LANGUAGE="und",DEFAULT=NO`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("master lacks %s:\n%s", s, body)
		}
	}
}
