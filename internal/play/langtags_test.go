package play

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// A language's name says the script and the region its code names, in
// brackets after it, as the platforms say them in their standard form
// (docs/chino, subtitles.md): never "Brazilian Portuguese".
func TestLangDisplayNamesTheScriptAndTheRegion(t *testing.T) {
	for code, want := range map[string]string{
		"pt-BR":          "Portuguese (Brazil)",
		"pt_br":          "Portuguese (Brazil)",
		"por-BR":         "Portuguese (Brazil)",
		"zh-Hans":        "Chinese (Simplified)",
		"zh-hant":        "Chinese (Traditional)",
		"zh-Hant-TW":     "Chinese (Traditional, Taiwan)",
		"es-419":         "Spanish (Latin America)",
		"en-GU":          "English (Guam)",
		"en-US":          "English (United States)",
		"en-UK":          "English (United Kingdom)", // UK is GB
		"fr-CA":          "French (Canada)",
		"nl-BE":          "Dutch (Belgium)",
		"de-CH":          "German (Switzerland)",
		"en-US-x-custom": "English (United States)",
		"de-1901":        "German", // a variant names nothing
		"en-XY":          "English",
		"eng":            "English",
		"zxx-Latn":       "No dialogue",
		"und":            "Unknown",
		"fil-PH":         "fil-PH", // no name for the language: the code as it came
	} {
		if got := langDisplay(code); got != want {
			t.Errorf("%q: %q, want %q", code, got, want)
		}
	}
}

