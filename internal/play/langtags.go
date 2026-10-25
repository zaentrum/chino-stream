package play

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// A track's language with its script and its region. A track is named by
// its language, with the script and the region its tag names in brackets
// after it, as the platforms name a language in their standard form and
// chino-web's lib/languages.ts names a track: "Portuguese (Brazil)",
// "Chinese (Simplified)", "Spanish (Latin America)", "Chinese (Traditional,
// Taiwan)" (docs/chino, subtitles.md). A title that names a script or a
// region of the track's language that its tag does not names the track so,
// and /play/info tags it so ("pt-BR"; a master's LANGUAGE stays the file's
// code): a code of the language ("PT-BR", "pt_BR", "zh-Hans", "ES-419"),
// alone or in brackets ("Commentary (PT-BR)"), one of its regions' codes in
// brackets ("Português (BR)", "Español (419)", "SDH (US)"), the region's or
// the script's name in brackets ("Portuguese (Brazil)", "Chinese
// (Simplified)", "Mandarin (Simplified)"; not the script the language is
// written in anyway, "Spanish (Latin)", nor one named as a language is,
// "English (Korean)"), a name of the regional form ("Brazilian Portuguese",
// "Español latino"), a Chinese title's script ("简体中文"). A bracket that
// says no more than that goes wherever it stands, as in chino-web's
// readParts. A title that names another script or region than the tag's
// says something, and stays ("Portuguese (Portugal) · PT-BR"); one that
// names the tag's own again says nothing more ("PT-BR" on a pt-BR track).

// langTag is a language code read as BCP 47 (RFC 5646) reads it: its
// language subtag, lower-cased, its script, title-cased, and its region,
// upper-cased and canonical ("UK" is "GB": regionAliases). A variant, an
// extension or a private use names nothing a name says ("de-1901" is "de").
type langTag struct{ lang, script, region string }

// parseTag reads code ("pt-BR", "PT_br", "zh-Hant-TW", "por") as a langTag.
func parseTag(code string) langTag {
	subs := strings.Split(strings.ReplaceAll(strings.TrimSpace(code), "_", "-"), "-")
	t := langTag{lang: strings.ToLower(subs[0])}
	for _, sub := range subs[1:] {
		switch {
		case len(sub) == 1: // an extension or a private use: nothing named past it
			return t
		case t.script == "" && t.region == "" && len(sub) == 4 && allLetters(sub):
			t.script = strings.ToUpper(sub[:1]) + strings.ToLower(sub[1:])
		case t.region == "" && (len(sub) == 2 && allLetters(sub) || len(sub) == 3 && allDigits(sub)):
			t.region = canonicalRegion(sub)
		}
	}
	return t
}

// String is the tag as BCP 47 writes it: the language by its ISO 639-1 code
// where it has one ("pt" for "por"), then its script and its region
// ("pt-BR", "zh-Hant-TW", "es-419").
func (t langTag) String() string {
	parts := []string{iso6391(t.lang)}
	for _, s := range []string{t.script, t.region} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

// qualifier is what a language's name says of t in brackets after it: its
// script's name and its region's, those the table has ("Simplified",
// "Brazil", "Traditional, Taiwan"); "" for neither.
func (t langTag) qualifier() string {
	var parts []string
	if n := scriptNames[t.script]; n != "" {
		parts = append(parts, n)
	}
	if n := regionNames[t.region]; n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, ", ")
}

// canonicalRegion is a region's code upper-cased, a replaced one by the code
// that replaces it ("UK" is "GB").
func canonicalRegion(code string) string {
	code = strings.ToUpper(code)
	if a, ok := regionAliases[code]; ok {
		return a
	}
	return code
}

func allLetters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return s != ""
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// twoLetterCodes are the ISO 639-1 codes of the languages languageNames has,
// by their names.
var twoLetterCodes = func() map[string]string {
	m := map[string]string{}
	for code, name := range languageNames {
		if len(code) == 2 {
			m[name] = code
		}
	}
	return m
}()

