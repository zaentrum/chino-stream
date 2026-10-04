package play

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

// With the ffmpeg and ffprobe on $PATH (the image's, in a container): the
// stream-copy plan's ffprobe fallback — what a source mp4ff can't read
// (MKV, WebM) goes through — finds the keyframes, and the plan groups them
// into segments. Ubuntu 22.04's ffprobe 4.4 answers `frame=pts_time` with
// empty fields, so on that build the plan had no keyframes at all.
func TestCopyPlanKeyframesFromTheFFprobeOnPath(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	if !allEncoders(t, ffmpeg)["libx264"] {
		t.Skip("this ffmpeg has no libx264")
	}
	src := filepath.Join(t.TempDir(), "gop2s.mkv")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc2=size=320x180:rate=24:duration=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", src)

	if _, err := keyframesViaMp4ff(src); err == nil {
		t.Fatal("mp4ff read an MKV: the test would not reach the ffprobe fallback")
	}
	kf, err := probeKeyframes(context.Background(), ffprobe, src)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 2, 4, 6, 8}
	if len(kf) != len(want) {
		t.Fatalf("keyframes %v, want %v", kf, want)
	}
	for i := range want {
		if math.Abs(kf[i]-want[i]) > 0.002 {
			t.Fatalf("keyframes %v, want %v", kf, want)
		}
	}

	plan, err := buildPassPlan(context.Background(), ffprobe, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Segments) != 2 || math.Abs(plan.Segments[0].DurSec-6) > 0.002 ||
		math.Abs(plan.Segments[1].StartSec-6) > 0.002 || math.Abs(plan.Segments[1].DurSec-4) > 0.05 {
		t.Errorf("plan %+v, want [0,6) and [6,10)", plan.Segments)
	}
}
