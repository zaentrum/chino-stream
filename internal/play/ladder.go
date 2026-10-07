package play

import (
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zaentrum/chino-stream/internal/pkgmanifest"
)

// Serving a packaged ladder.
//
// A ladder package's master lists every video rung (an HEVC top rung,
// H.264 rungs under it, never upscaled) once per audio group — the stereo
// AAC group "audio" and, with the packager's SURROUND_AUDIO, the 5.1
// group "audio-surround" — plus the SUBTITLES group (HLS_SUBTITLES) and
// one I-frame playlist per rung. Served as it is it plays badly: players
// do not switch between HEVC and H.264 in a stream, and hls.js starts on
// the best level under min(first BANDWIDTH, 5 Mbit/s) — an H.264 rung
// whenever the HEVC top rung is above that, where an HEVC browser then
// stays. And a client gets a 5.1 group it may not decode.
//
// So a client is served one codec family, at the heights its decoder
// takes, with the audio groups it decodes (Caps, from ?caps=):
//
//   - Video: the HEVC rungs when it decodes HEVC and one of them fits its
//     HEVC height cap; else the H.264 rungs that fit its H.264 cap; else
//     the first other family it decodes (AV1 and VP9 are not packaged
//     today). Never a rung taller than the client's cap for its codec
//     (Caps.VideoMaxHeight, the ":<height>" suffix of a caps token).
//   - q=<rung id> (/play/info lists them): that rung alone, when the
//     client decodes it at its height — any family, since a quality pick
//     loads a new master. Any other q (auto, the on-the-fly ladder's
//     high/medium/low, a rung the client can't play) serves the ladder.
//   - Audio: a group stays when the client decodes all its codecs. Stereo
//     AAC always does; ec-3 needs eac3 in caps, ac-3 needs ac3, AAC with
//     more than two channels needs aacmc, opus and mp3 their tokens. A
//     dropped group's EXT-X-MEDIA lines and variants go together.
//   - SUBTITLES stay as packaged (a master has the group only when the
//     packager wrote it); I-frame playlists only for the rungs served.
//   - Each audio group served has exactly one DEFAULT=YES: the
//     packager's, else the rendition in the language of the first group's
//     default, else the first AUTOSELECT one, else the first. A SUBTITLES
//     group keeps at most one.
//
// A master with one video rendition and at most one audio group — every
// package from before renditions.json — has nothing to choose between and
// is served exactly as packaged (shaka's own masters mark no audio
// rendition DEFAULT; that stays as it is). A choice that would leave the
// client nothing to play (no rung it decodes) serves the master as
// packaged, as before: packagedPlayableBy has already sent clients that
// decode none of a package's rungs to the on-the-fly transcode.

// masterVariant is one EXT-X-STREAM-INF of a master and its URI line.
type masterVariant struct {
	inf, uri    int    // line indexes
	rung        string // the URI's first path segment: "v0"
	codec       string // the CODECS entry of the video
	family      string // codecFamily(codec)
	audioCodecs []string
	width       int
	height      int
	bandwidth   int
	videoRange  string
	audio       string // AUDIO group id
	subtitles   string // SUBTITLES group id
}

// masterMedia is one EXT-X-MEDIA line.
type masterMedia struct {
	line       int
	typ        string // AUDIO, SUBTITLES, …
	group      string
	rend       string // the URI's first path segment: "a0", "s1"
	language   string
	isDefault  bool
	autoselect bool
	channels   int
}

// hlsMaster is a master playlist split into lines, with its variants,
// renditions and I-frame playlists.
type hlsMaster struct {
	lines    []string
	variants []masterVariant
	media    []masterMedia
	iframes  map[int]string // line index → rung
}

func parseMaster(body string) *hlsMaster {
	m := &hlsMaster{lines: strings.Split(body, "\n"), iframes: map[int]string{}}
	for i := 0; i < len(m.lines); i++ {
		line := strings.TrimRight(m.lines[i], "\r")
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			a := hlsAttributes(line)
			v := masterVariant{inf: i, uri: -1, audio: a["AUDIO"], subtitles: a["SUBTITLES"],
				videoRange: a["VIDEO-RANGE"]}
			v.bandwidth, _ = strconv.Atoi(a["BANDWIDTH"])
			v.width, v.height = parseResolution(a["RESOLUTION"])
			for _, c := range strings.Split(a["CODECS"], ",") {
				c = strings.TrimSpace(c)
				switch {
				case c == "":
				case isVideoCodec(c):
					if v.codec == "" {
						v.codec, v.family = c, codecFamily(c)
					}
				default:
					v.audioCodecs = append(v.audioCodecs, c)
				}
			}
			for j := i + 1; j < len(m.lines); j++ {
				if t := strings.TrimSpace(m.lines[j]); t != "" && !strings.HasPrefix(t, "#") {
					v.uri, v.rung = j, firstPathSegment(t)
					break
				}
			}
			m.variants = append(m.variants, v)
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			a := hlsAttributes(line)
			md := masterMedia{line: i, typ: a["TYPE"], group: a["GROUP-ID"], rend: firstPathSegment(a["URI"]),
				language: a["LANGUAGE"], isDefault: a["DEFAULT"] == "YES", autoselect: a["AUTOSELECT"] == "YES"}
			md.channels, _ = strconv.Atoi(strings.SplitN(a["CHANNELS"], "/", 2)[0])
			m.media = append(m.media, md)
		case strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:"):
			m.iframes[i] = firstPathSegment(hlsAttributes(line)["URI"])
		}
	}
	return m
}

