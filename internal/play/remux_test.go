package play

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zaentrum/live-worker/cmaf"

	"github.com/zaentrum/chino-stream/internal/catalog"
	"github.com/zaentrum/chino-stream/internal/metrics"
)

// copyOriginal makes, with a real ffmpeg, a 20 s original of H.264 (B-frames,
// a keyframe every 2 s) and stereo AAC at 48 kHz in an MP4 — fragmented with
// fragmented — in a media root of its own.
func copyOriginal(t *testing.T, ffmpeg string, fragmented bool) (root, file string) {
	t.Helper()
	if !allEncoders(t, ffmpeg)["libx264"] || !allEncoders(t, ffmpeg)["aac"] {
		t.Skip("this ffmpeg has no libx264 or aac")
	}
	root = t.TempDir()
	file = filepath.Join(root, "film.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=20",
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "2", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-c:a", "aac", "-ac", "2", "-b:a", "96k", "-shortest"}
	if fragmented {
		args = append(args, "-movflags", "+frag_keyframe+empty_moov+default_base_moof")
	}
	run(t, ffmpeg, append(args, file)...)
	return root, file
}

// frameHashes are the hashes of the frames of the stream of input (v:0 or
// a:0), in order: decoded, or with packets the packets as they are.
func frameHashes(t *testing.T, ffmpeg, input, stream string, packets bool) []string {
	t.Helper()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", input, "-map", "0:" + stream}
	if packets {
		args = append(args, "-c", "copy")
	}
	out := run(t, ffmpeg, append(args, "-f", "framemd5", "-")...)
	var hashes []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		hashes = append(hashes, strings.TrimSpace(f[len(f)-1]))
	}
	return hashes
}

// On the live pipeline the stream copy is remuxed in Go: the session's
// master points at copy/ with the pin, its playlist is the one the ffmpeg path
// makes, its init is built from the original's moov and its segments read
// from the original by byte range — together they hold the original's
// packets, video and audio, exactly, and decode to its frames; ffmpeg never
// runs. So it is for a
// fragmented original, whose keyframes come from its trun flags.
func TestLiveStreamCopyIsRemuxedInGo(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	for _, fragmented := range []bool{false, true} {
		t.Run("fragmented="+strconv.FormatBool(fragmented), func(t *testing.T) {
			root, file := copyOriginal(t, ffmpeg, fragmented)
			fw := newFakeWorker(t)
			state := t.TempDir()
			h := &HLSHandler{Catalog: fakeKatalog(t, file), MediaRoot: root, FFmpegBin: windowFFmpeg(t, state, 99, 0, "no ffmpeg here"),
				FFprobeBin: ffprobe, CacheDir: t.TempDir(), Live: fw.live(LiveWorkerOn)}
			quiesce(t, h)
			const q = "?caps=avc,aac&lw=1"

			master := get(h, "/api/play/i1/master.m3u8?caps=avc,aac")
			if master.Code != 200 || !strings.Contains(master.Body.String(), "\ncopy/index.m3u8"+q+"\n") || strings.Contains(master.Body.String(), "EXT-X-MEDIA") {
				t.Fatalf("master: %d\n%s", master.Code, master.Body)
			}
			pl := get(h, "/api/play/i1/copy/index.m3u8"+q)
			if pl.Code != 200 {
				t.Fatalf("playlist: %d %q", pl.Code, pl.Body)
			}
			if !fragmented {
				off := &HLSHandler{Catalog: h.Catalog, MediaRoot: root, FFmpegBin: ffmpeg, FFprobeBin: ffprobe, CacheDir: t.TempDir()}
				want := get(off, "/api/play/i1/copy/index.m3u8?caps=avc,aac").Body.String()
				if got := strings.ReplaceAll(pl.Body.String(), "&lw=1", ""); got != want {
					t.Errorf("the live playlist:\n%s\nthe ffmpeg path's:\n%s", got, want)
				}
			}
			n := strings.Count(pl.Body.String(), "#EXTINF:")
			if n < 3 {
				t.Fatalf("%d segments:\n%s", n, pl.Body)
			}
			init := get(h, "/api/play/i1/copy/init.mp4"+q)
			if init.Code != 200 {
				t.Fatalf("init: %d %q", init.Code, init.Body)
			}
			joined := append([]byte(nil), init.Body.Bytes()...)
			for k := 0; k < n; k++ {
				w := get(h, "/api/play/i1/copy/"+strconv.Itoa(k)+".m4s"+q)
				if w.Code != 200 {
					t.Fatalf("segment %d: %d %q", k, w.Code, w.Body)
				}
				counts, err := sampleCounts(init.Body.Bytes(), w.Body.Bytes())
				if err != nil || counts["vide"] == 0 || counts["soun"] == 0 {
					t.Errorf("segment %d: %v %v", k, counts, err)
				}
				joined = append(joined, w.Body.Bytes()...)
			}
			if w := get(h, "/api/play/i1/copy/"+strconv.Itoa(n)+".m4s"+q); w.Code != 404 {
				t.Errorf("past the plan: %d", w.Code)
			}
			remux := filepath.Join(t.TempDir(), "remux.mp4")
			if err := os.WriteFile(remux, joined, 0o644); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				stream  string
				packets bool
			}{{"v:0", true}, {"a:0", true}, {"v:0", false}} {
				want, got := frameHashes(t, ffmpeg, file, c.stream, c.packets), frameHashes(t, ffmpeg, remux, c.stream, c.packets)
				if len(want) == 0 || !reflect.DeepEqual(got, want) {
					t.Errorf("%s (packets %v): the remux has %d, the original %d (equal: %v)", c.stream, c.packets, len(got), len(want), reflect.DeepEqual(got, want))
				}
			}
			if runs := ffmpegRuns(t, state); len(runs) != 0 {
				t.Errorf("ffmpeg ran %d times", len(runs))
			}
			if calls := fw.got(""); len(calls) != 0 {
				t.Errorf("the worker was asked %d times for a stream copy", len(calls))
			}
		})
	}
}

