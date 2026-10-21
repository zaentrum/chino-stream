package play

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/metrics"
	"github.com/zaentrum/chino-stream/internal/streamcopy"
)

// The stream copy of an original on the fly (copyplan.go): its video at
// video/, each audio track the client decodes at audiocopy{n}/ — all on the
// keyframes of the original's own index (copyindex.go), each segment one
// read of the byte range they bound — and each other audio track converted
// at audio/{n}/ (stereo AAC, on the 6 s grid of every on-the-fly rendition:
// an ffmpeg window, or a live worker job with LIVE_WORKER_MODE on) and, for
// a client that decodes AAC beyond two channels, at surround{n}/ (5.1 AAC,
// ffmpeg only). A copy session's URIs carry c=, the fingerprint of the file
// it began on: another file at the item's path answers 404, so a player
// never mixes two files' segments.

// onTheFlyPlan is how a session of an on-the-fly source plays.
type onTheFlyPlan struct {
	// mode: passthrough (the muxed stream copy of an MP4), remux (the
	// stream copy) or transcode (the encode path).
	mode, reason string
	// ix and audio are a stream copy's index and each audio track's plan.
	ix    *streamcopy.Index
	audio []trackPlan
	// fallback is why a session whose video the client decodes as it is
	// takes the encode path ("index", "plan"), "" for none.
	fallback string
}

// converts says whether a stream copy session converts any audio track.
func (p onTheFlyPlan) converts() bool {
	for _, a := range p.audio {
		if !a.copy {
			return true
		}
	}
	return false
}

// readThroughWhy is the reason of a session of a file read through for its
// index.
const readThroughWhy = "the file is read through once before it can play as it is"

// planOnTheFly is how a session of src plays for a client with caps asking
// for the rung q: the stream copy where the client decodes the video as it
// is and the original's index is at hand, else the encode path.
func (h *HLSHandler) planOnTheFly(ctx context.Context, src *source, caps Caps, q string) onTheFlyPlan {
	p := src.probe
	audio := len(p.AudioTracks) > 0
	if src.fromPackage() {
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(packageWhy(src, caps), audio)}
	}
	if q != "" && q != "high" {
		what := "The video is converted to H.264 at the quality picked"
		if audio {
			what += ", and the audio to stereo AAC"
		}
		return onTheFlyPlan{mode: "transcode", reason: what + "."}
	}
	sp := planStreams(p, caps)
	if !sp.videoCopy {
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(sp.videoWhy, audio)}
	}
	if sp.muxed {
		if isoBMFF(p.Container) {
			return onTheFlyPlan{mode: "passthrough", reason: "The video and the audio play as they are in the file."}
		}
		if h.Live.On() {
			// The live pipeline stream-copies what mp4ff reads only.
			return onTheFlyPlan{mode: "transcode", reason: encodeReason(formatWhy(p.Container), audio)}
		}
		switch h.passPlanState(src) {
		case planReady:
			return onTheFlyPlan{mode: "passthrough", reason: "The video and the audio play as they are in the file."}
		case planFailed:
			return onTheFlyPlan{mode: "transcode", reason: encodeReason("the file could not be played as it is", audio), fallback: "plan"}
		}
		h.queuePassPlan(src)
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(readThroughWhy, audio), fallback: "index"}
	}
	if !isMatroska(p.Container) && !isoBMFF(p.Container) {
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(formatWhy(p.Container), audio)}
	}
	ix, state, _ := h.streamIndex(ctx, src)
	switch {
	case state == indexPending:
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(readThroughWhy, audio), fallback: "index"}
	case state == indexFailed || len(ix.Audio) != len(p.AudioTracks):
		return onTheFlyPlan{mode: "transcode", reason: encodeReason("the file could not be played as it is", audio), fallback: "plan"}
	}
	// The file's own Dolby Vision record, whatever the probe said of it.
	if why := indexDolbyVisionWhy(ix, caps); why != "" {
		return onTheFlyPlan{mode: "transcode", reason: encodeReason(why, audio)}
	}
	for i := range sp.audio {
		if a := &sp.audio[i]; a.copy && !ix.Audio[i].Copyable() {
			a.copy, a.why = false, codecDisplay(p.AudioTracks[i].Codec)
			a.surround = caps.AACMultichannel && p.AudioTracks[i].Channels > 2
		}
	}
	return onTheFlyPlan{mode: "remux", reason: copyReason(sp, p), ix: ix, audio: sp.audio}
}

// formatWhy is why a file of the container format is converted.
func formatWhy(format string) string {
	name := strings.ToUpper(strings.Split(format, ",")[0])
	switch name {
	case "MPEGTS":
		name = "MPEG-TS"
	case "MOV":
		name = "QuickTime"
	}
	return "the file is in " + name + " format"
}