// videoCodecs are the sample entries of a CODECS video entry: those
// codecFamily knows and those it doesn't (H.264 with in-band parameter
// sets, Dolby Vision, MPEG-4 Part 2, VP8). An entry of the second kind is
// still the variant's video, of no family a client may be served (a
// browser rejects a dvh1 level it can't decode); it must not be read as an
// audio codec, which would drop its audio group.
var videoCodecs = []string{"avc1", "avc3", "hvc1", "hev1", "dvh1", "dvhe", "dva1", "dvav", "dav1",
	"vp08", "vp09", "av01", "mp4v"}

func isVideoCodec(c string) bool {
	c = strings.ToLower(c)
	for _, p := range videoCodecs {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return codecFamily(c) != ""
}

// ladderRung is one video rendition of a master: the attributes of its
// first variant (the stereo group's, the packager writes it first).
type ladderRung struct {
	id         string
	codec      string
	family     string
	width      int
	height     int
	bandwidth  int
	videoRange string
}

// rungs lists the master's video renditions in master order (the top rung
// first, as the packager writes them).
func (m *hlsMaster) rungs() []ladderRung {
	var out []ladderRung
	seen := map[string]bool{}
	for _, v := range m.variants {
		if v.rung == "" || seen[v.rung] {
			continue
		}
		seen[v.rung] = true
		out = append(out, ladderRung{id: v.rung, codec: v.codec, family: v.family, width: v.width,
			height: v.height, bandwidth: v.bandwidth, videoRange: v.videoRange})
	}
	return out
}

func (m *hlsMaster) audioGroups() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range m.variants {
		if v.audio != "" && !seen[v.audio] {
			seen[v.audio] = true
			out = append(out, v.audio)
		}
	}
	return out
}

// videoFamilyOK reports whether caps carries a decoder for a codec family
// (codecFamily's keys).
func videoFamilyOK(family string, caps Caps) bool {
	switch family {
	case "h264":
		return caps.Video["h264"] || caps.Video["avc1"]
	case "hevc":
		return caps.Video["hevc"] || caps.Video["hvc1"] || caps.Video["h265"]
	case "vp9", "av1":
		return caps.Video[family]
	}
	return false
}

// rungPlayable: the client decodes the rung's codec at its height.
func rungPlayable(r ladderRung, caps Caps) bool {
	if !videoFamilyOK(r.family, caps) {
		return false
	}
	maxH := caps.VideoMaxHeight[r.family]
	return maxH <= 0 || r.height <= maxH
}

// ladderFamily is the codec family a client is served: HEVC when one of the
// HEVC rungs plays, else H.264, else the first other one that does; "" when
// none does.
func ladderFamily(rungs []ladderRung, caps Caps) string {
	for _, fam := range []string{"hevc", "h264"} {
		for _, r := range rungs {
			if r.family == fam && rungPlayable(r, caps) {
				return fam
			}
		}
	}
	for _, r := range rungs {
		if rungPlayable(r, caps) {
			return r.family
		}
	}
	return ""
}

// audioCodecOK reports whether caps decodes an audio CODECS entry carried
// with up to channels channels.
func audioCodecOK(codec string, channels int, caps Caps) bool {
	c := strings.ToLower(codec)
	switch {
	case c == "mp4a.40.34" || c == "mp4a.6b" || c == "mp4a.69":
		return caps.Audio["mp3"]
	case strings.HasPrefix(c, "mp4a."):
		// Stereo AAC is what every client decodes: the stereo group is
		// never dropped. More channels only with the aacmc opt-in.
		return channels <= 2 || caps.AACMultichannel
	case c == "ec-3":
		return caps.Audio["eac3"]
	case c == "ac-3":
		return caps.Audio["ac3"]
	case c == "opus":
		return caps.Audio["opus"]
	}
	return false
}