// iso6391 is a language's ISO 639-1 code ("pt" for "por", "pt" or
// "PT"), the code lower-cased where it has none or the table does not
// know it ("gsw").
func iso6391(code string) string {
	code = strings.ToLower(code)
	if two, ok := twoLetterCodes[languageNames[code]]; ok {
		return two
	}
	return code
}

// sameLanguage reports whether two codes name one language ("PT-BR" and
// "por"; "fil" and "fil" where the table has no name for it).
func sameLanguage(a, b string) bool {
	pa, pb := primarySubtag(a), primarySubtag(b)
	if pa == "" || pb == "" {
		return false
	}
	if name, ok := languageNames[pa]; ok {
		return name == languageNames[pb]
	}
	return pa == pb
}

// regionsByName and scriptsByName are the regions and the scripts by their
// names (folded): a region's long name and its short one ("united kingdom",
// "uk").
var (
	regionsByName = func() map[string]string {
		m := map[string]string{}
		for code, name := range regionNames {
			m[folded(name)] = code
		}
		for code, name := range regionShortNames {
			m[folded(name)] = code
		}
		return m
	}()
	scriptsByName = func() map[string]string {
		m := map[string]string{}
		for code, name := range scriptNames {
			m[folded(name)] = code
		}
		return m
	}()
)

// chineseHeads are the words of chineseScripts, the longest first: what a
// Chinese track's title may begin with ("简体中文 · SDH").
var chineseHeads = func() []string {
	heads := make([]string, 0, len(chineseScripts))
	for w := range chineseScripts {
		heads = append(heads, w)
	}
	slices.SortFunc(heads, func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return heads
}()

// accents are the accented letters a title may give a name with, and the
// letter each is without its accent.
var accents = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ā", "a", "ă", "a", "ą", "a",
	"ç", "c", "č", "c", "ć", "c", "ď", "d", "è", "e", "é", "e", "ê", "e", "ë", "e", "ē", "e", "ě", "e", "ę", "e",
	"ğ", "g", "ì", "i", "í", "i", "î", "i", "ï", "i", "ī", "i", "ı", "i", "ł", "l", "ñ", "n", "ń", "n", "ň", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "ō", "o", "ő", "o", "ř", "r",
	"ś", "s", "š", "s", "ş", "s", "ș", "s", "ť", "t", "ţ", "t", "ț", "t",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ū", "u", "ů", "u", "ű", "u", "ý", "y", "ÿ", "y", "ź", "z", "ż", "z", "ž", "z",
	"’", "'",
)

// folded is text lower-cased, without its accents and its outer spaces:
// "Latinoamérica" is "latinoamerica", as the tables' words are written.
func folded(text string) string {
	return accents.Replace(strings.ToLower(strings.TrimSpace(text)))
}

var (
	// bracketedRegion is a region's code as a title gives it in brackets: two
	// capitals, or the three digits of one of the world's regions ("419").
	bracketedRegion = regexp.MustCompile(`^(?:[A-Z]{2}|[0-9]{3})$`)
	// codeHead is a language code at the start of a title ("PT-BR (Forced)").
	codeHead = regexp.MustCompile(`^[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]{1,8})*`)
)

// brackets are the brackets a title's words may stand in: round, square,
// and the full-width round ones of a Chinese or Japanese title.
var brackets = map[rune]rune{'(': ')', '[': ']', '（': '）'}

// leadingBracket splits words that begin with a bracket into what it holds
// and what follows it; false when they begin with none, or it is not closed.
func leadingBracket(words string) (inner, after string, ok bool) {
	open, size := utf8.DecodeRuneInString(words)
	closing, isOpen := brackets[open]
	if !isOpen {
		return "", "", false
	}
	end := strings.IndexRune(words[size:], closing)
	if end < 0 {
		return "", "", false
	}
	return words[size : size+end], words[size+end+utf8.RuneLen(closing):], true
}