// The Go remux's plan of a progressive original is the ffmpeg path's: the
// same keyframes (mp4ff's), grouped alike, the probe's duration.
func TestCopyRemuxPlanIsTheFFmpegPaths(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	_, file := copyOriginal(t, ffmpeg, false)
	want, err := buildPassPlan(context.Background(), ffprobe, file)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := RunFFprobe(context.Background(), ffprobe, file)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := buildCopyIndex(file, probe.DurationMs)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(ix.plan)
	if !bytes.Equal(a, b) {
		t.Errorf("plan %s, the ffmpeg path's %s", b, a)
	}
	// Every video sample and every audio sample is in exactly one segment.
	v, au := 0, 0
	for k := range ix.plan.Segments {
		seg, err := ix.segment(k)
		if err != nil {
			t.Fatal(err)
		}
		c, err := sampleCounts(ix.init, seg)
		if err != nil {
			t.Fatal(err)
		}
		v, au = v+c["vide"], au+c["soun"]
	}
	if v != len(ix.video.samples) || au != len(ix.audio.samples) {
		t.Errorf("segments hold %d of %d video and %d of %d audio samples", v, len(ix.video.samples), au, len(ix.audio.samples))
	}
	// An original mp4ff cannot read is not stream-copied on the live pipeline.
	ts := filepath.Join(t.TempDir(), "film.ts")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-i", file, "-c", "copy", ts)
	tsProbe, err := RunFFprobe(context.Background(), ffprobe, ts)
	if err != nil {
		t.Fatal(err)
	}
	if isoBMFF(tsProbe.Container) || copyRemuxable(&source{key: "ts", file: ts, probe: &tsProbe}) {
		t.Errorf("an MPEG-TS original (%s) is stream-copied", tsProbe.Container)
	}
	if mode, _ := liveDecision(&LiveWorker{mode: LiveWorkerOn}, &tsProbe, "passthrough", ""); mode != "remux" {
		t.Errorf("an MPEG-TS original on the live pipeline: %s", mode)
	}
	if mode, _ := liveDecision(&LiveWorker{mode: LiveWorkerOn}, &probe, "passthrough", ""); mode != "passthrough" {
		t.Errorf("an MP4 original on the live pipeline: %s", mode)
	}
}