// servedLadder is what a client gets of a packaged master: the body and
// the variant it starts on — the served master's first variant (where
// players without a bandwidth estimate start, the top rung served) and the
// audio rendition it starts with.
type servedLadder struct {
	body  string
	video string // "v1"
	audio string // "a0"; "" when the variant has no audio group
	// more are the renditions the start rung's other audio groups start on
	// ("a2"): a native player's per-codec groups, between which the player
	// picks itself (mirrorAudioGroups). None for a master of one group.
	more []string
	// listed is every audio rendition the master lists, in its order, when
	// one of its groups is mixed — a variant whose CODECS name more than one
	// audio codec: the union, a native player's 5.1 group that plays a
	// member in stereo. Media3 reads the playlist and init of each of those
	// before it starts (it prepares from the master alone only when a
	// variant names one audio codec). nil for a master of single-codec
	// groups, a stereo-only client's.
	listed []string
}

// audioTwins maps a rendition of a further audio group — a 5.1 companion,
// "a2" — to the stereo rendition made from the same source track, "a0", as
// a package's manifest pairs them (twinsOf). A rendition it has no entry
// for, and every one when it is nil, is paired by language.
type audioTwins map[string]string

// twinsOf pairs each 5.1 companion of the package mf with the stereo
// rendition of its source track (pkgmanifest.AudioRendition.SourceTrack).
func twinsOf(mf *pkgmanifest.Manifest) audioTwins {
	if mf == nil || len(mf.Renditions.AudioSurround) == 0 {
		return nil
	}
	stereo := map[int]string{}
	for _, a := range mf.Renditions.Audio {
		if k, ok := a.SourceTrack(); ok {
			if _, seen := stereo[k]; !seen {
				stereo[k] = renditionID(a)
			}
		}
	}
	out := audioTwins{}
	for _, a := range mf.Renditions.AudioSurround {
		if k, ok := a.SourceTrack(); ok {
			if twin, ok := stereo[k]; ok {
				out[renditionID(a)] = twin
			}
		}
	}
	return out
}

// renditionID is an audio rendition's id in its master — the folder its
// URI names, "a2" for hls/a2 — the manifest's id when it names no folder.
func renditionID(a pkgmanifest.AudioRendition) string {
	if a.Dir != "" {
		return path.Base(a.Dir)
	}
	return a.ID
}

// serveLadder is the packaged master body as the client with caps, asking
// for quality q, is served (see the top of this file). twins pairs the 5.1
// companions with their stereo renditions (twinsOf; nil: by language).
func serveLadder(body string, caps Caps, q string, twins audioTwins) servedLadder {
	m := parseMaster(body)
	rungs := m.rungs()
	if len(rungs) <= 1 && len(m.audioGroups()) <= 1 {
		return m.served(body)
	}

	videos := map[string]bool{}
	for _, r := range rungs {
		if r.id == q && rungPlayable(r, caps) {
			videos[r.id] = true
		}
	}
	if len(videos) == 0 {
		fam := ladderFamily(rungs, caps)
		for _, r := range rungs {
			if r.family == fam && rungPlayable(r, caps) {
				videos[r.id] = true
			}
		}
	}
	if len(videos) == 0 {
		return m.served(body) // nothing it decodes: as packaged, as before
	}

	// The audio groups of the chosen rungs and whether the client decodes
	// them. Should it decode none, it gets them all, as packaged.
	codecs := map[string][]string{}
	for _, v := range m.variants {
		if videos[v.rung] && v.audio != "" {
			codecs[v.audio] = append(codecs[v.audio], v.audioCodecs...)
		}
	}
	channels := map[string]int{}
	for _, md := range m.media {
		if md.typ == "AUDIO" && md.channels > channels[md.group] {
			channels[md.group] = md.channels
		}
	}
	groups := map[string]bool{}
	for g, cs := range codecs {
		ok := true
		for _, c := range cs {
			ok = ok && audioCodecOK(c, channels[g], caps)
		}
		groups[g] = ok
	}
	// A client that decodes a further group besides the stereo one (the 5.1
	// companions, with eac3) is served one variant per rung, its audio the
	// two groups as one — but a player that plays the master natively, which
	// picks between the groups itself (mirrorAudioGroups, below).
	if !caps.Native {
		if s, ok := m.serveUnion(videos, groups, twins); ok {
			return s
		}
	}
	keep := func(v masterVariant) bool { return videos[v.rung] && (v.audio == "" || groups[v.audio]) }
	kept := false
	for _, v := range m.variants {
		kept = kept || keep(v)
	}
	if !kept {
		for g := range groups {
			groups[g] = true
		}
	}

	drop := make([]bool, len(m.lines))
	audioRefs, subRefs := map[string]bool{}, map[string]bool{}
	for _, v := range m.variants {
		if keep(v) {
			audioRefs[v.audio], subRefs[v.subtitles] = true, true
			continue
		}
		drop[v.inf] = true
		if v.uri >= 0 {
			drop[v.uri] = true
		}
	}
	for _, md := range m.media {
		switch md.typ {
		case "AUDIO":
			drop[md.line] = !audioRefs[md.group]
		case "SUBTITLES":
			drop[md.line] = !subRefs[md.group]
		}
	}
	for line, rung := range m.iframes {
		drop[line] = !videos[rung]
	}
	m.normalizeDefaults(drop)
	served := m.render(drop)
	if caps.Native {
		if mirrored, ok := served.mirrorAudioGroups(twins); ok {
			return mirrored.served("")
		}
	}
	return served.served("")
}