// A title that names a script or a region of the track's language that its
// tag does not - a code of the language, a region's code in brackets after
// its name, the region's or the regional form's name, a Chinese title's
// script - keeps it in the track's name, by chino-web's rules
// (lib/languages.ts), and /play/info's tag of the track says it. One that
// names the tag's own again says nothing more; one that names another stays
// as the title says it.
func TestATitlesScriptOrRegionStaysInTheName(t *testing.T) {
	cases := []struct {
		lang, title, name, tag string
	}{
		// A code of the track's language.
		{"por", "PT-BR", "Portuguese (Brazil)", "pt-BR"},
		{"por", "pt-BR", "Portuguese (Brazil)", "pt-BR"},
		{"por", "pt_BR", "Portuguese (Brazil)", "pt-BR"},
		{"por", "PT-PT", "Portuguese (Portugal)", "pt-PT"},
		{"chi", "zh-Hans", "Chinese (Simplified)", "zh-Hans"},
		{"zho", "zh-Hant", "Chinese (Traditional)", "zh-Hant"},
		{"chi", "zh-Hant-TW", "Chinese (Traditional, Taiwan)", "zh-Hant-TW"},
		{"spa", "ES-419", "Spanish (Latin America)", "es-419"},
		{"spa", "es-ES", "Spanish (Spain)", "es-ES"},
		{"eng", "en-GB", "English (United Kingdom)", "en-GB"},
		{"eng", "en-US-x-custom", "English (United States)", "en-US"},
		// A region's code in brackets after the language's name, where it
		// is one of the language's regions.
		{"por", "Português (BR)", "Portuguese (Brazil)", "pt-BR"},
		{"por", "Português (PT)", "Portuguese (Portugal)", "pt-PT"},
		{"spa", "Español (419)", "Spanish (Latin America)", "es-419"},
		{"spa", "Español [MX]", "Spanish (Mexico)", "es-MX"},
		{"fre", "Français (CA)", "French (Canada)", "fr-CA"},
		{"ger", "Deutsch (CH)", "German (Switzerland)", "de-CH"},
		{"chi", "中文（TW）", "Chinese (Taiwan)", "zh-TW"},
		{"eng", "English (UK)", "English (United Kingdom)", "en-GB"},
		{"por", "Português (PT-BR)", "Portuguese (Brazil)", "pt-BR"},
		// The language's own code where it is none of its regions, or in
		// lower case: the language. Other capitals are a word.
		{"swe", "Svenska (SV)", "Swedish", "swe"},
		{"ara", "Arabic (AR)", "Arabic", "ara"},
		{"por", "Português (pt)", "Portuguese", "por"},
		{"eng", "English (BD)", "English · BD", "eng"},
		// The region's or the script's name, as the word list writes it.
		{"spa", "Spanish (Latin America)", "Spanish (Latin America)", "es-419"},
		{"chi", "Chinese (Simplified)", "Chinese (Simplified)", "zh-Hans"},
		{"chi", "Chinese (Traditional, Taiwan)", "Chinese (Traditional, Taiwan)", "zh-Hant-TW"},
		{"por", "Português (Brasil)", "Portuguese (Brazil)", "pt-BR"},
		{"por", "Português - Brasil", "Portuguese (Brazil)", "pt-BR"},
		{"spa", "Español (Latinoamérica)", "Spanish (Latin America)", "es-419"},
		{"spa", "Español (España)", "Spanish (Spain)", "es-ES"},
		{"ger", "Deutsch (Österreich)", "German (Austria)", "de-AT"},
		{"spa", "Latinoamerica", "Spanish (Latin America)", "es-419"},
		// The script the language is written in anyway, or one CLDR names
		// as a language, is a word, as chino-web reads it; another script
		// is the script.
		{"spa", "Spanish (Latin)", "Spanish · Latin", "spa"},
		{"spa", "Español (Latin)", "Spanish · Latin", "spa"},
		{"eng", "English (Latin)", "English · Latin", "eng"},
		{"eng", "English (Korean)", "English · Korean", "eng"},
		{"hin", "Hindi (Latin)", "Hindi (Latin)", "hi-Latn"},
		{"jpn", "Japanese (Latin)", "Japanese (Latin)", "ja-Latn"},
		// A name of the regional form, or its adjective.
		{"por", "Brazilian Portuguese", "Portuguese (Brazil)", "pt-BR"},
		{"por", "European Portuguese", "Portuguese (Portugal)", "pt-PT"},
		{"spa", "Latin American Spanish", "Spanish (Latin America)", "es-419"},
		{"spa", "Español latino", "Spanish (Latin America)", "es-419"},
		{"fre", "Français canadien", "French (Canada)", "fr-CA"},
		{"dut", "Flemish", "Dutch (Belgium)", "nl-BE"},
		{"por", "Portuguese (Brazilian)", "Portuguese (Brazil)", "pt-BR"},
		{"eng", "English [American]", "English (United States)", "en-US"},
		{"ger", "German (Austrian)", "German (Austria)", "de-AT"},
		{"por", "Portuguese - Brazilian", "Portuguese (Brazil)", "pt-BR"},
		// A Chinese title's script.
		{"chi", "简体中文", "Chinese (Simplified)", "zh-Hans"},
		{"chi", "CHS", "Chinese (Simplified)", "zh-Hans"},
		{"chi", "Simplified Chinese", "Chinese (Simplified)", "zh-Hans"},
		{"zho", "繁體中文", "Chinese (Traditional)", "zh-Hant"},
		{"chi", "繁體中文（台灣）", "Chinese (Traditional, Taiwan)", "zh-Hant-TW"},
		{"chi", "中文", "Chinese", "chi"},
		// What the title says besides stays, after the name.
		{"por", "PT-BR (Forced)", "Portuguese (Brazil) · Forced", "pt-BR"},
		{"por", "PT-BR Commentary", "Portuguese (Brazil) · Commentary", "pt-BR"},
		{"zh-Hans", "Chinese (Simplified) · Converted", "Chinese (Simplified) · Converted", "zh-Hans"},
		{"zh-Hans", "AI", "Chinese (Simplified) · AI", "zh-Hans"},
		// A title that names the source's format: its words go, the script
		// and the region it names stay.
		{"por", "Português (Brasil) - Dolby Digital 5.1", "Portuguese (Brazil)", "pt-BR"},
		{"por", "Português (Portugal) - Dolby Digital 5.1", "Portuguese (Portugal)", "pt-PT"},
		{"por", "PT-BR AC3 5.1", "Portuguese (Brazil)", "pt-BR"},
		{"spa", "Español Latino AC3 5.1", "Spanish (Latin America)", "es-419"},
		{"spa", "Spanish (Latin America) DD 5.1", "Spanish (Latin America)", "es-419"},
		{"chi", "简体中文 AAC", "Chinese (Simplified)", "zh-Hans"},
		{"por", "Commentary AC3", "Portuguese", "por"},
		{"eng", "DTS-HD MA 5.1", "English", "eng"},
		// The tag's own script or region again says nothing more.
		{"pt-BR", "PT-BR", "Portuguese (Brazil)", "pt-BR"},
		{"pt-br", "Português", "Portuguese (Brazil)", "pt-br"},
		{"zh-Hans", "Chinese (Simplified) · 中文", "Chinese (Simplified)", "zh-Hans"},
		{"en-GB", "English", "English (United Kingdom)", "en-GB"},
		{"es-419", "Español (Latinoamérica)", "Spanish (Latin America)", "es-419"},
		{"es-Latn", "Spanish (Latin)", "Spanish (Latin)", "es-Latn"},
		{"en-Kore", "English (Korean)", "English (Korean)", "en-Kore"},
		// A code of the language alone is the language.
		{"por", "pt", "Portuguese", "por"},
		{"eng", "eng", "English", "eng"},
		// Another script or region than the tag's stays, as the title says
		// it; a region among other words is a word.
		{"pt-PT", "PT-BR", "Portuguese (Portugal) · PT-BR", "pt-PT"},
		{"zh-Hant", "zh-Hans", "Chinese (Traditional) · zh-Hans", "zh-Hant"},
		{"zh-Hans", "繁體中文", "Chinese (Simplified) · 繁體中文", "zh-Hans"},
		{"es-ES", "Latin American Spanish", "Spanish (Spain) · Latin American Spanish", "es-ES"},
		{"pt-PT", "Portuguese (Brazilian)", "Portuguese (Portugal) · Brazilian", "pt-PT"},
		{"fre", "Canada Commentaire", "French · Canada Commentaire", "fre"},
		{"por", "Brazilian Commentary", "Portuguese · Brazilian Commentary", "por"},
		// Another language's code is something else; a track in no
		// language is called what its title says.
		{"eng", "de", "English · de", "eng"},
		{"und", "pt-BR", "pt-BR", "und"},
		// The language's own name before a region's: Finnish, not Finland.
		{"fin", "Suomi", "Finnish", "fin"},
		{"ger", "German · Deutsch", "German", "ger"},
	}
	for _, tc := range cases {
		if got := trackDisplayName(tc.lang, tc.title); got != tc.name {
			t.Errorf("%s titled %q: %q, want %q", tc.lang, tc.title, got, tc.name)
		}
		if got := trackTag(tc.lang, tc.title); got != tc.tag {
			t.Errorf("%s titled %q: tag %q, want %q", tc.lang, tc.title, got, tc.tag)
		}
	}
	// A subtitle's too, its forced flag after.
	if got := subtitleDisplayName("chi", "zh-Hans", true); got != "Chinese (Simplified) (forced)" {
		t.Errorf("forced zh-Hans: %q", got)
	}
}