// packageWhy is why a client is served a package's video encoded: it
// decodes none of its rungs at its height.
func packageWhy(src *source, caps Caps) string {
	p := src.probe
	fam := videoFamily(p.VideoCodec)
	if fam != "" && caps.Video[p.VideoCodec] {
		if maxH := caps.VideoMaxHeight[fam]; maxH > 0 && p.Height > maxH {
			return fmt.Sprintf("this device plays %s up to %dp", codecDisplay(p.VideoCodec), maxH)
		}
	}
	return "this device does not play " + codecDisplay(p.VideoCodec)
}

// indexDolbyVisionWhy is why a client with caps is not served the video of
// the index ix as it is, from the file's own Dolby Vision record (a probe may
// not say it is there): one with an enhancement layer (profile 7), or one
// without a base layer other decoders play (profile 5) to a client that does
// not declare Dolby Vision; "" when it is served.
func indexDolbyVisionWhy(ix *streamcopy.Index, caps Caps) string {
	v := ix.Video
	switch {
	case v.DVProfile == 7:
		return "this device does not play Dolby Vision with an enhancement layer"
	case (v.DVProfile == 5 || v.DVProfile > 0 && v.DVCompatibility == 0) && !caps.DolbyVision:
		return "this device does not play Dolby Vision"
	}
	return ""
}

// The states of a muxed stream copy's plan of a container mp4ff does not
// read (MPEG-TS): its keyframes are probed by reading the file through, in
// the background.
const (
	planNone   = iota // not made: queued now
	planReady         // in the cache, or kept with the persisted indexes
	planFailed        // the file could not be read for one
)

// passPlanState is the state of the muxed stream copy's plan of src: in the
// item's cache, else kept with the persisted indexes (copy-index/, which the
// cache's sweep leaves alone: put back in the item's cache then), else
// failed, for a while or until the file changes.
func (h *HLSHandler) passPlanState(src *source) int {
	planPath := h.cachePath(src.key, "copy-"+copyPipelineVersion, "plan")
	if statOK(planPath) {
		return planReady
	}
	st, err := os.Stat(src.file)
	if err != nil {
		return planNone
	}
	if until, ok := h.passPlanFails.Load(src.file); ok && time.Now().Before(until.(time.Time)) {
		return planFailed
	}
	dir := h.indexDir()
	if dir == "" {
		return planNone
	}
	p, ok := readPersistedPlan(dir, src.file, st)
	switch {
	case !ok:
		return planNone
	case p.Failed != "":
		return planFailed
	}
	if b, err := json.Marshal(p.Plan); err != nil || writeFileAtomic(planPath, b) != nil {
		return planNone
	}
	return planReady
}

// passPlanRetry is how long a muxed stream copy's plan that could not be made
// (ffprobe could not read the file through) is not tried again.
const passPlanRetry = time.Hour

// queuePassPlan has the muxed stream copy's plan of src made in the
// background, and kept with the persisted indexes.
func (h *HLSHandler) queuePassPlan(src *source) {
	st, err := os.Stat(src.file)
	if err != nil {
		return
	}
	key, file := src.key, src.file
	j := copyScanJob{path: file, size: st.Size(), mtime: st.ModTime(), dir: h.indexDir()}
	j.passPlan = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		plan, err := h.loadOrBuildPlanAt(ctx, key, file, copyScanRate)
		if err != nil {
			log.Printf("stream copy plan of %s: %v — the encode path for %v", src.label(), err, passPlanRetry)
			h.passPlanFails.Store(file, time.Now().Add(passPlanRetry))
			return
		}
		writePersistedPlan(j.dir, file, st, plan, "")
	}
	h.streamIndexes.enqueue(j, h)
}

// passPlanFailed notes that the muxed stream copy's plan of the job's file
// could not be made (the build panicked): kept until the file changes.
func (h *HLSHandler) passPlanFailed(j copyScanJob, err error) {
	h.passPlanFails.Store(j.path, time.Now().Add(passPlanRetry))
	if st, serr := os.Stat(j.path); serr == nil && st.Size() == j.size && st.ModTime().Equal(j.mtime) {
		writePersistedPlan(j.dir, j.path, st, nil, err.Error())
	}
}

// copyLive says whether a stream copy session converts its audio on the live
// worker (LIVE_WORKER_MODE on, the worker ready): its URIs then carry lw=1.
// The copy itself is Go's whatever the mode.
func (h *HLSHandler) copyLive(ctx context.Context, plan onTheFlyPlan) bool {
	if !h.Live.On() || !plan.converts() {
		return false
	}
	if h.Live.Ready(ctx) {
		return true
	}
	metrics.LiveWorkerFallbacks.WithLabelValues("unready").Inc()
	return false
}