// serveUnion serves the client that decodes a further audio group as well
// as the stereo one — the packager's "audio-surround", the 5.1 companions,
// to a client with eac3 — one variant per rung, not one per rung and audio
// group: a player with two variants of a rung (one per group, their
// BANDWIDTH and CODECS apart) lists them as two levels, starts on the stereo
// ones and keeps to them (hls.js), and a pick of a 5.1 track had to move it
// to another level. A variant names one audio group (RFC 8216), so the
// client's group holds both: every stereo rendition, and each of the
// further group's renditions just before the stereo rendition of its source
// track (twins, else the first stereo one in its language) — "English 5.1",
// "English", "German" — so no track goes (a stereo source track has no
// companion, a commentary never has one) and a player that takes the first
// rendition of a language takes the 5.1. Exactly one is DEFAULT: the
// companion of the stereo group's default, else that default itself. It is
// the further group (its GROUP-ID): the stereo renditions join it.
//
// The variant of each rung served is the further group's, its CODECS with
// the stereo codecs added (RFC 8216: every format any rendition of the
// group carries) and its BANDWIDTH the larger of the rung's two. The other
// groups, their renditions and variants go; the subtitles and I-frame
// playlists are served as for any client.
//
// ok false (serve as before) when the client decodes no further group that
// every rung served carries, or no stereo group is served.
func (m *hlsMaster) serveUnion(videos map[string]bool, decoded map[string]bool, twins audioTwins) (servedLadder, bool) {
	stereo := ""
	for _, v := range m.variants {
		if videos[v.rung] && v.audio != "" {
			stereo = v.audio
			break
		}
	}
	if stereo == "" || !decoded[stereo] {
		return servedLadder{}, false
	}
	carried := func(group string) bool {
		for rung := range videos {
			found := false
			for _, v := range m.variants {
				found = found || (v.rung == rung && v.audio == group)
			}
			if !found {
				return false
			}
		}
		return true
	}
	further := ""
	for _, v := range m.variants {
		if videos[v.rung] && v.audio != "" && v.audio != stereo && decoded[v.audio] && carried(v.audio) {
			further = v.audio
			break
		}
	}
	if further == "" {
		return servedLadder{}, false
	}

	// The renditions of the two groups, paired.
	var sMedia, fMedia []int // indexes into m.media
	for i, md := range m.media {
		switch {
		case md.typ != "AUDIO":
		case md.group == stereo:
			sMedia = append(sMedia, i)
		case md.group == further:
			fMedia = append(fMedia, i)
		}
	}
	if len(sMedia) == 0 {
		return servedLadder{}, false
	}
	pairOf, paired := m.pairCompanions(sMedia, fMedia, twins)
	var order []int
	for _, si := range sMedia {
		if fi, ok := pairOf[si]; ok {
			order = append(order, fi)
		}
		order = append(order, si)
	}
	for _, fi := range fMedia {
		if !paired[fi] {
			order = append(order, fi)
		}
	}
	def := -1
	for _, si := range sMedia {
		if m.media[si].isDefault {
			def = si
			break
		}
	}
	if def < 0 {
		def = pickDefault(m.media, sMedia, "")
	}
	if fi, ok := pairOf[def]; ok {
		def = fi
	}
	names := make([]string, len(order))
	for i, mi := range order {
		names[i] = hlsAttributes(m.lines[m.media[mi].line])["NAME"]
	}
	unique := uniqueNames(names)
	union := make([]string, len(order))
	for i, mi := range order {
		line := setHLSAttribute(m.lines[m.media[mi].line], "GROUP-ID", further)
		line = setHLSAttribute(line, "DEFAULT", yesNo(mi == def))
		if unique[i] != names[i] {
			line = setHLSAttribute(line, "NAME", unique[i])
		}
		union[i] = line
	}

	// The variants: each rung's of the further group, with the stereo
	// codecs and the larger BANDWIDTH of the two.
	stereoOf := map[string]masterVariant{}
	for _, v := range m.variants {
		if v.audio == stereo {
			if _, ok := stereoOf[v.rung]; !ok {
				stereoOf[v.rung] = v
			}
		}
	}
	keptInf := map[int]string{} // line → the variant line served
	drop := make([]bool, len(m.lines))
	subRefs := map[string]bool{}
	for _, v := range m.variants {
		if !videos[v.rung] || v.audio != further {
			drop[v.inf] = true
			if v.uri >= 0 {
				drop[v.uri] = true
			}
			continue
		}
		line := m.lines[v.inf]
		if sv, ok := stereoOf[v.rung]; ok {
			line = m.withAudioOf(v, sv)
		}
		keptInf[v.inf] = line
		subRefs[v.subtitles] = true
	}
	first := -1 // the line the union's renditions are served at
	for _, md := range m.media {
		switch md.typ {
		case "AUDIO":
			drop[md.line] = true
			if first < 0 && (md.group == stereo || md.group == further) {
				first = md.line
			}
		case "SUBTITLES":
			drop[md.line] = !subRefs[md.group]
		}
	}
	for line, rung := range m.iframes {
		drop[line] = !videos[rung]
	}
	var out []string
	for i, l := range m.lines {
		switch {
		case i == first:
			out = append(out, union...)
		case drop[i]:
		case keptInf[i] != "":
			out = append(out, keptInf[i])
		default:
			out = append(out, l)
		}
	}
	served := parseMaster(strings.Join(out, "\n"))
	none := make([]bool, len(served.lines))
	served.normalizeDefaults(none)
	return served.render(none).served(""), true
}