// Two tracks of one language in two regions are two names, not one
// numbered.
func TestTwoRegionsOfALanguageAreTwoNames(t *testing.T) {
	got := audioRenditionNames([]TrackInfo{
		{Index: 0, Language: "por", Title: "PT-BR"},
		{Index: 1, Language: "por", Title: "PT-PT"},
		{Index: 2, Language: "spa", Title: "ES-419"},
		{Index: 3, Language: "spa"},
	})
	if want := []string{"Portuguese (Brazil)", "Portuguese (Portugal)", "Spanish (Latin America)", "Spanish"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NAMEs %q, want %q", got, want)
	}
	// Titled with the source's format besides.
	got = audioRenditionNames([]TrackInfo{
		{Index: 0, Language: "por", Title: "Português (Brasil) - Dolby Digital 5.1"},
		{Index: 1, Language: "por", Title: "Português (Portugal) - Dolby Digital 5.1"},
	})
	if want := []string{"Portuguese (Brazil)", "Portuguese (Portugal)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NAMEs %q, want %q", got, want)
	}
}

// On the fly, /play/info names a source's tracks with the script and the
// region their titles name, and tags them so; the master's NAMEs read the
// same, its LANGUAGE the source's own.
func TestPlayInfoKeepsATitlesRegionOrScript(t *testing.T) {
	packages := usePackages(t, t.TempDir())
	probe := `{"streams":[` +
		`{"codec_type":"video","codec_name":"h264","width":1280,"height":720},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":6,"tags":{"language":"por","title":"PT-BR"},"disposition":{"default":1}},` +
		`{"codec_type":"audio","codec_name":"ac3","channels":2,"tags":{"language":"por","title":"Português (PT)"}},` +
		`{"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"spa","title":"ES-419"}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"chi","title":"zh-Hans"}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"chi","title":"繁體中文"}},` +
		`{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"pt-PT","title":"PT-BR"}}` +
		`],"format":{"format_name":"matroska,webm","duration":"600.0","bit_rate":"4000000"}}`
	root, src := mediaFile(t, "film.mkv")
	h := &Handler{Catalog: fakeKatalog(t, src), Packages: packages, MediaRoot: root, FFprobeBin: probeFFprobe(t, probe)}
	_, audio, subs := infoBody(t, h, "i1")
	languages := func(tracks []map[string]any) []string {
		var out []string
		for _, tr := range tracks {
			l, _ := tr["language"].(string)
			out = append(out, l)
		}
		return out
	}
	if got, want := names(t, audio), []string{"Portuguese (Brazil)", "Portuguese (Portugal)", "Spanish (Latin America)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio names %q, want %q", got, want)
	}
	if got, want := languages(audio), []string{"pt-BR", "pt-PT", "es-419"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio languages %q, want %q", got, want)
	}
	if got, want := names(t, subs), []string{"Chinese (Simplified)", "Chinese (Traditional)", "Portuguese (Portugal) · PT-BR"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtitle names %q, want %q", got, want)
	}
	if got, want := languages(subs), []string{"zh-Hans", "zh-Hant", "pt-pt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtitle languages %q, want %q", got, want)
	}

	captureLog(t)
	// The master starts a warm in the background; its ffmpeg fails at once,
	// and its cache lives outside the test's temp dirs so the warm cannot
	// race their cleanup.
	cache, err := os.MkdirTemp("", "chino-stream-master-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	hls := &HLSHandler{Catalog: h.Catalog, MediaRoot: root, FFmpegBin: fakeBin(t, "ffmpeg", "exit 1"), FFprobeBin: h.FFprobeBin, CacheDir: cache}
	w := get(hls, "/api/play/i1/master.m3u8?caps=avc,aac")
	for _, s := range []string{
		`NAME="Portuguese (Brazil)",LANGUAGE="por",DEFAULT=YES`,
		`NAME="Portuguese (Portugal)",LANGUAGE="por",DEFAULT=NO`,
		`NAME="Spanish (Latin America)",LANGUAGE="spa",DEFAULT=NO`,
	} {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("master lacks %s:\n%s", s, w.Body)
		}
	}
}