// copyParam is the query parameter of a copy session's fingerprint.
const copyParam = "c"

// withQueryParam is raw with key=value, any value it had replaced.
func withQueryParam(raw, key, value string) string {
	var out []string
	for _, kv := range strings.Split(raw, "&") {
		if kv != "" && kv != key && !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return strings.Join(append(out, key+"="+value), "&")
}

// serveCopyMaster serves the master of a stream copy session of src for a
// client with caps, and warms its start.
func (h *HLSHandler) serveCopyMaster(w http.ResponseWriter, r *http.Request, src *source, caps Caps, plan onTheFlyPlan) {
	ix := plan.ix
	live := h.copyLive(r.Context(), plan)
	q := withQueryParam(r.URL.RawQuery, copyParam, h.copyFingerprintOf(src, ix))
	switch {
	case live:
		q = withLivePin(q)
	case h.Live != nil:
		q = withoutLivePin(q)
	}
	q = "?" + q
	group, surround := copyRenditions(src.probe, ix, plan.audio, caps, live)
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	for _, g := range []struct {
		id      string
		choices []audioChoice
	}{{onTheFlyAudioGroup, group}, {onTheFlySurroundGroup, surround}} {
		for _, c := range g.choices {
			sb.WriteString(fmt.Sprintf(
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=%q,NAME=%q,LANGUAGE=%q,DEFAULT=%s,AUTOSELECT=YES,CHANNELS=\"%d\",URI=\"%s%s\"\n",
				g.id, c.name, c.language, yesNo(c.isDefault), c.channels, c.uri, q))
		}
	}
	uri := "video/index.m3u8" + q + "\n"
	sb.WriteString(copyStreamInf(ix, src.probe, caps, onTheFlyAudioGroup, group) + "\n" + uri)
	if surround != nil {
		sb.WriteString(copyStreamInf(ix, src.probe, caps, onTheFlySurroundGroup, surround) + "\n" + uri)
	}
	h.warmCopySession(src, ix, plan, group, live, tParam(r))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(sb.String()))
}

// copyRenditions are the audio groups of a stream copy master: for a player
// that picks among one group's renditions (hls.js, Media3), one group, each
// track copied or converted (its 5.1 conversion, for a client that decodes
// it, just before its stereo one); for a native player Apple's shape: the
// stereo group — each track as AAC, copied when it is stereo AAC — and,
// when a track plays in surround, a 5.1 group of the same members, each
// played by its surround rendition where it has one.
func copyRenditions(p *Probe, ix *streamcopy.Index, plan []trackPlan, caps Caps, live bool) (group, surround []audioChoice) {
	tracks := p.AudioTracks
	names := audioRenditionNames(tracks)
	def := defaultAudioIndex(tracks)
	copyChoice := func(i int, isDefault bool) audioChoice {
		t := tracks[i]
		return audioChoice{uri: fmt.Sprintf("audiocopy%d/index.m3u8", t.Index), rendition: fmt.Sprintf("audiocopy%d", t.Index),
			name: names[i], language: langOr(t.Language), codec: ix.Audio[i].Codec, channels: max(1, t.Channels), isDefault: isDefault}
	}
	surroundChoice := func(i int, isDefault bool) audioChoice {
		t := tracks[i]
		return audioChoice{uri: fmt.Sprintf("surround%d/index.m3u8", t.Index), rendition: fmt.Sprintf("surround%d", t.Index),
			name: names[i] + " 5.1", language: langOr(t.Language), codec: "mp4a.40.2", channels: 6, bitrate: surroundAudioBitrateBps,
			isDefault: isDefault}
	}
	// The worker converts to stereo only.
	surroundOK := func(i int) bool { return plan[i].surround && !live }
	if !caps.Native {
		for i, t := range tracks {
			isDef := t.Index == def
			switch {
			case plan[i].copy:
				group = append(group, copyChoice(i, isDef))
			case surroundOK(i):
				group = append(group, surroundChoice(i, isDef), stereoChoice(t, names[i], false))
			default:
				group = append(group, stereoChoice(t, names[i], isDef))
			}
		}
		uniqueChoiceNames(group)
		return group, nil
	}
	hasSurround := false
	for i, t := range tracks {
		isDef := t.Index == def
		stereoAAC := plan[i].copy && ix.Audio[i].Codec != "" && strings.HasPrefix(ix.Audio[i].Codec, "mp4a") && t.Channels <= 2
		member := stereoChoice(t, names[i], isDef)
		if stereoAAC {
			member = copyChoice(i, isDef)
		}
		group = append(group, member)
		switch {
		case plan[i].copy && !stereoAAC:
			c := copyChoice(i, isDef)
			surround = append(surround, c)
			hasSurround = true
		case surroundOK(i):
			c := surroundChoice(i, isDef)
			c.name = names[i]
			surround = append(surround, c)
			hasSurround = true
		default:
			m := member
			m.channels = max(1, min(2, m.channels))
			surround = append(surround, m)
		}
	}
	if !hasSurround {
		surround = nil
	}
	uniqueChoiceNames(group)
	for i := range surround {
		surround[i].name = group[i].name
	}
	return group, surround
}

