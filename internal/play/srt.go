package play

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A SubRip sidecar is served as WebVTT at its .vtt URL. Every client is given
// /api/v1/play/subs/<id>.vtt (chino-api) and chino-api lists such a file as
// webvtt, which is what the URL then serves: a browser's <track> reads WebVTT
// only, and a player picks its parser by the format it is told.

// maxSidecarBytes bounds a SubRip file read whole to be converted; a film's
// subtitles are tens of kilobytes.
const maxSidecarBytes = 8 << 20

// srtTiming is a SubRip cue's timing line: "00:01:02,345 --> 00:01:04,000",
// a dot for the comma and one-digit hours or fractions as files have them,
// with anything after the end time (SubRip's X1:… coordinates) dropped.
var srtTiming = regexp.MustCompile(`^\s*(\d+):(\d{1,2}):(\d{1,2})(?:[,.](\d{1,3}))?\s*-->\s*(\d+):(\d{1,2}):(\d{1,2})(?:[,.](\d{1,3}))?`)

// assOverride is an ASS override block in cue text ({\an8}, {\i1}), which a
// SubRip file may carry and WebVTT would show as text.
var assOverride = regexp.MustCompile(`\{\\[^}]*\}`)

// serveSRTAsWebVTT answers with the SubRip file at path converted to WebVTT.
// The conversion is cheap and the file small, so it runs on every request;
// browsers cache the answer for a day (not for good: a better conversion must
// reach them).
func serveSRTAsWebVTT(w http.ResponseWriter, r *http.Request, path string, st os.FileInfo) {
	if st.Size() > maxSidecarBytes {
		http.Error(w, "subtitle file too large", http.StatusBadGateway)
		return
	}
	src, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "subtitle file missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "", st.ModTime(), bytes.NewReader(srtToWebVTT(src)))
}

// srtToWebVTT converts a SubRip file to WebVTT. The text is decoded to UTF-8
// (decodeSubtitleText) and read cue by cue, as SubRip readers read it: a cue
// is a timing line and the text up to the next one, less that one's cue
// number (the last line before it, when only digits) and the blank lines,
// which files put between a cue's number, timing and text as often as
// between cues. Each cue is written as WebVTT writes one: its timing with
// two-digit hours and a dot and three digits for the fraction, its text
// without ASS override blocks and with an arrow written as one character
// (in WebVTT a line with "-->" is a timing line), and a blank line after it.
// A cue without text is left out.
func srtToWebVTT(src []byte) []byte {
	text := decodeSubtitleText(src)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	var b strings.Builder
	b.Grow(len(text) + 16)
	b.WriteString("WEBVTT\n")
	for i := 0; i < len(lines); i++ {
		m := srtTiming.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		// The cue's text: the lines up to the next timing line.
		j := i + 1
		for j < len(lines) && !srtTiming.MatchString(lines[j]) {
			j++
		}
		var cue []string
		for _, line := range lines[i+1 : j] {
			line = strings.TrimSpace(assOverride.ReplaceAllString(line, ""))
			if line != "" {
				cue = append(cue, strings.ReplaceAll(line, "-->", "→"))
			}
		}
		// The next cue's number ends this one's lines.
		if j < len(lines) && len(cue) > 0 && isCueNumber(cue[len(cue)-1]) {
			cue = cue[:len(cue)-1]
		}
		if len(cue) > 0 {
			b.WriteString("\n" + vttTime(m[1], m[2], m[3], m[4]) + " --> " + vttTime(m[5], m[6], m[7], m[8]) + "\n")
			b.WriteString(strings.Join(cue, "\n") + "\n")
		}
		i = j - 1
	}
	return []byte(b.String())
}

// isCueNumber reports whether a line is a SubRip cue number: digits only.
func isCueNumber(line string) bool {
	for _, c := range line {
		if c < '0' || c > '9' {
			return false
		}
	}
	return line != ""
}

// vttTime is a SubRip time's parts as a WebVTT timestamp: hh:mm:ss.ttt. A
// fraction is the digits after the separator ("5" is 500 ms).
func vttTime(h, m, s, frac string) string {
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	ss, _ := strconv.Atoi(s)
	ms, _ := strconv.Atoi((frac + "000")[:3])
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hh, mm, ss, ms)
}

// decodeSubtitleText is a subtitle file's text as UTF-8: after a UTF-8 byte
// order mark, as UTF-16 after one of its marks (or, without one, when every
// other byte is zero), as UTF-8 when it is that, else as Windows-1252, the
// encoding older SubRip files are most often in.
func decodeSubtitleText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(b[3:]), "�")
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], binary.BigEndian)
	case len(b) >= 4 && b[0] != 0 && b[1] == 0 && b[2] != 0 && b[3] == 0:
		return decodeUTF16(b, binary.LittleEndian)
	case len(b) >= 4 && b[0] == 0 && b[1] != 0 && b[2] == 0 && b[3] != 0:
		return decodeUTF16(b, binary.BigEndian)
	case utf8.Valid(b):
		return string(b)
	}
	return decodeWindows1252(b)
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

// windows1252 is what Windows-1252 has at 0x80-0x9F, where it differs from
// Latin-1; the five bytes it leaves undefined are their Latin-1 controls.
var windows1252 = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

func decodeWindows1252(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) + len(b)/8)
	for _, c := range b {
		switch {
		case c >= 0x80 && c < 0xA0:
			s.WriteRune(windows1252[c-0x80])
		default:
			s.WriteRune(rune(c))
		}
	}
	return s.String()
}
