package play

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a track is called - the NAME of an on-the-fly audio rendition and the
// name /play/info gives a track - by the rule the packager names its
// renditions with (packager.py _track_display_name) and the clients label
// tracks with (chino-web's lib/languages.ts): the name of the track's
// language, then what its source title says besides ("English ·
// Commentary"). A track in no known language is called what its title says,
// else "Unknown"; "zxx" is "No dialogue". The source's title as it is is no
// name: it is free text, often the source's codec ("AC3 5.1 @ 640 Kbps"),
// wrong anyway once the track is stereo AAC.

// audioRenditionNames are the NAMEs of the master's audio renditions, in
// track order: each track's audioRenditionName, unique in the group as RFC
// 8216 wants them.
func audioRenditionNames(tracks []TrackInfo) []string {
	names := make([]string, len(tracks))
	for i, t := range tracks {
		names[i] = audioRenditionName(t)
	}
	return uniqueNames(names)
}

// audioRenditionName is what a player lists an on-the-fly audio rendition
// as: trackDisplayName of its language and title.
func audioRenditionName(t TrackInfo) string {
	return trackDisplayName(t.Language, t.Title)
}

// uniqueNames are names, a second and later one that reads the same
// numbered: a second "English" is "English (2)".
func uniqueNames(names []string) []string {
	out := make([]string, len(names))
	used := make(map[string]bool, len(names))
	for i, base := range names {
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		used[name] = true
		out[i] = name
	}
	return out
}

// trackDisplayName is a track's name: the name of its language, then what
// its title says besides ("English · Commentary"); a track in no known
// language what its title says, else "Unknown"; one without dialogue "No
// dialogue".
func trackDisplayName(language, title string) string {
	words := titleWords(title, language)
	if p := primarySubtag(language); (p == "" || p == "und") && words != "" {
		return words
	}
	if words != "" {
		return langDisplay(language) + " · " + words
	}
	return langDisplay(language)
}

// subtitleDisplayName is a subtitle track's name: trackDisplayName, and
// "(forced)" for a forced track whose name does not say so ("English
// (forced)"; "English · Forced" when its title did).
func subtitleDisplayName(language, title string, forced bool) string {
	name := trackDisplayName(language, title)
	if forced && !strings.Contains(strings.ToLower(name), "forced") {
		name += " (forced)"
	}
	return name
}

var (
	// formatWords mark a title that describes the source's audio format -
	// a codec, a bitrate, a sample rate or depth ("AC3 5.1 @ 640 Kbps",
	// "DTS-HD MA 5.1"). It says nothing of the track.
	formatWords = regexp.MustCompile(`(?i)(^|[^a-z0-9])(dts(-hd)?|truehd|atmos|dolby|e?-?ac-?3|ddp?\+?|aac|flac|l?pcm|opus|mp3|vorbis|lossless|master audio|\d+ ?k?hz|\d* ?[km]bps|kb/s|\d+[- ]?bit)($|[^a-z0-9])`)
	// layoutWords are a channel layout, which goes from a title that says
	// more ("Commentary 5.1" is "Commentary"): the rendition is stereo.
	layoutWords = regexp.MustCompile(`(?i)(^|[^a-z0-9.])(?:mono|stereo|surround|[1-9]\.[0-2]|\d{1,2} ?ch(?:annels?)?)($|[^a-z0-9.])`)
	// emptyBrackets are what a layout in brackets leaves ("English (5.1)").
	emptyBrackets = regexp.MustCompile(`\(\s*\)|\[\s*\]`)
	// numberedTitle only numbers the track ("Track 2", "Audio Track 1",
	// "Audio", "2").
	numberedTitle = regexp.MustCompile(`(?i)^(?:audio|sound|track|stream|[\s#])*\d*$`)
	// codeTitle is a language code and no more ("eng", "en-US").
	codeTitle = regexp.MustCompile(`(?i)^[a-z]{2,3}([-_][a-z0-9]{1,8})*$`)
)

// titleWordsMax is how long what a title adds to a name may be, in
// characters; longer is cut at a word, as the packager cuts it.
const titleWordsMax = 40

// separators are what stands between a language's name and what a title
// says besides it ("English - Commentary", "English: SDH").
const separators = " \t-–—:·,|/"

// titleWords is what a track's title says besides its language:
// "Commentary", "SDH" ("English (SDH)" on an English track), "Signs &
// Songs"; "" when it says nothing - no title, a format, a number, the
// track's language by its name, its own name for it ("Deutsch") or its code,
// a code that names no language.
func titleWords(title, language string) string {
	raw := strings.TrimSpace(title)
	if raw == "" || formatWords.MatchString(raw) {
		return ""
	}
	words := raw
	for layoutWords.MatchString(words) {
		words = layoutWords.ReplaceAllString(words, "${1}${2}")
	}
	words = emptyBrackets.ReplaceAllString(words, "")
	words = strings.Trim(strings.Join(strings.Fields(words), " "), separators)
	if words == "" || numberedTitle.MatchString(words) || isLanguageCode(words, language) {
		return ""
	}
	if hasOneLanguage(language) {
		name := langDisplay(language)
		for _, said := range []string{name, endonyms[name]} {
			if said == "" || len(words) < len(said) || !strings.EqualFold(words[:len(said)], said) {
				continue
			}
			// The name, then a separator or a bracket ("English (SDH)"), or
			// nothing: not "Englishman".
			rest := words[len(said):]
			if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !strings.ContainsRune(separators+"([", r) {
				continue
			}
			words = strings.TrimSpace(strings.TrimLeft(rest, separators))
			if len(words) >= 2 && (words[0] == '(' && words[len(words)-1] == ')' || words[0] == '[' && words[len(words)-1] == ']') {
				words = strings.TrimSpace(words[1 : len(words)-1])
			}
			break
		}
	}
	if words == "" || numberedTitle.MatchString(words) {
		return ""
	}
	if r := []rune(words); len(r) > titleWordsMax {
		cut := string(r[:titleWordsMax+1])
		if i := strings.LastIndex(cut, " "); i >= 0 {
			cut = cut[:i]
		}
		if cut == "" {
			cut = string(r[:titleWordsMax])
		}
		if cr := []rune(cut); len(cr) > titleWordsMax {
			cut = string(cr[:titleWordsMax])
		}
		words = strings.Trim(cut, separators) + "…"
	}
	return words
}

