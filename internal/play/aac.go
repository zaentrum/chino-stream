package play

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// AAC encoding for the on-the-fly pipelines: the window transcoder's audio
// renditions and the legacy progressive /api/play stream.
//
// Which AAC encoder an ffmpeg build carries is not a given. libfdk_aac only
// exists in --enable-nonfree builds, which may not be redistributed: the
// distro package in the public image (and the common static builds) carry
// ffmpeg's native "aac" encoder and nothing else. Both pipelines used to
// hard-code libfdk_aac, so on the public image every audio window failed with
// "Unknown encoder 'libfdk_aac'" — a fallback session without stream copy had
// no audio and often never started.
//
// DetectAACEncoder asks ffmpeg once, at startup, which encoder it has.
// libfdk_aac stays in use where a build has it; everywhere else the native
// encoder, which every ffmpeg build ships, does the job. (An older note
// blamed the native encoder for AAC frames Chromium rejected with
// PIPELINE_ERROR_DECODE on 5.1→2.0 downmixes; the same notes also blamed
// `-async 1`, fixed since. Should that error show up on native-encoded
// audio, this is the first suspect.)
const (
	aacNative = "aac"
	aacFDK    = "libfdk_aac"
)

// windowAudioBitrate is the bitrate of every on-the-fly audio rendition:
// AAC-LC stereo at 160 kb/s, the stereo rate common HLS authoring guidance
// recommends. One rate for all rungs, since the audio renditions are shared
// by every video rung; it also leaves the native encoder (a little less
// efficient than libfdk_aac) enough room.
const windowAudioBitrate = "160k"

// windowAudioBitrateBps is windowAudioBitrate in bit/s, for the BANDWIDTH a
// master playlist advertises.
const windowAudioBitrateBps = 160_000

// DetectAACEncoder lists ffmpeg's encoders and returns the AAC encoder the
// pipelines use: libfdk_aac when the build has it, else the native aac
// encoder. It fails when ffmpeg cannot be run or lists neither; then no
// on-the-fly audio can be produced at all, and the caller reports not ready.
func DetectAACEncoder(ctx context.Context, ffmpegBin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", fmt.Errorf("list the encoders of %s: %w", ffmpegBin, err)
	}
	return pickAACEncoder(audioEncoders(string(out)))
}

// audioEncoders parses `ffmpeg -encoders` into the set of audio encoder
// names. An encoder line is a six-letter capability column whose first letter
// is the media type, then the name: " A....D aac   AAC (Advanced Audio
// Coding)". The legend above the list (" A..... = Audio") has "=" where a
// name would be.
func audioEncoders(listing string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 6 || f[0][0] != 'A' || f[1] == "=" {
			continue
		}
		out[f[1]] = true
	}
	return out
}

func pickAACEncoder(have map[string]bool) (string, error) {
	for _, name := range []string{aacFDK, aacNative} {
		if have[name] {
			return name, nil
		}
	}
	return "", errors.New("ffmpeg lists no AAC encoder (neither libfdk_aac nor aac)")
}

// aacArgs is the audio encode of one output: AAC-LC, 48 kHz, stereo, at
// bitrate, with encoder ("" = the native one). Stereo is unconditional —
// 5.1 / 7.1 sources are downmixed to 2.0 — so every output matches the
// mp4a.40.2 the master playlists advertise, whatever the source layout.
func aacArgs(encoder, bitrate string) []string {
	if encoder == "" {
		encoder = aacNative
	}
	return []string{
		"-c:a", encoder,
		"-profile:a", "aac_low",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", bitrate,
	}
}