// titleSays is what a track's title says besides its language - its words
// ("Commentary"), "" for none - and the script and the region of the
// language it names that the track's tag does not.
type titleSays struct {
	words          string
	script, region string
}

// tag is the track's language code with the script and the region its
// title names besides the tag's: language as it is where it names none
// ("por"), else as BCP 47 writes it ("pt-BR").
func (s titleSays) tag(language string) string {
	if s.script == "" && s.region == "" {
		return language
	}
	t := parseTag(language)
	if t.script == "" {
		t.script = s.script
	}
	if t.region == "" {
		t.region = s.region
	}
	return t.String()
}

// titleReader reads a track's title for what it says of the track's
// language.
type titleReader struct {
	// tag is the track's, with the script and the region the title has
	// named so far; added those of them the tag did not name.
	tag, added langTag
	// one: the tag names one language.
	one bool
}

// says is what the title said: words, and the script and the region it
// named.
func (r *titleReader) says(words string) titleSays {
	return titleSays{words: words, script: r.added.script, region: r.added.region}
}

// takes the script and the region a piece of the title names, where both
// agree with the tag's and with what the title named before: true then (the
// piece says the language and no more), false where either is another.
func (r *titleReader) takes(script, region string) bool {
	if script != "" && r.tag.script != "" && !strings.EqualFold(script, r.tag.script) ||
		region != "" && r.tag.region != "" && region != r.tag.region {
		return false
	}
	if script != "" && r.tag.script == "" {
		r.tag.script, r.added.script = script, script
	}
	if region != "" && r.tag.region == "" {
		r.tag.region, r.added.region = region, region
	}
	return true
}

// names reports whether a piece of a title - all of it, what follows the
// language's name at its start, a bracket in it (bracketed) - says
// nothing but the track's language, and takes the script and the region
// it names (takes): a code that names no language ("und"), or of the
// track's language ("pt", "PT-BR", "zh-Hans"); in brackets a region's code
// that is one of the language's regions ("BR" of "Português (BR)",
// languageRegions) or a script's name ("Simplified"); the language's name
// in English or in itself ("Deutsch"); a region's English name, long or
// short ("Brazil", "Latin America", "UK"), with a script's before it
// ("Traditional, Taiwan"); a Chinese title's script ("简体中文", "CHS"); a
// word for a regional form of the language or one of its regions' names in
// itself ("Brazilian", "latino", "Flemish", "Brasil", "Latinoamérica").
func (r *titleReader) names(piece string, bracketed bool) bool {
	t := strings.TrimSpace(piece)
	if t == "" {
		return false
	}
	code := codeTitle.MatchString(t)
	if code && notOneLanguage[primarySubtag(t)] {
		return true
	}
	if !r.one {
		return false
	}
	if bracketed && bracketedRegion.MatchString(t) && slices.Contains(languageRegions[iso6391(r.tag.lang)], canonicalRegion(t)) {
		return r.takes("", canonicalRegion(t))
	}
	if code && sameLanguage(t, r.tag.lang) {
		c := parseTag(t)
		return r.takes(c.script, c.region)
	}
	// Its name before a region's: "Suomi" is Finnish, not Finland.
	if name := languageNames[r.tag.lang]; name != "" && (strings.EqualFold(t, name) || endonyms[name] != "" && strings.EqualFold(t, endonyms[name])) {
		return true
	}
	f := folded(t)
	if script, region, ok := qualifierSays(f, bracketed, r.tag); ok {
		return r.takes(script, region)
	}
	if script := chineseScripts[f]; script != "" && languageNames[r.tag.lang] == "Chinese" {
		return r.takes(script, "")
	}
	if region := regionWords[iso6391(r.tag.lang)][f]; region != "" {
		return r.takes("", region)
	}
	return false
}