// pairCompanions pairs the renditions of a further audio group (fMedia,
// indexes into m.media) with the stereo ones (sMedia): each with the stereo
// rendition of its source track (twins), else — when twins says nothing of
// it — with the first stereo one in its language not paired yet. pairOf
// maps a stereo rendition to its companion, paired the further renditions
// that have a stereo one.
func (m *hlsMaster) pairCompanions(sMedia, fMedia []int, twins audioTwins) (pairOf map[int]int, paired map[int]bool) {
	pairOf, paired = map[int]int{}, map[int]bool{}
	pair := func(fi int, match func(si int) bool) {
		for _, si := range sMedia {
			if _, taken := pairOf[si]; !taken && match(si) {
				pairOf[si], paired[fi] = fi, true
				return
			}
		}
	}
	for _, fi := range fMedia {
		if twin, ok := twins[m.media[fi].rend]; ok {
			pair(fi, func(si int) bool { return m.media[si].rend == twin })
		}
	}
	for _, fi := range fMedia {
		if _, ok := twins[m.media[fi].rend]; !ok && !paired[fi] {
			lang := strings.ToLower(m.media[fi].language)
			pair(fi, func(si int) bool { return strings.ToLower(m.media[si].language) == lang })
		}
	}
	return pairOf, paired
}

// withAudioOf is the EXT-X-STREAM-INF line of v, whose audio group holds
// renditions of the stereo variant sv's group as well: its CODECS with sv's
// audio codecs added (RFC 8216: every format any rendition of the group
// carries) and its BANDWIDTH and AVERAGE-BANDWIDTH the larger of the two.
func (m *hlsMaster) withAudioOf(v, sv masterVariant) string {
	line := m.lines[v.inf]
	a := hlsAttributes(line)
	codecs := strings.Split(a["CODECS"], ",")
	for _, c := range sv.audioCodecs {
		if !containsFold(codecs, c) {
			codecs = append(codecs, c)
		}
	}
	line = setHLSAttribute(line, "CODECS", strings.Join(codecs, ","))
	if sv.bandwidth > v.bandwidth {
		line = setHLSAttribute(line, "BANDWIDTH", strconv.Itoa(sv.bandwidth))
	}
	if avg, err := strconv.Atoi(hlsAttributes(m.lines[sv.inf])["AVERAGE-BANDWIDTH"]); err == nil {
		if own, err := strconv.Atoi(a["AVERAGE-BANDWIDTH"]); err == nil && avg > own {
			line = setHLSAttribute(line, "AVERAGE-BANDWIDTH", strconv.Itoa(avg))
		}
	}
	return line
}

