package streamcopy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// FuzzMatroskaCopy: whatever the bytes of a Matroska file, its index and
// every segment of its copy are made or end in an error — never a panic.
// The seeds are small files ffmpeg makes (none without ffmpeg).
func FuzzMatroskaCopy(f *testing.F) {
	if ffmpeg, err := exec.LookPath("ffmpeg"); err == nil {
		dir := f.TempDir()
		for i, args := range [][]string{
			{"-c:v", "libx264", "-preset", "ultrafast", "-g", "12", "-bf", "2", "-c:a", "ac3"},
			{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=12:bframes=2:log-level=error", "-c:a", "aac"},
			{"-c:v", "libx264", "-preset", "ultrafast", "-g", "12", "-c:a", "eac3", "-cluster_time_limit", "200"},
		} {
			out := filepath.Join(dir, "seed"+string(rune('a'+i))+".mkv")
			cmd := exec.Command(ffmpeg, append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=24:duration=2", "-f", "lavfi", "-i", "sine=sample_rate=48000:duration=2"},
				args...), "-y", out)...)
			if cmd.Run() != nil {
				continue
			}
			if b, err := os.ReadFile(out); err == nil {
				f.Add(b)
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		ix, err := IndexMatroska(r, int64(len(data)), Options{DurationNs: 2e9})
		if err != nil {
			return
		}
		if ix.Check() != nil {
			if ix.ScanMatroska(r) != nil || ix.Check() != nil {
				return
			}
		}
		var audio []int
		for i, a := range ix.Audio {
			if a.Copyable() {
				audio = append(audio, i)
			}
		}
		for n := range ix.Spans() {
			if n >= 32 {
				break
			}
			_, _ = ix.Produce(r, n, audio)
		}
		_, _ = ix.Bitrates()
	})
}
