package play

// The names a language tag's script and region are given in, and the words
// a track's title may give them in: CLDR's English names, as the platforms
// name them (Intl.DisplayNames on the web, ICU on Android) and so as
// chino-web's lib/languages.ts names a track - "Portuguese (Brazil)",
// "Chinese (Simplified)", "Spanish (Latin America)" (docs/chino,
// subtitles.md, "For the builders").

// regionNames are the regions' names by their ISO 3166 and UN M.49 codes.
var regionNames = map[string]string{
	"001": "world", "002": "Africa", "003": "North America", "005": "South America", "009": "Oceania",
	"011": "Western Africa", "013": "Central America", "014": "Eastern Africa", "015": "Northern Africa",
	"017": "Middle Africa", "018": "Southern Africa", "019": "Americas", "021": "Northern America",
	"029": "Caribbean", "030": "Eastern Asia", "034": "Southern Asia", "035": "Southeast Asia",
	"039": "Southern Europe", "053": "Australasia", "054": "Melanesia", "057": "Micronesian Region",
	"061": "Polynesia", "142": "Asia", "143": "Central Asia", "145": "Western Asia", "150": "Europe",
	"151": "Eastern Europe", "154": "Northern Europe", "155": "Western Europe", "202": "Sub-Saharan Africa",
	"419": "Latin America", "AC": "Ascension Island", "AD": "Andorra", "AE": "United Arab Emirates",
	"AF": "Afghanistan", "AG": "Antigua & Barbuda", "AI": "Anguilla", "AL": "Albania", "AM": "Armenia",
	"AO": "Angola", "AQ": "Antarctica", "AR": "Argentina", "AS": "American Samoa", "AT": "Austria",
	"AU": "Australia", "AW": "Aruba", "AX": "Åland Islands", "AZ": "Azerbaijan", "BA": "Bosnia & Herzegovina",
	"BB": "Barbados", "BD": "Bangladesh", "BE": "Belgium", "BF": "Burkina Faso", "BG": "Bulgaria",
	"BH": "Bahrain", "BI": "Burundi", "BJ": "Benin", "BL": "St. Barthélemy", "BM": "Bermuda", "BN": "Brunei",
	"BO": "Bolivia", "BQ": "Caribbean Netherlands", "BR": "Brazil", "BS": "Bahamas", "BT": "Bhutan",
	"BV": "Bouvet Island", "BW": "Botswana", "BY": "Belarus", "BZ": "Belize", "CA": "Canada",
	"CC": "Cocos (Keeling) Islands", "CD": "Congo - Kinshasa", "CF": "Central African Republic",
	"CG": "Congo - Brazzaville", "CH": "Switzerland", "CI": "Côte d’Ivoire", "CK": "Cook Islands",
	"CL": "Chile", "CM": "Cameroon", "CN": "China", "CO": "Colombia", "CP": "Clipperton Island", "CQ": "Sark",
	"CR": "Costa Rica", "CU": "Cuba", "CV": "Cape Verde", "CW": "Curaçao", "CX": "Christmas Island",
	"CY": "Cyprus", "CZ": "Czechia", "DE": "Germany", "DG": "Diego Garcia", "DJ": "Djibouti", "DK": "Denmark",
	"DM": "Dominica", "DO": "Dominican Republic", "DZ": "Algeria", "EA": "Ceuta & Melilla", "EC": "Ecuador",
	"EE": "Estonia", "EG": "Egypt", "EH": "Western Sahara", "ER": "Eritrea", "ES": "Spain", "ET": "Ethiopia",
	"FI": "Finland", "FJ": "Fiji", "FK": "Falkland Islands", "FM": "Micronesia", "FO": "Faroe Islands",
	"FR": "France", "GA": "Gabon", "GB": "United Kingdom", "GD": "Grenada", "GE": "Georgia",
	"GF": "French Guiana", "GG": "Guernsey", "GH": "Ghana", "GI": "Gibraltar", "GL": "Greenland",
	"GM": "Gambia", "GN": "Guinea", "GP": "Guadeloupe", "GQ": "Equatorial Guinea", "GR": "Greece",
	"GS": "South Georgia & South Sandwich Islands", "GT": "Guatemala", "GU": "Guam", "GW": "Guinea-Bissau",
	"GY": "Guyana", "HK": "Hong Kong SAR China", "HM": "Heard & McDonald Islands", "HN": "Honduras",
	"HR": "Croatia", "HT": "Haiti", "HU": "Hungary", "IC": "Canary Islands", "ID": "Indonesia", "IE": "Ireland",
	"IL": "Israel", "IM": "Isle of Man", "IN": "India", "IO": "British Indian Ocean Territory", "IQ": "Iraq",
	"IR": "Iran", "IS": "Iceland", "IT": "Italy", "JE": "Jersey", "JM": "Jamaica", "JO": "Jordan",
	"JP": "Japan", "KE": "Kenya", "KG": "Kyrgyzstan", "KH": "Cambodia", "KI": "Kiribati", "KM": "Comoros",
	"KN": "St. Kitts & Nevis", "KP": "North Korea", "KR": "South Korea", "KW": "Kuwait", "KY": "Cayman Islands",
	"KZ": "Kazakhstan", "LA": "Laos", "LB": "Lebanon", "LC": "St. Lucia", "LI": "Liechtenstein",
	"LK": "Sri Lanka", "LR": "Liberia", "LS": "Lesotho", "LT": "Lithuania", "LU": "Luxembourg", "LV": "Latvia",
	"LY": "Libya", "MA": "Morocco", "MC": "Monaco", "MD": "Moldova", "ME": "Montenegro", "MF": "St. Martin",
	"MG": "Madagascar", "MH": "Marshall Islands", "MK": "North Macedonia", "ML": "Mali",
	"MM": "Myanmar (Burma)", "MN": "Mongolia", "MO": "Macao SAR China", "MP": "Northern Mariana Islands",
	"MQ": "Martinique", "MR": "Mauritania", "MS": "Montserrat", "MT": "Malta", "MU": "Mauritius",
	"MV": "Maldives", "MW": "Malawi", "MX": "Mexico", "MY": "Malaysia", "MZ": "Mozambique", "NA": "Namibia",
	"NC": "New Caledonia", "NE": "Niger", "NF": "Norfolk Island", "NG": "Nigeria", "NI": "Nicaragua",
	"NL": "Netherlands", "NO": "Norway", "NP": "Nepal", "NR": "Nauru", "NU": "Niue", "NZ": "New Zealand",
	"OM": "Oman", "PA": "Panama", "PE": "Peru", "PF": "French Polynesia", "PG": "Papua New Guinea",
	"PH": "Philippines", "PK": "Pakistan", "PL": "Poland", "PM": "St. Pierre & Miquelon",
	"PN": "Pitcairn Islands", "PR": "Puerto Rico", "PS": "Palestinian Territories", "PT": "Portugal",
	"PW": "Palau", "PY": "Paraguay", "QA": "Qatar", "RE": "Réunion", "RO": "Romania", "RS": "Serbia",
	"RU": "Russia", "RW": "Rwanda", "SA": "Saudi Arabia", "SB": "Solomon Islands", "SC": "Seychelles",
	"SD": "Sudan", "SE": "Sweden", "SG": "Singapore", "SH": "St. Helena", "SI": "Slovenia",
	"SJ": "Svalbard & Jan Mayen", "SK": "Slovakia", "SL": "Sierra Leone", "SM": "San Marino", "SN": "Senegal",
	"SO": "Somalia", "SR": "Suriname", "SS": "South Sudan", "ST": "São Tomé & Príncipe", "SV": "El Salvador",
	"SX": "Sint Maarten", "SY": "Syria", "SZ": "Eswatini", "TA": "Tristan da Cunha",
	"TC": "Turks & Caicos Islands", "TD": "Chad", "TF": "French Southern Territories", "TG": "Togo",
	"TH": "Thailand", "TJ": "Tajikistan", "TK": "Tokelau", "TL": "Timor-Leste", "TM": "Turkmenistan",
	"TN": "Tunisia", "TO": "Tonga", "TR": "Türkiye", "TT": "Trinidad & Tobago", "TV": "Tuvalu", "TW": "Taiwan",
	"TZ": "Tanzania", "UA": "Ukraine", "UG": "Uganda", "UM": "U.S. Outlying Islands", "US": "United States",
	"UY": "Uruguay", "UZ": "Uzbekistan", "VA": "Vatican City", "VC": "St. Vincent & Grenadines",
	"VE": "Venezuela", "VG": "British Virgin Islands", "VI": "U.S. Virgin Islands", "VN": "Vietnam",
	"VU": "Vanuatu", "WF": "Wallis & Futuna", "WS": "Samoa", "XK": "Kosovo", "YE": "Yemen", "YT": "Mayotte",
	"ZA": "South Africa", "ZM": "Zambia", "ZW": "Zimbabwe",
}