// mirrorAudioGroups is the master m — a client's share, the audio groups it
// decodes, each rung once per group — as a player that plays it natively
// (caps.Native: AVPlayer, Safari without MSE) is served it: Apple's shape,
// one audio group per codec, each rung once per group, which RFC 8216
// (4.3.4.1.1) allows only with every group holding the same members, alike
// but for URI and CHANNELS. That is what lets the player match a track
// across groups and pick the group it decodes itself. So each further group
// (the 5.1 companions, "audio-surround") holds the stereo group's members, in
// their order and as they are named, each with the URI and CHANNELS of its
// companion there (pairCompanions) when it has one, else its own: a track
// without a companion (a stereo source, a commentary) plays stereo in that
// group too, as RFC 8216 lets a group's members differ in sample format. A
// further rendition that is nobody's companion stays in its group after
// them, named apart. The further group's variants list the stereo codecs too
// when a member plays from the stereo group, with the larger BANDWIDTH of
// their rung's two.
//
// ok false (m as it is) when m serves a single audio group.
func (m *hlsMaster) mirrorAudioGroups(twins audioTwins) (*hlsMaster, bool) {
	if len(m.variants) == 0 || m.variants[0].audio == "" {
		return m, false
	}
	stereo := m.variants[0].audio
	var further []string
	for _, v := range m.variants {
		if v.audio != "" && v.audio != stereo && !slices.Contains(further, v.audio) {
			further = append(further, v.audio)
		}
	}
	var sMedia []int
	fMedia := map[string][]int{}
	for i, md := range m.media {
		switch {
		case md.typ != "AUDIO":
		case md.group == stereo:
			sMedia = append(sMedia, i)
		case slices.Contains(further, md.group):
			fMedia[md.group] = append(fMedia[md.group], i)
		}
	}
	if len(further) == 0 || len(sMedia) == 0 {
		return m, false
	}
	block := map[string][]string{} // a further group → its lines, served
	viaStereo := map[string]bool{} // a further group a member plays stereo in
	for _, g := range further {
		pairOf, paired := m.pairCompanions(sMedia, fMedia[g], twins)
		var lines, names []string
		for _, si := range sMedia {
			line := setHLSAttribute(m.lines[m.media[si].line], "GROUP-ID", g)
			if fi, ok := pairOf[si]; ok {
				a := hlsAttributes(m.lines[m.media[fi].line])
				line = setHLSAttribute(line, "URI", a["URI"])
				if ch, ok := a["CHANNELS"]; ok {
					line = setQuotedHLSAttribute(line, "CHANNELS", ch)
				}
			} else {
				viaStereo[g] = true
			}
			lines = append(lines, line)
			names = append(names, hlsAttributes(line)["NAME"])
		}
		var extra []int
		for _, fi := range fMedia[g] {
			if !paired[fi] {
				extra = append(extra, len(lines))
				lines = append(lines, m.lines[m.media[fi].line])
				names = append(names, hlsAttributes(m.lines[m.media[fi].line])["NAME"])
			}
		}
		unique := uniqueNames(names)
		for _, i := range extra {
			if unique[i] != names[i] {
				lines[i] = setHLSAttribute(lines[i], "NAME", unique[i])
			}
		}
		block[g] = lines
	}
	stereoOf := map[string]masterVariant{} // a rung → its stereo variant
	for _, v := range m.variants {
		if _, ok := stereoOf[v.rung]; !ok && v.audio == stereo {
			stereoOf[v.rung] = v
		}
	}
	inf := map[int]string{} // a further variant's line → the line served
	for _, v := range m.variants {
		if sv, ok := stereoOf[v.rung]; ok && viaStereo[v.audio] {
			inf[v.inf] = m.withAudioOf(v, sv)
		}
	}
	// Each further group's lines where its renditions were, else after the
	// stereo group's.
	at := map[int][]string{}
	lastStereo := m.media[sMedia[len(sMedia)-1]].line
	for _, g := range further {
		if fm := fMedia[g]; len(fm) > 0 {
			at[m.media[fm[0]].line] = append(at[m.media[fm[0]].line], block[g]...)
		} else {
			at[lastStereo] = append(at[lastStereo], block[g]...)
		}
	}
	skip := map[int]bool{}
	for _, g := range further {
		for _, fi := range fMedia[g] {
			skip[m.media[fi].line] = true
		}
	}
	var out []string
	for i, l := range m.lines {
		switch {
		case skip[i]:
		case inf[i] != "":
			out = append(out, inf[i])
		default:
			out = append(out, l)
		}
		out = append(out, at[i]...)
	}
	served := parseMaster(strings.Join(out, "\n"))
	served.normalizeDefaults(make([]bool, len(served.lines)))
	return served, true
}

// setQuotedHLSAttribute sets a quoted-string attribute of a tag line to
// value, adding it, quoted, at the end when the line has none.
func setQuotedHLSAttribute(line, key, value string) string {
	if _, ok := hlsAttributes(line)[key]; ok {
		return setHLSAttribute(line, key, value)
	}
	return setHLSAttribute(line, key, strconv.Quote(value))
}