func langOr(l string) string {
	if l == "" {
		return "und"
	}
	return l
}

// surroundAudioBitrate is the bit rate of a 5.1 AAC conversion.
const (
	surroundAudioBitrate    = "384k"
	surroundAudioBitrateBps = 384_000
)

// copyStreamInf is the #EXT-X-STREAM-INF line of a stream copy's variant with
// the audio group of choices: BANDWIDTH the copy's peak (a Matroska file's
// segments hold its audio as well) and the group's largest converted
// rendition, CODECS the video's (its Dolby Vision form for a client that
// plays it) and every audio codec of the group, VIDEO-RANGE the source's.
func copyStreamInf(ix *streamcopy.Index, p *Probe, caps Caps, groupID string, choices []audioChoice) string {
	peak, avg := ix.Bitrates()
	video := ix.Video.Codec
	if caps.DolbyVision && ix.Video.DVProfile > 0 && ix.Video.CodecDV != "" {
		video = ix.Video.CodecDV
	}
	codecs := []string{video}
	extra := 0
	for _, c := range choices {
		if !containsFold(codecs[1:], c.codec) {
			codecs = append(codecs, c.codec)
		}
		if !strings.HasPrefix(c.rendition, copiedPrefix) {
			extra = max(extra, c.bitrate)
		}
	}
	group := ""
	if len(choices) > 0 {
		group = fmt.Sprintf(",AUDIO=%q", groupID) // a file without audio has no group
	}
	return fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d%s,CODECS=%q,VIDEO-RANGE=%s%s",
		peak+extra, avg+extra, resolutionAttr(ix.Video.Width, ix.Video.Height), strings.Join(codecs, ","), videoRange(p), group)
}

// copyFingerprintOf is the fingerprint of the file the index ix of src is of.
func (h *HLSHandler) copyFingerprintOf(src *source, ix *streamcopy.Index) string {
	return copyFingerprint(src.file, ix.Size, time.Unix(0, ix.ModTime))
}

// copyRequest is the original and its ready index a stream copy request
// reads; false (answered) when there is none, or the request's session began
// on another file (c=).
func (h *HLSHandler) copyRequest(w http.ResponseWriter, r *http.Request) (*source, *streamcopy.Index, bool) {
	src, err := h.requestSource(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, nil, false
	}
	if src.fromPackage() {
		http.Error(w, "stream copy needs the original", http.StatusNotFound)
		return nil, nil, false
	}
	// A session begun before its title failed goes on as long as its
	// segments can be made: the failure sends new sessions to the ladder.
	ix, state, why := h.streamIndex(r.Context(), src)
	c := r.URL.Query().Get(copyParam)
	switch {
	case ix == nil || state != indexReady && c == "":
		http.Error(w, "no stream copy of this file: "+why, http.StatusNotFound)
		return nil, nil, false
	case c != "" && c != h.copyFingerprintOf(src, ix):
		http.Error(w, "the file changed since the session began", http.StatusNotFound)
		return nil, nil, false
	}
	return src, ix, true
}

// copyPlaylist writes the media playlist of a copied track: one segment per
// span of the index, its init and segments beside it.
func copyPlaylist(w http.ResponseWriter, r *http.Request, ix *streamcopy.Index) {
	q := r.URL.RawQuery
	if q != "" {
		q = "?" + q
	}
	spans := ix.Spans()
	target := 1
	for _, s := range spans {
		target = max(target, int(s.Dur+0.5))
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"init.mp4%s\"\n", q))
	for n, s := range spans {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%d.m4s%s\n", s.Dur, n, q))
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(sb.String()))
}

// serveBytes answers b, an init the index holds.
func serveBytes(w http.ResponseWriter, b []byte, ctype string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
	_, _ = w.Write(b)
}

// CopyVideoPlaylist serves video/index.m3u8.
func (h *HLSHandler) CopyVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	if _, ix, ok := h.copyRequest(w, r); ok {
		copyPlaylist(w, r, ix)
	}
}

