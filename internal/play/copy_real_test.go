package play

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The stream copy of real originals, through the HTTP routes: each file a
// sparse copy of what MKV_SAMPLES/<name>/ holds of it — its head and its
// Cues at their places (<offset>.part), holes elsewhere, its size (size).
// Library media is never committed: without MKV_SAMPLES this is skipped.
func TestStreamCopyOfRealFiles(t *testing.T) {
	samples := os.Getenv("MKV_SAMPLES")
	if samples == "" {
		t.Skip("MKV_SAMPLES is not set: no byte ranges of real files at hand")
	}
	ffmpeg, ffprobe := realFFmpeg(t)
	captureLog(t)
	inPlaceWarms(t)
	dirs, _ := os.ReadDir(samples)
	for _, d := range dirs {
		dir := filepath.Join(samples, d.Name())
		sizeB, err := os.ReadFile(filepath.Join(dir, "size"))
		if !d.IsDir() || err != nil {
			continue
		}
		t.Run(d.Name(), func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "original.mkv")
			size, _ := strconv.ParseInt(strings.TrimSpace(string(sizeB)), 10, 64)
			sparse(t, file, dir, size)
			h := copyHandler(t, root, file, windowFFmpeg(t, t.TempDir(), 99, 0, "no ffmpeg here"), ffprobe)
			const caps = "?caps=avc:4096,hvc:4096,av1:3840,aac,mp3,opus,ac3,eac3"
			t0 := time.Now()
			m := get(h, "/api/play/i1/master.m3u8"+caps)
			took := time.Since(t0)
			media, variants := copyMaster(m.Body.String())
			if len(variants) != 1 || !strings.HasPrefix(variants[0]["uri"], "video/") {
				t.Logf("not copied for a TV (%v):\n%s", took, m.Body)
				return
			}
			t.Logf("master in %v: %s %s %s, audio %d", took.Round(time.Millisecond), variants[0]["CODECS"], variants[0]["RESOLUTION"],
				variants[0]["VIDEO-RANGE"], len(media))
			// The init and segment 0, as a player asks for them first.
			base := "/api/play/i1/"
			pl := get(h, base+variants[0]["uri"])
			if pl.Code != 200 {
				t.Fatalf("playlist: %d", pl.Code)
			}
			v := strings.SplitN(variants[0]["uri"], "?", 2)
			init := get(h, base+"video/init.mp4?"+v[1])
			t1 := time.Now()
			seg := get(h, base+"video/0.m4s?"+v[1])
			if init.Code != 200 || seg.Code != 200 {
				t.Logf("segment 0: init %d, segment %d %.80q (the head holds less than it)", init.Code, seg.Code, seg.Body)
				return
			}
			t.Logf("segment 0: %d bytes in %v", seg.Body.Len(), time.Since(t1).Round(time.Millisecond))
			vf := writeTemp(t, "v.mp4", append(init.Body.Bytes(), seg.Body.Bytes()...))
			head := filepath.Join(t.TempDir(), "head.mkv")
			if b, err := os.ReadFile(filepath.Join(dir, "0.part")); err == nil {
				_ = os.WriteFile(head, b, 0o644)
			}
			got, want := packetMD5s(t, ffmpeg, vf, "v:0"), packetMD5s(t, ffmpeg, head, "v:0")
			if len(got) == 0 || len(want) < len(got) || !reflect.DeepEqual(got, want[:len(got)]) {
				t.Errorf("video segment 0: %d packets, not the head's first", len(got))
			}
			tiles(t, ffprobe, vf, "v:0")
			for i, md := range media {
				if !strings.HasPrefix(md["URI"], copiedPrefix) {
					continue
				}
				a := strings.SplitN(md["URI"], "?", 2)
				folder := strings.TrimSuffix(a[0], "/index.m3u8")
				ai := get(h, base+folder+"/init.mp4?"+a[1])
				as := get(h, base+folder+"/0.m4s?"+a[1])
				if ai.Code != 200 || as.Code != 200 {
					t.Errorf("%s: init %d segment %d", folder, ai.Code, as.Code)
					continue
				}
				af := writeTemp(t, folder+".mp4", append(ai.Body.Bytes(), as.Body.Bytes()...))
				got, want := packetMD5s(t, ffmpeg, af, "a:0"), packetMD5s(t, ffmpeg, head, "a:"+strings.TrimPrefix(folder, copiedPrefix))
				if len(got) == 0 || len(want) < len(got) || !reflect.DeepEqual(got, want[:len(got)]) {
					t.Errorf("%s (rendition %d) segment 0: %d packets, not the head's first", folder, i, len(got))
				}
			}
		})
	}
}

// sparse writes the parts of a sample (dir/<offset>.part) into file at their
// offsets, the file size bytes long, the rest holes.
func sparse(t *testing.T, file, dir string, size int64) {
	t.Helper()
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	sort.Strings(parts)
	for _, p := range parts {
		at, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(p), ".part"), 10, 64)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(b, at); err != nil {
			t.Fatal(err)
		}
	}
}