// containsFold reports whether list holds s, case aside.
func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

// packagedQualities is the quality choice /play/info offers for a packaged
// master: {"name": "auto", "label": "Auto"} — the client's ladder,
// adaptive — then one entry per picture size the client may pick, the
// tallest first: name and id the rung id for ?q=, label the size class
// ("1080p"), width, height, codec (its CODECS video entry), bitrate (its
// variant's BANDWIDTH, the stereo group's), video_range. Of two rungs of
// one size class (an HEVC 1080p next to an H.264 one) the one in the
// family its ladder is in is listed. nil when the client may pick fewer
// than two rungs: nothing to choose (every package before
// renditions.json). Every entry has name and label, the two fields the
// clients' quality menus read.
func packagedQualities(body string, caps Caps) []map[string]any {
	rungs := parseMaster(body).rungs()
	fam := ladderFamily(rungs, caps)
	var picks []ladderRung
	byLabel := map[string]int{}
	for _, r := range rungs {
		if !rungPlayable(r, caps) {
			continue
		}
		label := rungLabel(r)
		if i, ok := byLabel[label]; ok {
			if picks[i].family != fam && r.family == fam {
				picks[i] = r
			}
			continue
		}
		byLabel[label] = len(picks)
		picks = append(picks, r)
	}
	if len(picks) < 2 {
		return nil
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].height > picks[j].height })
	out := []map[string]any{{"name": "auto", "label": "Auto"}}
	for _, r := range picks {
		e := map[string]any{"name": r.id, "id": r.id, "label": rungLabel(r), "width": r.width,
			"height": r.height, "codec": r.codec, "bitrate": r.bandwidth}
		if r.videoRange != "" {
			e["video_range"] = r.videoRange
		}
		out = append(out, e)
	}
	return out
}

// sizeClasses are the picture heights quality labels name.
var sizeClasses = []int{240, 360, 480, 540, 576, 720, 1080, 1440, 2160, 4320}

// rungLabel names a rung's picture size the way the ladder's rungs are
// named ("720p" = the 1280x720 box): the smallest class whose 16:9 box
// holds the frame, 10% of width to spare for DCI frames (4096x2160 is
// 2160p). So a 2.39:1 film's 1280x536 rung is 720p and its 3840x1606 top
// rung 2160p, like the transcoder's LADDER names them.
func rungLabel(r ladderRung) string {
	if r.height <= 0 {
		return r.id
	}
	for _, c := range sizeClasses {
		boxW := (c*16/9 + 1) &^ 1
		if r.height <= c && r.width*10 <= boxW*11 {
			return strconv.Itoa(c) + "p"
		}
	}
	return strconv.Itoa(r.height) + "p"
}

// normalizeDefaults leaves each audio group that is served with exactly
// one DEFAULT=YES and each SUBTITLES group with at most one, by rewriting
// the DEFAULT attribute of m's lines (and m.media to match).
func (m *hlsMaster) normalizeDefaults(drop []bool) {
	var order []string
	byGroup := map[string][]int{} // "TYPE/group" → indexes into m.media
	for i, md := range m.media {
		if drop[md.line] || (md.typ != "AUDIO" && md.typ != "SUBTITLES") {
			continue
		}
		key := md.typ + "/" + md.group
		if _, ok := byGroup[key]; !ok {
			order = append(order, key)
		}
		byGroup[key] = append(byGroup[key], i)
	}
	lang := "" // the language of the first audio group's default
	for _, key := range order {
		idx := byGroup[key]
		audio := strings.HasPrefix(key, "AUDIO/")
		def := -1
		for _, i := range idx {
			if m.media[i].isDefault && def < 0 {
				def = i
			}
		}
		if def < 0 && audio {
			def = pickDefault(m.media, idx, lang)
		}
		for _, i := range idx {
			if want := i == def; m.media[i].isDefault != want {
				m.media[i].isDefault = want
				m.lines[m.media[i].line] = setHLSAttribute(m.lines[m.media[i].line], "DEFAULT", yesNo(want))
			}
		}
		if audio && lang == "" && def >= 0 {
			lang = m.media[def].language
		}
	}
}

