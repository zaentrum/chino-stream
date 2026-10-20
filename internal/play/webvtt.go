package play

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A package's subtitle on the live pipeline (phase-1.md, F for a package): a
// WebVTT file already, so chino-stream shifts its cues itself instead of
// running it through ffmpeg's WebVTT encoder. The cues from t on are kept, by
// the rule the live worker applies to an original's (its subtitle.WebVTT):
// a cue that ends at or before t goes, the others are shifted by -t (one that
// began before t starts at 0), sorted by start; a cue with no end lasts until
// the next one, at most webvttDefaultCue. A cue's identifier, settings and
// text stay as they are; NOTE blocks go, the STYLE and REGION blocks before
// the first cue stay.

// webvttDefaultCue is how long a cue without an end shows, at most.
const webvttDefaultCue = 5.0

// webvttTiming is a cue's timing line: start --> end, then its settings.
var webvttTiming = regexp.MustCompile(`^\s*((?:\d+:)?\d{1,2}:\d{1,2}[.,]\d{1,3})\s+-->\s+((?:\d+:)?\d{1,2}:\d{1,2}[.,]\d{1,3})(.*)$`)

// webvttCue is one cue of a document.
type webvttCue struct {
	id         string
	start, end float64
	settings   string
	text       string
}

// shiftWebVTT is the WebVTT document doc from t seconds on (see the top of
// this file).
func shiftWebVTT(doc []byte, t float64) ([]byte, error) {
	s := string(bytes.TrimPrefix(doc, []byte("\xEF\xBB\xBF")))
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	blocks := strings.Split(s, "\n\n")
	if len(blocks) == 0 || !strings.HasPrefix(strings.TrimSpace(blocks[0]), "WEBVTT") {
		return nil, errors.New("not a WebVTT document")
	}
	var head []string
	var cues []webvttCue
	for _, block := range blocks[1:] {
		block = strings.Trim(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		first := strings.TrimSpace(lines[0])
		switch {
		case strings.HasPrefix(first, "NOTE"):
			continue
		case (strings.HasPrefix(first, "STYLE") || strings.HasPrefix(first, "REGION")) && len(cues) == 0 && !strings.Contains(block, "-->"):
			head = append(head, block)
			continue
		}
		at := 0
		if !strings.Contains(lines[0], "-->") {
			at = 1 // an identifier first
		}
		if at >= len(lines) {
			continue
		}
		m := webvttTiming.FindStringSubmatch(lines[at])
		if m == nil {
			continue
		}
		start, ok1 := webvttSeconds(m[1])
		end, ok2 := webvttSeconds(m[2])
		if !ok1 || !ok2 {
			continue
		}
		c := webvttCue{start: start, end: end, settings: strings.TrimRight(m[3], " \t"), text: strings.Join(lines[at+1:], "\n")}
		if at == 1 {
			c.id = lines[0]
		}
		cues = append(cues, c)
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
	for i := range cues {
		if cues[i].end <= cues[i].start {
			end := cues[i].start + webvttDefaultCue
			if i+1 < len(cues) && cues[i+1].start > cues[i].start && cues[i+1].start < end {
				end = cues[i+1].start
			}
			cues[i].end = end
		}
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, h := range head {
		b.WriteString(h + "\n\n")
	}
	for _, c := range cues {
		if strings.TrimSpace(c.text) == "" || c.end <= t {
			continue
		}
		if c.id != "" {
			b.WriteString(c.id + "\n")
		}
		fmt.Fprintf(&b, "%s --> %s%s\n%s\n\n", webvttStamp(math.Max(0, c.start-t)), webvttStamp(c.end-t), c.settings, c.text)
	}
	return []byte(b.String()), nil
}

// webvttSeconds reads a WebVTT timestamp, [hh:]mm:ss.ttt.
func webvttSeconds(s string) (float64, bool) {
	s = strings.Replace(s, ",", ".", 1)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	mul := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, false
		}
		sec += float64(v) * mul
		mul *= 60
	}
	return sec, true
}

// webvttStamp is seconds as a WebVTT timestamp, HH:MM:SS.mmm.
func webvttStamp(sec float64) string {
	ms := int64(math.Round(sec * 1000))
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