// regionShortNames are the short names CLDR gives some regions, which a
// title may say instead ("English (UK)").
var regionShortNames = map[string]string{
	"BA": "Bosnia", "CC": "Cocos Islands", "GB": "UK", "HK": "Hong Kong", "MM": "Myanmar", "MO": "Macao",
	"PN": "Pitcairn", "PS": "Palestine", "US": "US",
}

// scriptNames are the scripts' names by their ISO 15924 codes, as a
// language's name says them ("Chinese (Simplified)").
var scriptNames = map[string]string{
	"Arab": "Arabic", "Armn": "Armenian", "Beng": "Bangla", "Bopo": "Bopomofo", "Cyrl": "Cyrillic",
	"Deva": "Devanagari", "Ethi": "Ethiopic", "Geor": "Georgian", "Grek": "Greek", "Gujr": "Gujarati",
	"Guru": "Gurmukhi", "Hang": "Hangul", "Hani": "Han", "Hans": "Simplified", "Hant": "Traditional",
	"Hebr": "Hebrew", "Hira": "Hiragana", "Jpan": "Japanese", "Kana": "Katakana", "Khmr": "Khmer",
	"Knda": "Kannada", "Kore": "Korean", "Laoo": "Lao", "Latn": "Latin", "Mlym": "Malayalam",
	"Mong": "Mongolian", "Mymr": "Myanmar", "Orya": "Odia", "Sinh": "Sinhala", "Taml": "Tamil",
	"Telu": "Telugu", "Thaa": "Thaana", "Thai": "Thai", "Tibt": "Tibetan",
}