// CopyVideoInit serves video/init.mp4: the video's header, with its
// Dolby Vision configuration for a client that plays it.
func (h *HLSHandler) CopyVideoInit(w http.ResponseWriter, r *http.Request) {
	_, ix, ok := h.copyRequest(w, r)
	if !ok {
		return
	}
	init := ix.Video.Init
	if ParseCaps(r.URL.Query().Get("caps")).DolbyVision && len(ix.Video.InitDV) > 0 {
		init = ix.Video.InitDV
	}
	serveBytes(w, init, "video/mp4")
}

// CopyVideoSegment serves video/{n}.m4s, and has the next segments made
// ahead of the play head.
func (h *HLSHandler) CopyVideoSegment(w http.ResponseWriter, r *http.Request) {
	src, ix, ok := h.copyRequest(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(chi.URLParam(r, "seg"))
	if err != nil || n < 0 || n >= len(ix.Spans()) {
		http.Error(w, "segment past end", http.StatusNotFound)
		return
	}
	if !h.serveCopySegment(w, r, src, ix, -1, n) {
		return
	}
	for k := n + 1; k <= n+copyPrefetchAhead && k < len(ix.Spans()); k++ {
		h.prefetchCopy(src, ix, k)
	}
}

// CopyAudioPlaylist serves audiocopy{n}/index.m3u8: the segments of the
// video's, of the audio track n.
func (h *HLSHandler) CopyAudioPlaylist(w http.ResponseWriter, r *http.Request) {
	if _, ix, ok := h.copyRequest(w, r); ok && copiedTrack(w, r, ix) >= 0 {
		copyPlaylist(w, r, ix)
	}
}

// CopyAudioInit serves audiocopy{n}/init.mp4.
func (h *HLSHandler) CopyAudioInit(w http.ResponseWriter, r *http.Request) {
	if _, ix, ok := h.copyRequest(w, r); ok {
		if a := copiedTrack(w, r, ix); a >= 0 {
			serveBytes(w, ix.Audio[a].Init, "video/mp4")
		}
	}
}

// CopyAudioSegment serves audiocopy{n}/{seg}.m4s.
func (h *HLSHandler) CopyAudioSegment(w http.ResponseWriter, r *http.Request) {
	src, ix, ok := h.copyRequest(w, r)
	if !ok {
		return
	}
	a := copiedTrack(w, r, ix)
	if a < 0 {
		return
	}
	n, err := strconv.Atoi(chi.URLParam(r, "seg"))
	if err != nil || n < 0 || n >= len(ix.Spans()) {
		http.Error(w, "segment past end", http.StatusNotFound)
		return
	}
	h.serveCopySegment(w, r, src, ix, a, n)
}

// copiedPrefix and surroundPrefix name the folders of a copied audio track
// and of a 5.1 conversion: audiocopy0, surround1.
const (
	copiedPrefix   = "audiocopy"
	surroundPrefix = "surround"
)

// trackParam is the audio track a request's {track} folder names after
// prefix, -1 when it names none.
func trackParam(r *http.Request, prefix string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(chi.URLParam(r, "track"), prefix))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// copiedTrack is the audio track an audiocopy{n} request names, -1
// (answered) when the copy does not carry it.
func copiedTrack(w http.ResponseWriter, r *http.Request, ix *streamcopy.Index) int {
	a := trackParam(r, copiedPrefix)
	if a < 0 || a >= len(ix.Audio) || !ix.Audio[a].Copyable() {
		http.Error(w, "no copied audio track "+chi.URLParam(r, "track"), http.StatusNotFound)
		return -1
	}
	return a
}

// copyCacheBudget bounds the bytes of the copy segments kept in memory. A
// copy segment is remade by one read of the original, so none is written to
// the disk cache: a 4K original's run to tens of megabytes each, which a
// cache volume of a few gigabytes would not hold for long.
var copyCacheBudget int64 = 384 << 20

// copyCache keeps the copy segments made, in memory, the least recently used
// dropped beyond copyCacheBudget.
type copyCache struct {
	mu    sync.Mutex
	m     map[string]*copySegment
	bytes int64
}

// copySegment is one segment of a copy: its video and each copied audio
// track's fragment.
type copySegment struct {
	video []byte
	audio map[int][]byte
	size  int64
	used  time.Time
}

// track is the fragment of track (-1: the video), nil when it has none.
func (s *copySegment) track(track int) []byte {
	if track < 0 {
		return s.video
	}
	return s.audio[track]
}

func (c *copyCache) get(key string) *copySegment {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.m[key]
	if s != nil {
		s.used = time.Now()
	}
	return s
}

func (c *copyCache) put(key string, s *copySegment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*copySegment{}
	}
	if old := c.m[key]; old != nil {
		c.bytes -= old.size
	}
	s.used = time.Now()
	c.m[key] = s
	c.bytes += s.size
	for c.bytes > copyCacheBudget && len(c.m) > 1 {
		oldest := ""
		for k, x := range c.m {
			if k != key && (oldest == "" || x.used.Before(c.m[oldest].used)) {
				oldest = k
			}
		}
		c.bytes -= c.m[oldest].size
		delete(c.m, oldest)
	}
}