// LIVE_WORKER_MODE shadow, with a real ffmpeg: the Go remuxes are checked
// against what ffmpeg served — the stream copy's segments hold its video
// samples, a package's audio segments as many AAC frames as ffmpeg's
// re-encode of the rendition.
func TestLiveShadowChecksTheRemuxesAgainstFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	logs := captureLog(t)
	root, file := copyOriginal(t, ffmpeg, false)
	fw := newFakeWorker(t)
	h := &HLSHandler{Catalog: fakeKatalog(t, file), MediaRoot: root, FFmpegBin: ffmpeg, FFprobeBin: ffprobe, CacheDir: t.TempDir(),
		Live: fw.live(LiveWorkerShadow)}
	quiesce(t, h)
	copyOK := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("copy", "ok"))
	if w := get(h, "/api/play/i1/copy/init.mp4?caps=avc,aac"); w.Code != 200 {
		t.Fatalf("copy init: %d %q", w.Code, w.Body)
	}
	// The first segment: ffmpeg's -t cut keeps two frames past the next
	// keyframe, within the check's tolerance. (Past it, an ffmpeg that counts
	// -t from zero under -copyts writes two frames a segment: what the check
	// is there to find.)
	if w := get(h, "/api/play/i1/copy/0.m4s?caps=avc,aac"); w.Code != 200 {
		t.Fatalf("copy segment: %d %q", w.Code, w.Body)
	}
	settle(t, h.Live)
	if got := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("copy", "ok")); got != copyOK+1 {
		t.Errorf("copy checks ok: %v more, want 1\n%s", got-copyOK, logs)
	}

	aac, err := DetectAACEncoder(context.Background(), ffmpeg)
	if err != nil {
		t.Skip(err)
	}
	f := newFakeLibrary(t)
	f.setItem(catalog.Playback{ItemID: mediaItem, Package: &catalog.PackageRef{VersionID: mediaVersion, Dir: mediaDir(t), Record: "package.json"}})
	ph := &HLSHandler{Catalog: noAsset(t), Packages: f.resolver(), FFmpegBin: ffmpeg, CacheDir: t.TempDir(), TranscodePreset: "ultrafast",
		AACEncoder: aac, Live: fw.live(LiveWorkerShadow)}
	quiesce(t, ph)
	audioOK := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("package-audio", "ok"))
	for k := 0; k < 3; k++ {
		if w := get(ph, "/api/play/"+mediaItem+"/audio/0/"+strconv.Itoa(k)+".m4s"); w.Code != 200 {
			t.Fatalf("audio segment %d: %d %.80q", k, w.Code, w.Body)
		}
	}
	settle(t, ph.Live)
	if got := testutil.ToFloat64(metrics.LiveWorkerShadow.WithLabelValues("package-audio", "ok")); got != audioOK+3 {
		t.Errorf("package audio checks ok: %v more, want 3\n%s", got-audioOK, logs)
	}
}

// A fragment of the remux is CMAF a player appends: a styp, one moof with a
// traf per track, its first video sample a sync sample at the plan
// segment's start, decode times the original's.
func TestCopyRemuxSegmentShape(t *testing.T) {
	ffmpeg, _ := realFFmpeg(t)
	_, file := copyOriginal(t, ffmpeg, false)
	ix, err := buildCopyIndex(file, 20000)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := ix.segment(1)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := mp4.DecodeFile(bytes.NewReader(seg))
	if err != nil || len(mf.Segments) != 1 || mf.Segments[0].Styp == nil || len(mf.Segments[0].Fragments) != 1 {
		t.Fatalf("segment: %v", err)
	}
	moof := mf.Segments[0].Fragments[0].Moof
	if len(moof.Trafs) != 2 || moof.Trafs[0].Tfhd.TrackID != 1 || moof.Trafs[1].Tfhd.TrackID != 2 {
		t.Fatalf("trafs %+v", moof.Trafs)
	}
	v := ix.video.samples[ix.segStart[1]]
	if got := moof.Trafs[0].Tfdt.BaseMediaDecodeTime(); got != uint64(v.dts) || !v.sync {
		t.Errorf("video tfdt %d, the keyframe's decode time %d (sync %v)", got, v.dts, v.sync)
	}
	tr, err := cmaf.ReadInit(ix.init)
	if err != nil || tr.Kind != "video" || len(tr.Edits) == 0 {
		t.Errorf("init: %v %+v (the original's edit list kept)", err, tr)
	}
}

// A package's WebVTT from t on, as the worker makes an original's: cues
// ended by t go, the rest shifted (one under way from 0), sorted; an
// identifier, settings and text kept; NOTE blocks dropped, STYLE kept; a cue
// without an end lasts until the next.
func TestShiftWebVTT(t *testing.T) {
	doc := "\xEF\xBB\xBFWEBVTT - a title\r\n\r\nSTYLE\r\n::cue { color: yellow }\r\n\r\nNOTE a comment\r\n\r\n" +
		"intro\r\n00:00:05.000 --> 00:00:09.500 align:start\r\n<i>Under way</i>\r\n\r\n" +
		"00:20.000 --> 00:21.000\r\nShort form\r\n\r\n" +
		"00:00:01.000 --> 00:00:02.000\r\nGone\r\n\r\n" +
		"01:00:00.000 --> 01:00:00.000\r\nNo end\r\n\r\n" +
		"01:00:02.000 --> 01:00:03.000\r\nNext\r\n"
	got, err := shiftWebVTT([]byte(doc), 8)
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n\nSTYLE\n::cue { color: yellow }\n\n" +
		"intro\n00:00:00.000 --> 00:00:01.500 align:start\n<i>Under way</i>\n\n" +
		"00:00:12.000 --> 00:00:13.000\nShort form\n\n" +
		"00:59:52.000 --> 00:59:54.000\nNo end\n\n" +
		"00:59:54.000 --> 00:59:55.000\nNext\n\n"
	if string(got) != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if _, err := shiftWebVTT([]byte("1\n00:00:01,000 --> 00:00:02,000\nSubRip\n"), 0); err == nil {
		t.Error("a SubRip file is read as WebVTT")
	}
}