// usualScripts are, by a language's ISO 639-1 code, the script it is
// written in unless a tag names another: the IANA language subtag
// registry's Suppress-Script, which a tag is not to name (RFC 5646, 3.1.9:
// "es", not "es-Latn"). A title that names it in brackets names no script:
// "Spanish (Latin)" means Latin American Spanish, and stays a word, as
// chino-web reads it.
var usualScripts = map[string]string{
	"ar": "Arab", "cs": "Latn", "da": "Latn", "de": "Latn", "el": "Grek", "en": "Latn", "es": "Latn",
	"fi": "Latn", "fr": "Latn", "gsw": "Latn", "he": "Hebr", "hi": "Deva", "hu": "Latn", "it": "Latn",
	"ja": "Jpan", "ko": "Kore", "nl": "Latn", "no": "Latn", "pl": "Latn", "pt": "Latn", "ro": "Latn",
	"ru": "Cyrl", "sv": "Latn", "th": "Thai", "tr": "Latn", "uk": "Cyrl", "vi": "Latn",
}

// languageNamedScripts are the scripts CLDR names as it names a language
// ("Korean", "Greek", "Tamil"). Such a name in a title's brackets is the
// language, not a script: "English (Korean)" is no en-Kore.
var languageNamedScripts = map[string]bool{
	"Arab": true, "Armn": true, "Beng": true, "Geor": true, "Grek": true, "Gujr": true, "Hebr": true,
	"Jpan": true, "Khmr": true, "Knda": true, "Kore": true, "Laoo": true, "Mlym": true, "Mong": true,
	"Orya": true, "Sinh": true, "Taml": true, "Telu": true, "Thai": true, "Tibt": true,
}