// copyKey names segment n of the copy of src in the copy cache.
func (h *HLSHandler) copyKey(src *source, ix *streamcopy.Index, n int) string {
	return h.copyFingerprintOf(src, ix) + "/" + strconv.Itoa(n)
}

// serveCopySegment serves segment n of track (-1: the video) of the copy of
// src, making it unless it is in the cache. A segment the copy cannot make
// is 404 — the player falls back to the encode path — and, when the cause is
// the file, the title's new sessions take the encode path; one the file
// could not be read for is 503, to be asked for again.
func (h *HLSHandler) serveCopySegment(w http.ResponseWriter, r *http.Request, src *source, ix *streamcopy.Index, track, n int) bool {
	key := h.copyKey(src, ix, n)
	if h.copyTries.refused(key) {
		http.Error(w, "segment unavailable", http.StatusNotFound)
		return false
	}
	seg, err := h.ensureCopySegment(r.Context(), src, ix, n, false)
	if err != nil {
		if r.Context().Err() != nil {
			return false
		}
		log.Printf("stream copy %s segment %d: %v", src.label(), n, err)
		switch {
		case errors.Is(err, errOriginalChanged):
			http.Error(w, "the file changed since the session began", http.StatusNotFound)
		case permanentCopyError(err):
			// The file's: the player steps down, and the title's new sessions
			// take the encode path; the segment is not made again.
			h.copyTries.refuse(key, permanentRefusal)
			h.copyFailed(src, "segment", err)
			http.Error(w, "segment unavailable", http.StatusNotFound)
		case h.copyTries.failed(key) >= copySegmentTries:
			// The storage's, too often: so for a while.
			h.copyTries.refuse(key, time.Now().Add(copyIndexRetry))
			h.copyFailed(src, "segment", err)
			http.Error(w, "segment unavailable", http.StatusNotFound)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "segment could not be read", http.StatusServiceUnavailable)
		}
		return false
	}
	h.copyTries.clear(key)
	b := seg.track(track)
	if b == nil {
		http.Error(w, "segment unavailable", http.StatusNotFound)
		return false
	}
	w.Header().Set("Content-Type", "video/iso.segment")
	w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	return true
}

// copyPrefetchAhead is how many segments after the one served are made ahead
// of the play head.
const copyPrefetchAhead = 2

// copySlots bounds the copy segments made at once on request, and
// copyPrefetchSlots those made ahead of the play head and for warms: each
// reads tens of megabytes.
var (
	copySlots         = make(chan struct{}, 4)
	copyPrefetchSlots = make(chan struct{}, 2)
)

// copySegmentTries is how many passing failures in a row (the storage failing
// a read) a copy segment is answered 503 for before the title's new sessions
// take the encode path and the player is answered 404.
const copySegmentTries = 3

// copyTries counts the passing failures of copy segments, by key, and
// keeps the segments that cannot be made, answered 404 at once until when
// (a player asks for a segment it was answered 404 for again).
type copyTries struct {
	mu   sync.Mutex
	n    map[string]int
	none map[string]time.Time
}

// permanentRefusal is the until of a segment the file is the cause of: its
// key names the file, so it holds until the file changes.
var permanentRefusal = time.Unix(1<<40, 0)

// failed counts one more passing failure of key, and says how many in a row.
func (c *copyTries) failed(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil || len(c.n) > 4096 {
		c.n = map[string]int{}
	}
	c.n[key]++
	return c.n[key]
}

func (c *copyTries) clear(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.n, key)
}

// refuse has the segment key answered 404 at once until until.
func (c *copyTries) refuse(key string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.none == nil || len(c.none) > 4096 {
		c.none = map[string]time.Time{}
	}
	c.none[key] = until
	delete(c.n, key)
}

// refused says whether the segment key is answered 404 at once.
func (c *copyTries) refused(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.none[key]
	if ok && !time.Now().Before(until) {
		delete(c.none, key)
		return false
	}
	return ok
}

// copyMake is a copy segment being made: its result once done is closed.
type copyMake struct {
	done chan struct{}
	seg  *copySegment
	err  error
}

