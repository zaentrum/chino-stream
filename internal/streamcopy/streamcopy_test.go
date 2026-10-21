package streamcopy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// tool is ffmpeg or ffprobe from $PATH; the test is skipped without it.
func tool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on $PATH", name)
	}
	return p
}

// run runs a command, failing the test with its output when it fails.
func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "Unknown encoder") {
			t.Skip(stderr.String())
		}
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// makeFile makes a media file with ffmpeg: args after the inputs.
func makeFile(t *testing.T, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	run(t, tool(t, "ffmpeg"), append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...), "-y", out)...)
	return out
}

// lavfi are the inputs of the files made here: a 23.976 fps picture, and
// two tones, each dur seconds.
func lavfi(dur int) []string {
	d := strconv.Itoa(dur)
	return []string{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001:duration=" + d,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + d,
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration=" + d}
}

// probeStart is ffprobe's format start_time of file in nanoseconds, and its
// duration: the origin and the duration an index takes.
func probeStart(t *testing.T, file string) (start, dur int64) {
	t.Helper()
	out := run(t, tool(t, "ffprobe"), "-v", "error", "-show_entries", "format=start_time,duration", "-of", "json", file)
	var p struct {
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	s, _ := strconv.ParseFloat(p.Format.StartTime, 64)
	d, _ := strconv.ParseFloat(p.Format.Duration, 64)
	return int64(s * 1e9), int64(d * 1e9)
}

// index indexes file as the serving side does.
func index(t *testing.T, file string) *Index {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	start, dur := probeStart(t, file)
	o := Options{OriginNs: start, DurationNs: dur, Path: file, ModTime: st.ModTime().UnixNano()}
	var ix *Index
	if strings.HasSuffix(file, ".mp4") {
		ix, err = IndexMP4(f, st.Size(), o)
	} else {
		ix, err = IndexMatroska(f, st.Size(), o)
	}
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := ix.Check(); err != nil {
		t.Fatalf("check: %v", err)
	}
	return ix
}

// produceAll makes every segment of ix, returning each track's init with all
// its segments after it (video, then the audio tracks asked for).
func produceAll(t *testing.T, ix *Index, audio []int) (video []byte, aud map[int][]byte, segs []*Output) {
	t.Helper()
	f, err := os.Open(ix.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	video = append([]byte(nil), ix.Video.Init...)
	aud = map[int][]byte{}
	for _, a := range audio {
		aud[a] = append([]byte(nil), ix.Audio[a].Init...)
	}
	for n := range ix.Spans() {
		out, err := ix.Produce(f, n, audio)
		if err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
		video = append(video, out.Video...)
		for _, a := range audio {
			aud[a] = append(aud[a], out.Audio[a]...)
		}
		segs = append(segs, out)
	}
	if _, err := ix.Produce(f, len(ix.Spans()), audio); !errors.Is(err, ErrPastEnd) {
		t.Errorf("past the end: %v", err)
	}
	return video, aud, segs
}

func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// hashes are the MD5s of a stream's frames, in order: its packets as they
// are (packets), or decoded.
func hashes(t *testing.T, file, stream string, packets bool) []string {
	t.Helper()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", file, "-map", "0:" + stream}
	if packets {
		args = append(args, "-c", "copy")
	}
	out := run(t, tool(t, "ffmpeg"), append(args, "-f", "framemd5", "-")...)
	var hs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		hs = append(hs, strings.TrimSpace(f[5]))
	}
	return hs
}

// packet is a packet as ffprobe shows it.
type packet struct {
	PTS, DTS, Dur int64
	Key           bool
}

// packets are the packets of stream of file in its own timebase's ticks.
func packets(t *testing.T, file, stream string) []packet {
	t.Helper()
	out := run(t, tool(t, "ffprobe"), "-v", "error", "-select_streams", stream, "-show_entries", "packet=pts,dts,duration,flags", "-of", "csv=p=0", file)
	var ps []packet
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 4 {
			continue
		}
		p := packet{Key: strings.HasPrefix(f[3], "K")}
		p.PTS, _ = strconv.ParseInt(f[0], 10, 64)
		p.DTS, _ = strconv.ParseInt(f[1], 10, 64)
		p.Dur, _ = strconv.ParseInt(f[2], 10, 64)
		ps = append(ps, p)
	}
	return ps
}

// checkTiling checks a track of a copy, its init and segments joined: decode
// times rising, each packet lasting until the next one's (no gap and no
// overlap at any segment's edge), the first packet a keyframe.
func checkTiling(t *testing.T, joined, stream string) []packet {
	t.Helper()
	ps := packets(t, joined, stream)
	if len(ps) == 0 {
		t.Fatalf("%s: no packets", stream)
	}
	if !ps[0].Key {
		t.Errorf("%s: the first packet is no keyframe", stream)
	}
	for i := 1; i < len(ps); i++ {
		if ps[i].DTS != ps[i-1].DTS+ps[i-1].Dur {
			t.Errorf("%s: packet %d at %d, the one before ends at %d", stream, i, ps[i].DTS, ps[i-1].DTS+ps[i-1].Dur)
			break
		}
	}
	return ps
}

// fragments are the decode times at which each fragment of a joined track
// starts.
func fragmentStarts(t *testing.T, b []byte) []uint64 {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for _, s := range f.Segments {
		for _, fr := range s.Fragments {
			out = append(out, fr.Moof.Traf.Tfdt.BaseMediaDecodeTime())
		}
	}
	return out
}

// A Matroska original with an open-GOP HEVC (CRA keyframes, their leading
// pictures after them in decode order, B-pyramids), 5.1 E-AC-3 and stereo
// AAC: its copy holds every packet of the original once, in order, and
// decodes to the original's frames; each track's decode times tile its
// timeline across every segment's edge; each video segment starts with a
// keyframe; the presentation is the original's, less the origin.
func TestMatroskaCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		video []string
	}{
		{"hevc-open-gop", []string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params",
			"keyint=48:min-keyint=48:open-gop=1:bframes=4:b-pyramid=1:scenecut=0:log-level=error"}},
		{"h264-bframes", []string{"-c:v", "libx264", "-preset", "veryfast", "-bf", "3", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(lavfi(20), "-map", "0", "-map", "1", "-map", "2")
			args = append(args, tc.video...)
			args = append(args, "-c:a:0", "eac3", "-ac:a:0", "6", "-c:a:1", "aac", "-ac:a:1", "2", "-cluster_time_limit", "1500")
			file := makeFile(t, "film.mkv", args...)
			ix := index(t, file)
			if ix.Source != FromCues || len(ix.Audio) != 2 || !ix.Audio[0].Copyable() || ix.Audio[0].Codec != "ec-3" ||
				!ix.Audio[1].Copyable() || !strings.HasPrefix(ix.Audio[1].Codec, "mp4a.40.") {
				t.Fatalf("index %+v", ix.Audio)
			}
			if n := len(ix.Spans()); n < 3 {
				t.Fatalf("%d segments", n)
			}
			t.Logf("video %s ts %d frame %d shift %d grid %d, %d keyframes, %d segments, audio %s/%d %s/%d",
				ix.Video.Codec, ix.Video.Timescale, ix.Video.FrameTicks, ix.Video.Shift, ix.Video.GridOrigin, len(ix.Keyframes), len(ix.Spans()),
				ix.Audio[0].Codec, ix.Audio[0].FrameSamples, ix.Audio[1].Codec, ix.Audio[1].FrameSamples)
			video, aud, segs := produceAll(t, ix, []int{0, 1})
			vf, ef, af := write(t, "video.mp4", video), write(t, "eac3.mp4", aud[0]), write(t, "aac.mp4", aud[1])

			// Every packet, once, in order.
			for _, c := range []struct{ copy, stream, orig string }{{vf, "v:0", "v:0"}, {ef, "a:0", "a:0"}, {af, "a:0", "a:1"}} {
				want, got := hashes(t, file, c.orig, true), hashes(t, c.copy, c.stream, true)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: %d packets copied, the original has %d (equal: %v)", c.orig, len(got), len(want), reflect.DeepEqual(got, want))
				}
			}
			// The frames, decoded.
			if want, got := hashes(t, file, "v:0", false), hashes(t, vf, "v:0", false); !reflect.DeepEqual(got, want) {
				t.Errorf("decoded: %d frames, the original's %d (equal: %v)", len(got), len(want), reflect.DeepEqual(got, want))
			}
			checkTiling(t, vf, "v:0")
			checkTiling(t, ef, "a:0")
			checkTiling(t, af, "a:0")
			// No decode time below 0 (an unsigned tfdt that wrapped): a track
			// presented from before zero is moved up, its edit list taking it
			// off.
			for name, b := range map[string][]byte{"video": video, "E-AC-3": aud[0], "AAC": aud[1]} {
				for _, s := range fragmentStarts(t, b) {
					if s > 1<<40 {
						t.Errorf("%s: a fragment decodes at %d", name, s)
					}
				}
			}

			// Each video segment starts at a keyframe, its decode time where the
			// one before ended.
			starts := fragmentStarts(t, video)
			if len(starts) != len(segs) {
				t.Fatalf("%d video fragments for %d segments", len(starts), len(segs))
			}
			for n, s := range ix.Spans() {
				kf := ix.Keyframes[s.First].PTS
				if n > 0 && (int64(starts[n]) > kf || kf-int64(starts[n]) > 4*int64(ix.Video.Timescale)/24) {
					t.Errorf("segment %d starts decoding at %d, its keyframe is presented at %d", n, starts[n], kf)
				}
			}

			// Presentation: the original's packets, less the origin, in step
			// across the tracks.
			orig := packets(t, file, "v:0")
			copied := packets(t, vf, "v:0")
			if len(orig) == len(copied) {
				// ffprobe's timebases: the original's milliseconds, the copy's
				// 1/timescale.
				for i := range orig {
					wantNs := orig[i].PTS*1e6 - ix.OriginNs
					gotNs := copied[i].PTS * 1e9 / int64(ix.Video.Timescale)
					if d := gotNs - wantNs; d > 1e6 || d < -1e6 {
						t.Errorf("frame %d presented at %d ns, the original at %d ns", i, gotNs, wantNs)
						break
					}
				}
			}
		})
	}
}

// The index of a file is the same read twice and, persisted as JSON, read
// back: what a cache keeps.
func TestIndexRoundTrip(t *testing.T) {
	file := makeFile(t, "film.mkv", append(lavfi(8), "-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48",
		"-c:a", "ac3")...)
	ix := index(t, file)
	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	var back Index
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Spans(), ix.Spans()) || !bytes.Equal(back.Video.Init, ix.Video.Init) || back.Audio[0].Codec != "ac-3" {
		t.Errorf("round trip: %+v", back.Audio)
	}
}