// A package's tracks: a name the manifest lacks by the same rule, the
// packager's as it wrote it; the language tagged with what the source's
// title names either way.
func TestPlayInfoTagsAPackagesTracksWithTheirTitlesRegion(t *testing.T) {
	_, packages := copyPackage(t, pkgSingle, func(m map[string]any) {
		audio := m["renditions"].(map[string]any)["audio"].([]any)
		a0, a1 := audio[0].(map[string]any), audio[1].(map[string]any)
		a0["language"], a0["title"], a0["name"] = "por", "PT-BR", ""
		a1["language"], a1["title"], a1["name"] = "por", "Português (PT)", "Portuguese"
		s0 := m["subtitles"].([]any)[0].(map[string]any)
		s0["language"], s0["title"], s0["name"] = "chi", "zh-Hant", ""
	})
	root, src := mediaFile(t, "film.mkv")
	_, audio, subs := infoBody(t, &Handler{Catalog: fakeKatalog(t, src), Packages: packages, MediaRoot: root}, pkgSingle)
	if got, want := names(t, audio), []string{"Portuguese (Brazil)", "Portuguese"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio names %q, want %q", got, want)
	}
	if audio[0]["language"] != "pt-BR" || audio[1]["language"] != "pt-PT" {
		t.Errorf("audio languages %v, %v", audio[0]["language"], audio[1]["language"])
	}
	if subs[0]["name"] != "Chinese (Traditional)" || subs[0]["language"] != "zh-Hant" {
		t.Errorf("first subtitle %v", subs[0])
	}
}
