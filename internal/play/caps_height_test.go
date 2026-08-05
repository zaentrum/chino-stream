package play

import "testing"

// TestParseCapsHeight locks the resolution-aware caps wire contract shared
// with chino-mobile + chino-androidtv: each video token may carry an optional
// ":<maxHeight>" suffix recorded under the codecFamily key. Legacy suffix-less
// caps must parse identically (no height = no cap).
func TestParseCapsHeight(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantMax   int            // Caps.MaxVideoHeight()
		wantFam   map[string]int // expected VideoMaxHeight entries (subset)
		wantVideo []string       // codec keys that must be present in Caps.Video
		wantAudio []string
	}{
		{
			name:      "legacy no suffix is uncapped",
			in:        "avc,hvc,av1",
			wantMax:   0,
			wantFam:   map[string]int{},
			wantVideo: []string{"h264", "hevc", "av1"},
		},
		{
			name:      "SM-T500: HEVC HW capped at 1080",
			in:        "avc:1080,hvc:1080,aac",
			wantMax:   1080,
			wantFam:   map[string]int{"h264": 1080, "hevc": 1080},
			wantVideo: []string{"h264", "hevc"},
			wantAudio: []string{"aac"},
		},
		{
			name:    "4K device",
			in:      "avc:2160,hvc:2160,av1:2160",
			wantMax: 2160,
			wantFam: map[string]int{"hevc": 2160, "av1": 2160},
		},
		{
			name:    "duplicate family keeps the larger height",
			in:      "hvc:1080,hevc:2160",
			wantMax: 2160,
			wantFam: map[string]int{"hevc": 2160},
		},
		{
			name:      "colon on an audio/non-codec token is ignored, not panicked",
			in:        "hvc:1080,aac:6,bogus:99",
			wantMax:   1080,
			wantFam:   map[string]int{"hevc": 1080},
			wantAudio: []string{"aac"},
		},
		{
			name:    "empty caps falls back to DefaultCaps (no cap)",
			in:      "",
			wantMax: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseCaps(tc.in)
			if got := c.MaxVideoHeight(); got != tc.wantMax {
				t.Errorf("MaxVideoHeight()=%d, want %d", got, tc.wantMax)
			}
			for fam, h := range tc.wantFam {
				if c.VideoMaxHeight[fam] != h {
					t.Errorf("VideoMaxHeight[%q]=%d, want %d", fam, c.VideoMaxHeight[fam], h)
				}
			}
			for _, v := range tc.wantVideo {
				if !c.Video[v] {
					t.Errorf("Video[%q] missing", v)
				}
			}
			for _, a := range tc.wantAudio {
				if !c.Audio[a] {
					t.Errorf("Audio[%q] missing", a)
				}
			}
		})
	}
}

// TestVideoCacheQuality guards the on-disk transcode cache namespace: an
// uncapped request MUST keep the bare rung name so existing caches stay valid;
// a capped request gets a distinct "{rung}-h{cap}" key.
func TestVideoCacheQuality(t *testing.T) {
	cases := []struct {
		quality   string
		maxHeight int
		want      string
	}{
		{"high", 0, "high"}, // uncapped: unchanged namespace (backward compat)
		{"high", 1080, "high-h1080"},
		{"medium", 480, "medium-h480"},
		{"low", 0, "low"},
	}
	for _, tc := range cases {
		if got := videoCacheQuality(tc.quality, tc.maxHeight); got != tc.want {
			t.Errorf("videoCacheQuality(%q,%d)=%q, want %q", tc.quality, tc.maxHeight, got, tc.want)
		}
	}
}