// pickDefault chooses the DEFAULT rendition of an audio group that has
// none: the one in language lang, else the first AUTOSELECT one, else the
// first.
func pickDefault(media []masterMedia, idx []int, lang string) int {
	if lang != "" {
		for _, i := range idx {
			if media[i].language == lang {
				return i
			}
		}
	}
	for _, i := range idx {
		if media[i].autoselect {
			return i
		}
	}
	return idx[0]
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

// render is m without the dropped lines, a blank line a drop left next to
// another blank line taken out too.
func (m *hlsMaster) render(drop []bool) *hlsMaster {
	var out []string
	for i, l := range m.lines {
		if drop[i] {
			continue
		}
		if strings.TrimSpace(l) == "" && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			continue
		}
		out = append(out, l)
	}
	return parseMaster(strings.Join(out, "\n"))
}

// ladderStart is where a client starts on a served master: its first
// variant and that variant's audio rendition.
func ladderStart(body string) servedLadder {
	return parseMaster(body).served(body)
}

// served is m served as body ("" = m's own lines), with its start.
func (m *hlsMaster) served(body string) servedLadder {
	if body == "" {
		body = strings.Join(m.lines, "\n")
	}
	s := servedLadder{body: body}
	if len(m.variants) == 0 {
		return s
	}
	start := m.variants[0]
	s.video = start.rung
	s.audio = m.groupStart(start.audio)
	for _, v := range m.variants {
		if v.rung != start.rung || v.audio == "" || v.audio == start.audio {
			continue
		}
		if r := m.groupStart(v.audio); r != "" && r != s.audio && !slices.Contains(s.more, r) {
			s.more = append(s.more, r)
		}
	}
	mixed := false
	for _, v := range m.variants {
		mixed = mixed || len(v.audioCodecs) > 1
	}
	for _, md := range m.media {
		if mixed && md.typ == "AUDIO" && md.rend != "" && !slices.Contains(s.listed, md.rend) {
			s.listed = append(s.listed, md.rend)
		}
	}
	return s
}

// groupStart is the rendition a player starts on in audio group group: its
// DEFAULT one, else its first (hls.js ignores AUTOSELECT when a group marks
// no default, as shaka's own masters do); "" for none.
func (m *hlsMaster) groupStart(group string) string {
	if group == "" {
		return ""
	}
	first := ""
	for _, md := range m.media {
		if md.typ != "AUDIO" || md.group != group {
			continue
		}
		if md.isDefault {
			return md.rend
		}
		if first == "" {
			first = md.rend
		}
	}
	return first
}

// hlsAttributes parses the attribute list of a tag line (after its first
// ':'): KEY=value pairs, a value a quoted string (commas allowed inside) or
// anything up to the next comma.
func hlsAttributes(line string) map[string]string {
	out := map[string]string{}
	for _, a := range hlsAttributeSpans(line) {
		out[a.key] = line[a.start:a.end]
	}
	return out
}

// hlsAttributeSpan locates one attribute value in a tag line, quotes
// excluded.
type hlsAttributeSpan struct {
	key        string
	start, end int
}

func hlsAttributeSpans(line string) []hlsAttributeSpan {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return nil
	}
	var out []hlsAttributeSpan
	i := colon + 1
	for i < len(line) {
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(line[i : i+eq])
		i += eq + 1
		var a hlsAttributeSpan
		if i < len(line) && line[i] == '"' {
			end := strings.IndexByte(line[i+1:], '"')
			if end < 0 {
				end = len(line) - i - 1
			}
			a = hlsAttributeSpan{key: key, start: i + 1, end: i + 1 + end}
			i = a.end + 1
		} else {
			end := strings.IndexByte(line[i:], ',')
			if end < 0 {
				end = len(line) - i
			}
			a = hlsAttributeSpan{key: key, start: i, end: i + end}
			i = a.end
			for a.end > a.start && (line[a.end-1] == '\r' || line[a.end-1] == ' ') {
				a.end-- // a CRLF line's CR is not the value's
			}
		}
		out = append(out, a)
		if i < len(line) && line[i] == ',' {
			i++
		}
	}
	return out
}

// setHLSAttribute sets an unquoted attribute of a tag line to value, adding
// it at the end when the line has none.
func setHLSAttribute(line, key, value string) string {
	for _, a := range hlsAttributeSpans(line) {
		if a.key == key {
			return line[:a.start] + value + line[a.end:]
		}
	}
	cr := ""
	if strings.HasSuffix(line, "\r") {
		line, cr = strings.TrimSuffix(line, "\r"), "\r"
	}
	return line + "," + key + "=" + value + cr
}

// parseResolution reads RESOLUTION=<w>x<h>; 0, 0 when absent or malformed.
func parseResolution(s string) (w, h int) {
	ws, hs, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0
	}
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

// firstPathSegment is a master URI's rendition dir: "v0" for
// "v0/playlist.m3u8?stream=…".
func firstPathSegment(uri string) string {
	if i := strings.IndexAny(uri, "/?"); i >= 0 {
		return uri[:i]
	}
	return uri
}