// wait is the make's result, or ctx's error when ctx ends first.
func (m *copyMake) wait(ctx context.Context) (*copySegment, error) {
	select {
	case <-m.done:
		return m.seg, m.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// retryable says whether a copy segment's error is worth one more try at
// once: a read the storage failed.
func retryable(err error) bool {
	return err != nil && transientCopyError(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// ensureCopySegment is segment n of the copy of src: the copy cache's, else
// made — its video and every audio track the copy carries, from one read of
// the original. One make per segment at a time, which a request for it joins
// for as long as its context lasts; a make takes its slot before it is
// registered, so a prefetch waiting for a slot of its own (background) holds
// up no request for the same segment.
func (h *HLSHandler) ensureCopySegment(ctx context.Context, src *source, ix *streamcopy.Index, n int, background bool) (seg *copySegment,
	err error) {
	key := h.copyKey(src, ix, n)
	if s := h.copyCache.get(key); s != nil {
		return s, nil
	}
	h.copyMu.Lock()
	m := h.copyMakes[key]
	h.copyMu.Unlock()
	if m != nil {
		return m.wait(ctx)
	}
	slots := copySlots
	if background {
		slots = copyPrefetchSlots
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h.copyMu.Lock()
	if m := h.copyMakes[key]; m != nil { // made meanwhile, or being made
		h.copyMu.Unlock()
		<-slots
		return m.wait(ctx)
	}
	if s := h.copyCache.get(key); s != nil {
		h.copyMu.Unlock()
		<-slots
		return s, nil
	}
	if h.copyMakes == nil {
		h.copyMakes = map[string]*copyMake{}
	}
	m = &copyMake{done: make(chan struct{})}
	h.copyMakes[key] = m
	h.copyMu.Unlock()

	// Whatever happens, the slot goes back and the requests that joined get
	// the make's result.
	defer func() {
		if r := recover(); r != nil {
			m.seg, m.err = nil, recoveredCopy(fmt.Sprintf("segment %d of %s", n, src.label()), r)
		}
		<-slots
		h.copyMu.Lock()
		delete(h.copyMakes, key)
		h.copyMu.Unlock()
		close(m.done)
		seg, err = m.seg, m.err
	}()
	m.seg, m.err = h.makeCopySegment(src, ix, n, key)
	return m.seg, m.err
}

// makeCopySegment makes segment n of the copy of src (key) and keeps it in
// the copy cache: one more try at once when the storage failed a read.
func (h *HLSHandler) makeCopySegment(src *source, ix *streamcopy.Index, n int, key string) (*copySegment, error) {
	var tracks []int
	for i, a := range ix.Audio {
		if a.Copyable() {
			tracks = append(tracks, i)
		}
	}
	t0 := time.Now()
	var out *streamcopy.Output
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if out, err = produceCopy(src.file, ix, n, tracks); !retryable(err) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	seg := &copySegment{video: out.Video, audio: out.Audio, size: int64(len(out.Video))}
	for _, b := range out.Audio {
		seg.size += int64(len(b))
	}
	h.copyCache.put(key, seg)
	metrics.ObserveSegment("copy", time.Since(t0), ix.Spans()[n].Dur)
	metrics.BytesServed.WithLabelValues("copy-read").Add(float64(out.Read))
	return seg, nil
}

// produceHook, when set (tests), runs before each copy segment is made.
var produceHook func(n int)

// produceCopy makes segment n of the copy of the original at path: a panic
// of it an error of the file's.
func produceCopy(path string, ix *streamcopy.Index, n int, tracks []int) (out *streamcopy.Output, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, recoveredCopy(fmt.Sprintf("segment %d of %s", n, filepath.Base(path)), r)
		}
	}()
	if produceHook != nil {
		produceHook(n)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() != ix.Size || st.ModTime().UnixNano() != ix.ModTime {
		return nil, errOriginalChanged
	}
	return ix.Produce(f, n, tracks)
}

// prefetchCopy has segment n of the copy of src made in the background,
// unless it is there or no prefetch slot is free. A segment the copy cannot
// make there fails the title as one asked for would.
func (h *HLSHandler) prefetchCopy(src *source, ix *streamcopy.Index, n int) {
	if h.copyCache.get(h.copyKey(src, ix, n)) != nil {
		return
	}
	h.copyWork.Add(1)
	goWarm(func() {
		defer h.copyWork.Done()
		defer func() {
			if r := recover(); r != nil {
				h.copyFailed(src, "segment", recoveredCopy(fmt.Sprintf("the prefetch of segment %d of %s", n, src.label()), r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := h.ensureCopySegment(ctx, src, ix, n, true)
		switch {
		case err == nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, errOriginalChanged):
		case permanentCopyError(err):
			h.copyTries.refuse(h.copyKey(src, ix, n), permanentRefusal)
			h.copyFailed(src, "segment", err)
		default:
			log.Printf("stream copy %s prefetch %d: %v", src.label(), n, err)
		}
	})
}

// warmCopySession warms the start of a stream copy session off the request
// path: the segment at t (its video and copied audio), and the audio track it
// starts on when that is converted — an ffmpeg window, or the worker's
// segments.
func (h *HLSHandler) warmCopySession(src *source, ix *streamcopy.Index, plan onTheFlyPlan, group []audioChoice, live bool, tSec float64) {
	k := 0
	for i, s := range ix.Spans() {
		if s.Start <= tSec {
			k = i
		}
	}
	h.prefetchCopy(src, ix, k)
	start := unionDefault(group)
	if start == nil || strings.HasPrefix(start.rendition, copiedPrefix) {
		return
	}
	idx := defaultAudioIndex(src.probe.AudioTracks)
	if live {
		k0 := liveSegmentAt(src.probe.DurationMs, tSec)
		for _, k := range append([]int{-1}, liveRange(src.probe.DurationMs, k0, liveWarmSegments)...) {
			if j, ok := h.liveAudioJob(src, idx, k, "prefetch"); ok {
				h.prefetchLive(j)
			}
		}
		return
	}
	seg := liveSegmentAt(src.probe.DurationMs, tSec)
	channels := 2
	if strings.HasPrefix(start.rendition, surroundPrefix) {
		channels = 6
	}
	goWarm(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := h.ensureAudioWindowCh(withWarmContext(ctx), src, idx, seg/windowSize, seg, channels); err != nil {
			log.Printf("warm copy session audio %s: %v", src.key, err)
		}
	})
}

// The 5.1 AAC conversions: surround{n}/, as audio/{n}/ but for six
// channels (ffmpeg only).

// SurroundAudioPlaylist serves surround{n}/index.m3u8: the grid, as
// audio/{n}/index.m3u8 lists it.
func (h *HLSHandler) SurroundAudioPlaylist(w http.ResponseWriter, r *http.Request) {
	src, err := h.requestSource(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if a := trackParam(r, surroundPrefix); a < 0 || src.probe == nil || a >= len(src.probe.AudioTracks) {
		http.Error(w, "no audio track "+chi.URLParam(r, "track"), http.StatusNotFound)
		return
	}
	gridPlaylist(w, r, src)
}

// SurroundAudioInit serves surround{n}/init.mp4.
func (h *HLSHandler) SurroundAudioInit(w http.ResponseWriter, r *http.Request) {
	h.serveAudioWindow(w, r, 6, -1)
}

// SurroundAudioSegment serves surround{n}/{seg}.m4s.
func (h *HLSHandler) SurroundAudioSegment(w http.ResponseWriter, r *http.Request) {
	seg, err := strconv.Atoi(chi.URLParam(r, "seg"))
	if err != nil || seg < 0 {
		http.Error(w, "bad segment number", http.StatusBadRequest)
		return
	}
	h.serveAudioWindow(w, r, 6, seg)
}

// serveAudioWindow serves the init (seg < 0) or segment seg of an audio
// rendition of channels converted by ffmpeg windows.
func (h *HLSHandler) serveAudioWindow(w http.ResponseWriter, r *http.Request, channels, seg int) {
	audioIdx := trackParam(r, surroundPrefix)
	if audioIdx < 0 {
		http.Error(w, "bad audio index", http.StatusBadRequest)
		return
	}
	src, err := h.requestSource(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if src.probe == nil || audioIdx >= len(src.probe.AudioTracks) {
		http.Error(w, "the source has no audio track "+strconv.Itoa(audioIdx), http.StatusNotFound)
		return
	}
	win, want := 0, 0
	if seg >= 0 {
		if total := float64(src.probe.DurationMs) / 1000; float64(seg*segmentSec) >= total {
			http.Error(w, "segment past end", http.StatusNotFound)
			return
		}
		win, want = seg/windowSize, seg
	}
	if err := h.ensureAudioWindowCh(r.Context(), src, audioIdx, win, want, channels); err != nil {
		log.Printf("hls audio %d ch %s/%d/%d: %v", channels, src.key, audioIdx, seg, err)
		windowError(w, err, "audio segment")
		return
	}
	ns := audioCacheKey(audioIdx, channels)
	if seg < 0 {
		h.serveCached(w, r, h.cachePath(src.key, ns, "init"), "video/mp4")
		return
	}
	path := h.cachePath(src.key, ns, strconv.Itoa(seg))
	if !statOK(path) {
		http.Error(w, "segment unavailable", http.StatusNotFound)
		return
	}
	h.serveCached(w, r, path, "video/iso.segment")
}
