package play

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
)

// Quality describes one rung of the adaptive ladder. Used only in transcode
// mode (remux is always stream-copy at source bitrate).
type Quality struct {
	Name    string // "high" | "medium" | "low"
	CRF     string // x264 quality (lower = better)
	Audio   string // AAC bitrate
	Scale   string // -vf scale expression, empty = no resize
	Label   string // human label for UI
}

var QualityLadder = map[string]Quality{
	"high":   {Name: "high",   CRF: "23", Audio: "192k", Scale: "",                 Label: "High (source resolution)"},
	"medium": {Name: "medium", CRF: "26", Audio: "128k", Scale: "scale=-2:720",     Label: "Medium (720p)"},
	"low":    {Name: "low",    CRF: "28", Audio: "96k",  Scale: "scale=-2:480",     Label: "Low (480p)"},
}

func ResolveQuality(q string) Quality {
	if v, ok := QualityLadder[q]; ok {
		return v
	}
	return QualityLadder["high"]
}

// BuildFFmpeg constructs the ffmpeg invocation. Three modes (transcode /
// remux / passthrough — the last is handled by the caller, not here).
//
// startSec lets the player resume mid-movie after a quality switch:
//   - passed as `-ss <sec>` BEFORE `-i` (input seek, fast).
//   - the resulting fragmented MP4 starts at t=0 from the client's
//     perspective; the player tracks a streamOffset in JS to keep the
//     displayed clock honest.
//
// audioIdx selects which audio stream to map (`0:a:N`). The default
// audio track on the file is index 0; the player overrides this when
// the user picks a different language in the audio switcher.
//
// Always downmix audio to stereo and emit fragmented MP4 (frag_keyframe +
// empty_moov + default_base_moof) so the browser can start decoding
// before transcoding completes.
func BuildFFmpeg(ctx context.Context, ffmpegBin, preset, path, mode string, q Quality, startSec int, audioIdx int) *exec.Cmd {
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "warning",
		// -re reads the input at its native frame rate so output is
		// produced at ~1× realtime. Without this, ffmpeg burns CPU
		// pushing 3-5× realtime to the client; Chromium then decides
		// it has "enough" buffered, closes the HTTP connection, our
		// io.Copy sees broken pipe, ffmpeg gets SIGKILLed, and the
		// player fires premature_eof recoverFault. With -re the
		// browser buffer fills only as fast as it drains, so the
		// connection stays alive for the whole movie.
		"-re",
		// +genpts regenerates PTS from frame durations; +igndts drops
		// broken container DTS; +discardcorrupt makes the demuxer drop
		// packets it identifies as broken (CRC errors, sync issues)
		// before they reach the encoder. Without discardcorrupt, a
		// single broken source frame survives the re-encode and
		// Chromium's AAC decoder rejects it with PIPELINE_ERROR_DECODE,
		// which the player treats as fatal and reloads from the start.
		"-fflags", "+genpts+igndts+discardcorrupt",
		// Forgive minor decode errors at the input side rather than
		// emitting a malformed downstream packet for them.
		"-err_detect", "ignore_err",
	}
	if startSec > 0 {
		// Input seek — fast but rounds to nearest keyframe. The drift
		// (≤ 2-4s) is well below what the user notices on a quality
		// switch.
		args = append(args, "-ss", strconv.Itoa(startSec))
	}
	if audioIdx < 0 {
		audioIdx = 0
	}
	args = append(args, "-i", path,
		"-map", "0:v:0",
		"-map", "0:a:"+strconv.Itoa(audioIdx)+"?",
	)
	switch mode {
	case "remux":
		// Always re-encode audio: source tracks are commonly AC-3 /
		// E-AC-3 / DTS, which Chromium can't decode in fragmented MP4.
		// Video stream-copy keeps remux cheap.
		args = append(args,
			"-c:v", "copy",
			// libfdk_aac is Fraunhofer's reference AAC encoder
			// (--enable-libfdk_aac --enable-nonfree in our ffmpeg).
			// Built-in `aac` occasionally produced AAC LC frames that
			// Chromium rejected with PIPELINE_ERROR_DECODE even though
			// the frame structure looked valid (observed on 5.1→2.0
			// downmix outputs from this file at source-times 52.9 s,
			// 96.5 s, 99.75 s). fdk_aac is stricter about the bitstream
			// shape and avoids those edge cases.
			"-c:a", "libfdk_aac",
			"-profile:a", "aac_low",
			"-ar", "48000",
			"-ac", "2",
			"-b:a", "192k",
			// Soft async: stretch/compress audio to track wall time;
			// only fall back to silence padding when drift exceeds
			// 100 ms (min_hard_comp). Avoids the duplicate-frame /
			// non-monotonic PTS that `-async 1` produced, which
			// Chromium decoded as PIPELINE_ERROR_DECODE on otherwise-
			// valid 266-byte AAC packets.
			"-af", "aresample=async=1000:first_pts=0:min_hard_comp=0.100",
		)
	default:
		vfFlag := []string{}
		if q.Scale != "" {
			vfFlag = []string{"-vf", q.Scale}
		}
		args = append(args, vfFlag...)
		args = append(args,
			"-c:v", "libx264",
			"-preset", preset,
			"-tune", "zerolatency",
			"-crf", q.CRF,
			"-pix_fmt", "yuv420p",
			// libfdk_aac is Fraunhofer's reference AAC encoder
			// (--enable-libfdk_aac --enable-nonfree in our ffmpeg).
			// Built-in `aac` occasionally produced AAC LC frames that
			// Chromium rejected with PIPELINE_ERROR_DECODE even though
			// the frame structure looked valid (observed on 5.1→2.0
			// downmix outputs from this file at source-times 52.9 s,
			// 96.5 s, 99.75 s). fdk_aac is stricter about the bitstream
			// shape and avoids those edge cases.
			"-c:a", "libfdk_aac",
			"-profile:a", "aac_low",
			"-ar", "48000",
			"-ac", "2",
			"-b:a", q.Audio,
			// Soft async: stretch audio for drifts < 100 ms; only
			// silence-pad past that threshold. `-async 1` was
			// emitting duplicate/non-monotonic AAC frames at drift
			// boundaries — Chromium rejected those as
			// PIPELINE_ERROR_DECODE on otherwise-normal packets.
			"-af", "aresample=async=1000:first_pts=0:min_hard_comp=0.100",
		)
	}
	args = append(args,
		// After -ss the first packet can carry a tiny negative TS;
		// make_zero shifts everything so the output starts at 0.
		"-avoid_negative_ts", "make_zero",
		// Long files with sparse audio frames can otherwise blow the
		// default 1024-packet muxer queue.
		"-max_muxing_queue_size", "4096",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return exec.CommandContext(ctx, ffmpegBin, args...)
}