// isLanguageCode reports a title that is only a language code: its track's
// own ("eng" or "en" on an English track) or one that names no language
// ("und").
func isLanguageCode(title, language string) bool {
	if !codeTitle.MatchString(title) {
		return false
	}
	return notOneLanguage[primarySubtag(title)] || langDisplay(title) == langDisplay(language)
}

// notOneLanguage are the codes that name no one language to follow:
// undetermined, no linguistic content, several, one with no code.
var notOneLanguage = map[string]bool{"und": true, "zxx": true, "mul": true, "mis": true}

// hasOneLanguage reports a code that names one language.
func hasOneLanguage(code string) bool {
	p := primarySubtag(code)
	return p != "" && !notOneLanguage[p]
}

// primarySubtag is the code's language subtag, lower-cased ("pt" of
// "PT_br").
func primarySubtag(code string) string {
	p, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(code)), "-")
	p, _, _ = strings.Cut(p, "_")
	return p
}

// languageNames are the English names of the languages a library carries,
// by ISO 639-2/T, 639-2/B and 639-1 code.
var languageNames = map[string]string{
	"eng": "English", "en": "English",
	"deu": "German", "ger": "German", "de": "German",
	"fra": "French", "fre": "French", "fr": "French",
	"spa": "Spanish", "es": "Spanish",
	"ita": "Italian", "it": "Italian",
	"jpn": "Japanese", "ja": "Japanese",
	"zho": "Chinese", "chi": "Chinese", "zh": "Chinese",
	"por": "Portuguese", "pt": "Portuguese",
	"rus": "Russian", "ru": "Russian",
	"nld": "Dutch", "dut": "Dutch", "nl": "Dutch",
	"kor": "Korean", "ko": "Korean",
	"pol": "Polish", "pl": "Polish",
	"swe": "Swedish", "sv": "Swedish",
	"nor": "Norwegian", "no": "Norwegian",
	"dan": "Danish", "da": "Danish",
	"fin": "Finnish", "fi": "Finnish",
	"tur": "Turkish", "tr": "Turkish",
	"ces": "Czech", "cze": "Czech", "cs": "Czech",
	"ell": "Greek", "gre": "Greek", "el": "Greek",
	"hun": "Hungarian", "hu": "Hungarian",
	"ron": "Romanian", "rum": "Romanian", "ro": "Romanian",
	"heb": "Hebrew", "he": "Hebrew",
	"ara": "Arabic", "ar": "Arabic",
	"hin": "Hindi", "hi": "Hindi",
	"tha": "Thai", "th": "Thai",
	"vie": "Vietnamese", "vi": "Vietnamese",
	"ukr": "Ukrainian", "uk": "Ukrainian",
	"gsw": "Swiss German",
}

// endonyms are what a language is called in that language, where a source's
// title is likely to say it so ("Deutsch" on a German track), as the
// packager has them.
var endonyms = map[string]string{
	"Arabic": "العربية", "Chinese": "中文", "Czech": "Čeština", "Danish": "Dansk",
	"Dutch": "Nederlands", "Finnish": "Suomi", "French": "Français", "German": "Deutsch",
	"Greek": "Ελληνικά", "Hebrew": "עברית", "Hindi": "हिन्दी", "Hungarian": "Magyar",
	"Italian": "Italiano", "Japanese": "日本語", "Korean": "한국어", "Norwegian": "Norsk",
	"Polish": "Polski", "Portuguese": "Português", "Romanian": "Română", "Russian": "Русский",
	"Spanish": "Español", "Swedish": "Svenska", "Thai": "ไทย", "Turkish": "Türkçe",
	"Ukrainian": "Українська", "Vietnamese": "Tiếng Việt",
}

// langDisplay names an ISO 639 code in English ("eng", "en", "en-US":
// "English"). "zxx" - no linguistic content, a film without dialogue - is
// "No dialogue", "mul" "Multiple languages", "mis" (a language with no code)
// "Other language"; "und" or none is "Unknown". A code it has no name for
// comes back as it is.
func langDisplay(code string) string {
	primary := primarySubtag(code)
	switch primary {
	case "", "und":
		return "Unknown"
	case "zxx":
		return "No dialogue"
	case "mul":
		return "Multiple languages"
	case "mis":
		return "Other language"
	}
	if name, ok := languageNames[primary]; ok {
		return name
	}
	return code
}