// qualifierSays reads words (folded) as the name of tag's language says a
// script and a region in brackets after it: a region's name ("brazil",
// "latin america", "uk"), a script's and a region's ("traditional,
// taiwan"), and in brackets a script's alone ("simplified"; "Korean" alone
// is a language) - not, unless tag names it, a script that CLDR names as it
// names a language ("korean", "greek": languageNamedScripts), nor the one
// the language is written in anyway ("latin" after Spanish: usualScripts).
func qualifierSays(words string, bracketed bool, tag langTag) (script, region string, ok bool) {
	if s, g, two := strings.Cut(words, ","); two {
		script, region = scriptsByName[strings.TrimSpace(s)], regionsByName[strings.TrimSpace(g)]
		return script, region, script != "" && region != ""
	}
	if region = regionsByName[words]; region != "" {
		return "", region, true
	}
	if script = scriptsByName[words]; bracketed && script != "" &&
		(script == tag.script || !languageNamedScripts[script] && script != usualScripts[iso6391(tag.lang)]) {
		return script, "", true
	}
	return "", "", false
}

// headEnds reports whether what follows a title's first words lets them be
// a name of their own: nothing, a space, a separator or a bracket - not
// "Englishman".
func headEnds(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	_, open := brackets[r]
	return open || strings.ContainsRune(separators, r)
}

// afterHead is a title after the track's language at its start - its name
// in English or in itself ("English Commentary", "Deutsch - Kommentar"), a
// code of it ("PT-BR (Forced)"), a Chinese title's script ("简体中文 ·
// SDH") - taking what that names (names); false where it does not begin
// with the language, or a code or a script of it the tag disagrees with.
func (r *titleReader) afterHead(words string) (string, bool) {
	rest := func(n int) string { return strings.TrimSpace(strings.TrimLeft(words[n:], separators)) }
	name := languageNames[r.tag.lang]
	heads := []string{name, endonyms[name]}
	if name == "Chinese" {
		heads = append(heads, chineseHeads...)
	}
	for _, said := range heads {
		if said == "" || len(words) < len(said) || !strings.EqualFold(words[:len(said)], said) || !headEnds(words[len(said):]) {
			continue
		}
		if said == name || said == endonyms[name] || r.names(said, false) {
			return rest(len(said)), true
		}
	}
	if code := codeHead.FindString(words); code != "" && headEnds(words[len(code):]) &&
		codeTitle.MatchString(code) && sameLanguage(code, r.tag.lang) && r.names(code, false) {
		return rest(len(code)), true
	}
	return "", false
}

var (
	// titleBrackets are the brackets in a title, the innermost: round,
	// square, and the full-width round ones of a Chinese or Japanese title.
	titleBrackets = regexp.MustCompile(`\([^()]*\)|\[[^\[\]]*\]|（[^（）]*）`)
	// doubledSeparators are what a bracket taken from between two
	// separators leaves ("Commentary - - Director").
	doubledSeparators = regexp.MustCompile(`( [-–—:·,|/]+)(?: [-–—:·,|/]+)+ `)
)

// withoutBrackets is words without the brackets in them, wherever they
// stand, that say nothing but the language (names, bracketed), as
// chino-web's readParts reads every bracket of a title: "Commentary" of
// "Commentary (PT-BR)" and "" of "[PT-BR]" on a Portuguese track, taking
// "BR", "Converted" of "(Simplified) · Converted" and "Mandarin" of
// "Mandarin (Simplified)" on a Chinese one, taking "Hans"; false where it
// takes none.
func (r *titleReader) withoutBrackets(words string) (string, bool) {
	took := false
	rest := titleBrackets.ReplaceAllStringFunc(words, func(b string) string {
		_, open := utf8.DecodeRuneInString(b)
		_, closing := utf8.DecodeLastRuneInString(b)
		if !r.names(b[open:len(b)-closing], true) {
			return b
		}
		took = true
		return " "
	})
	if !took {
		return words, false
	}
	rest = strings.Join(strings.Fields(emptyBrackets.ReplaceAllString(rest, "")), " ")
	rest = doubledSeparators.ReplaceAllString(rest, "$1 ")
	return strings.Trim(rest, separators), true
}