// languageRegions are, by a language's ISO 639-1 code, the regions it is
// that region's own language in: where it is the language the region speaks
// most (CLDR's likely subtags: "BR" and "PT" for "pt"), and where CLDR names
// a regional form of it ("CA" for "fr": "Canadian French"). A region's code
// in brackets after a language's name names one of these ("Português
// (BR)"); other capitals there are a word ("English (BD)"), or the
// language's own code ("Svenska (SV)"). chino-web's regionCodesOf.
var languageRegions = map[string][]string{
	"ar": {"AE", "BH", "DZ", "EG", "IQ", "JO", "KW", "LB", "LY", "MA", "OM", "QA", "SA", "SD", "SY", "TN", "YE"},
	"cs": {"CZ"},
	"da": {"DK"},
	"de": {"AT", "CH", "DE", "LI"},
	"el": {"CY", "GR"},
	"en": {"150", "AU", "CA", "GB", "IE", "NG", "NZ", "SG", "US", "ZA"},
	"es": {"419", "AR", "BO", "CL", "CO", "CR", "CU", "DO", "EC", "ES", "GT", "HN", "MX", "NI", "PA", "PE", "PR", "SV", "UY", "VE"},
	"fi": {"FI"},
	"fr": {"CA", "CH", "FR", "LU", "MC"},
	"he": {"IL"},
	"hi": {"IN"},
	"hu": {"HU"},
	"it": {"IT", "SM"},
	"ja": {"JP"},
	"ko": {"KP", "KR"},
	"nl": {"BE", "NL", "SR"},
	"pl": {"PL"},
	"pt": {"AO", "BR", "MZ", "PT"},
	"ro": {"MD", "RO"},
	"ru": {"BY", "KZ", "RU"},
	"sv": {"SE"},
	"th": {"TH"},
	"tr": {"TR"},
	"uk": {"UA"},
	"vi": {"VN"},
	"zh": {"CN", "HK", "MO", "TW"},
}