// BuildEmbeddedSubtitleFFmpeg extracts the Nth subtitle stream (per-kind
// ordinal — `0:s:N`) into WebVTT and writes to stdout. WebVTT is the
// HTML5 <track> native format so the browser can mount the result as a
// caption track with no further conversion.
//
// startSec mirrors the video's `-ss` input seek: when the player
// requests `?t=<sec>` (after a quality switch or resume), ffmpeg shifts
// cue timestamps so the first cue emitted is relative to t=0 of the
// trimmed output. Without this, cues for source-time T appear at
// player-time T even though the video is at player-time T-startSec, and
// subtitles run ahead by `startSec` seconds.
func BuildEmbeddedSubtitleFFmpeg(ctx context.Context, ffmpegBin, path string, subIdx, startSec int) *exec.Cmd {
	if subIdx < 0 {
		subIdx = 0
	}
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "warning",
	}
	if startSec > 0 {
		args = append(args, "-ss", strconv.Itoa(startSec))
	}
	args = append(args,
		"-i", path,
		"-map", "0:s:"+strconv.Itoa(subIdx),
		"-c:s", "webvtt",
		"-f", "webvtt",
		"pipe:1",
	)
	return exec.CommandContext(ctx, ffmpegBin, args...)
}

// PipeFFmpegTo wires the ffmpeg command's stdout into w and forwards
// stderr to the server log so failures are visible. Returns the wait
// error (which doubles as the "transcode complete" signal).
func PipeFFmpegTo(cmd *exec.Cmd, w io.Writer, label string) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				log.Printf("ffmpeg %s: %s", label, string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()
	_, copyErr := io.Copy(w, stdout)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("ffmpeg wait: %w", waitErr)
	}
	return copyErr
}