// regionWords are, by a language's ISO 639-1 code, what a title may call
// one of its regional forms other than by the region's name, lower-cased
// and without accents: CLDR's names of the form, in English and in the
// language itself ("brazilian portuguese", "espanol latinoamericano"), each
// without the language's own name too ("brazilian", "latin american"), and
// the words a title uses ("latino", "brasileiro"), and the names of its
// regions in the language itself ("brasil", "latinoamerica", "osterreich").
// chino-web's dialectsOf, REGION_WORDS and regionsOf.
var regionWords = map[string]map[string]string{
	"ar": {
		"الأردن": "JO", "الإمارات العربية المتحدة": "AE", "البحرين": "BH", "الجزائر": "DZ", "السودان": "SD",
		"العراق": "IQ", "الكويت": "KW", "المغرب": "MA", "المملكة العربية السعودية": "SA", "اليمن": "YE",
		"تونس": "TN", "سوريا": "SY", "عُمان": "OM", "قطر": "QA", "لبنان": "LB", "ليبيا": "LY", "مصر": "EG",
	},
	"cs": {
		"cesko": "CZ",
	},
	"da": {
		"danmark": "DK",
	},
	"de": {
		"austrian": "AT", "austrian german": "AT", "deutschland": "DE", "liechtenstein": "LI", "osterreich": "AT",
		"osterreichisches": "AT", "osterreichisches deutsch": "AT", "schweiz": "CH", "schweizer hochdeutsch": "CH",
		"swiss high": "CH", "swiss high german": "CH",
	},
	"el": {
		"ελλάδα": "GR", "κύπρος": "CY",
	},
	"en": {
		"american": "US", "american english": "US", "australian": "AU", "australian english": "AU",
		"british": "GB", "british english": "GB", "canadian": "CA", "canadian english": "CA",
	},
	"es": {
		"argentina": "AR", "bolivia": "BO", "chile": "CL", "colombia": "CO", "costa rica": "CR", "cuba": "CU",
		"de espana": "ES", "de mexico": "MX", "ecuador": "EC", "el salvador": "SV", "espana": "ES",
		"espanol de espana": "ES", "espanol de mexico": "MX", "espanol latinoamericano": "419", "european": "ES",
		"european spanish": "ES", "guatemala": "GT", "honduras": "HN", "latam": "419", "latin american": "419",
		"latin american spanish": "419", "latino": "419", "latinoamerica": "419", "latinoamericano": "419",
		"mexican": "MX", "mexican spanish": "MX", "mexico": "MX", "nicaragua": "NI", "panama": "PA", "peru": "PE",
		"puerto rico": "PR", "republica dominicana": "DO", "uruguay": "UY", "venezuela": "VE",
	},
	"fi": {
		"suomi": "FI",
	},
	"fr": {
		"canada": "CA", "canadian": "CA", "canadian french": "CA", "canadien": "CA", "francais canadien": "CA",
		"francais suisse": "CH", "france": "FR", "luxembourg": "LU", "monaco": "MC", "quebec": "CA",
		"quebecois": "CA", "suisse": "CH", "swiss": "CH", "swiss french": "CH",
	},
	"he": {
		"ישראל": "IL",
	},
	"hi": {
		"भारत": "IN",
	},
	"hu": {
		"magyarorszag": "HU",
	},
	"it": {
		"italia": "IT", "san marino": "SM",
	},
	"ja": {
		"日本": "JP",
	},
	"ko": {
		"대한민국": "KR", "북한": "KP", "한국": "KR",
	},
	"nl": {
		"belgie": "BE", "flemish": "BE", "nederland": "NL", "suriname": "SR", "vlaams": "BE",
	},
	"pl": {
		"polska": "PL",
	},
	"pt": {
		"angola": "AO", "brasil": "BR", "brasileiro": "BR", "brazilian": "BR", "brazilian portuguese": "BR",
		"european": "PT", "european portuguese": "PT", "europeu": "PT", "mocambique": "MZ", "portugal": "PT",
	},
	"ro": {
		"moldavian": "MD", "republica moldova": "MD", "romania": "RO",
	},
	"ru": {
		"беларусь": "BY", "казахстан": "KZ", "россия": "RU",
	},
	"sv": {
		"sverige": "SE",
	},
	"th": {
		"ไทย": "TH",
	},
	"tr": {
		"turkiye": "TR",
	},
	"uk": {
		"україна": "UA",
	},
	"vi": {
		"việt nam": "VN",
	},
	"zh": {
		"中国": "CN", "中国澳门特别行政区": "MO", "中国香港特别行政区": "HK", "中國": "CN", "中國澳門特別行政區": "MO", "中國香港特別行政區": "HK",
		"台湾": "TW", "台灣": "TW", "澳門": "MO", "澳门": "MO", "香港": "HK",
	},
}

// chineseScripts are the words a Chinese track's title gives its script in,
// lower-cased: they say more than "Chinese", and name it by its script
// ("Chinese (Simplified)"), as a track tagged with it is. chino-web's
// CHINESE_SCRIPTS, and the names CLDR gives the two forms.
var chineseScripts = map[string]string{
	"chs": "Hans", "cht": "Hant", "simplified": "Hans", "simplified chinese": "Hans", "traditional": "Hant",
	"traditional chinese": "Hant", "简中": "Hans", "简体": "Hans", "简体中文": "Hans", "簡中": "Hans", "簡體": "Hans",
	"簡體中文": "Hans", "繁中": "Hant", "繁体": "Hant", "繁体中文": "Hant", "繁體": "Hant", "繁體中文": "Hant",
}

// regionAliases are the codes of regions that CLDR replaces by another
// (UK by GB, a withdrawn ISO 3166 code by its successor), as a tag is
// canonicalized: "en-UK" is "en-GB".
var regionAliases = map[string]string{
	"AN": "CW", "BU": "MM", "CS": "RS", "DD": "DE", "DY": "BJ", "FX": "FR", "HV": "BF", "NH": "VU", "RH": "ZW",
	"SU": "RU", "TP": "TL", "UK": "GB", "VD": "VN", "YD": "YE", "YU": "RS", "ZR": "CD",
}
