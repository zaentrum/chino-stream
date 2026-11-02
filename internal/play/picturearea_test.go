package play

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// letterbox is where the picture of a 2.39:1 film letterboxed in a 1920x1080
// frame is, as katalog-api answers it: 1920x804 at y=138.
var letterbox = &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, Y: 138, Width: 1920, Height: 804}

// letterboxInfo is /play/info's pictureArea of letterbox, as it is on the
// wire.
const letterboxInfo = `{"left":0,"top":0.1278,"width":1,"height":0.7444}`

// Each edge of the area is a fraction of the frame measured, rounded to four
// decimals, and its width and height the distances between the edges,
// rounded again: a letterbox, a pillarbox, a picture a pixel in from every
// edge (its width 0.999 - 0.0005, not 1917/1920 rounded, 0.9984). A fraction
// holds for every size of the frame measured - a 4K measurement on a 1080p
// rung, a 1080p one on a 4K frame, on 720p and 480p rungs - and for a frame
// up to 1% wider or narrower than it (854x480 is 0.08% wider than 16:9).
func TestPictureAreaFractions(t *testing.T) {
	letterboxed := pictureArea{Top: 0.1278, Width: 1, Height: 0.7444}
	for _, tc := range []struct {
		name          string
		area          catalog.PictureArea
		width, height int
		want          pictureArea
	}{
		{"a letterbox", *letterbox, 1920, 1080, letterboxed},
		{"a pillarbox", catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, X: 240, Width: 1440, Height: 1080}, 1920, 1080,
			pictureArea{Left: 0.125, Width: 0.75, Height: 1}},
		{"a pixel in from every edge", catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, X: 1, Y: 1, Width: 1917, Height: 1077}, 1920, 1080,
			pictureArea{Left: 0.0005, Top: 0.0009, Width: 0.9985, Height: 0.9972}},
		{"a 4K measurement on a 1080p rung", catalog.PictureArea{FrameWidth: 3840, FrameHeight: 2160, Y: 276, Width: 3840, Height: 1608}, 1920, 1080,
			letterboxed},
		{"on a 4K frame", *letterbox, 3840, 2160, letterboxed},
		{"on a 720p rung", *letterbox, 1280, 720, letterboxed},
		{"on an 854x480 rung", *letterbox, 854, 480, letterboxed},
		{"on a frame 0.9% narrower", *letterbox, 1920, 1090, letterboxed},
	} {
		if got := infoPictureArea(&tc.area, tc.width, tc.height); got == nil || *got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	b, err := json.Marshal(infoPictureArea(letterbox, 1920, 1080))
	if err != nil || string(b) != letterboxInfo {
		t.Errorf("on the wire: %s %v, want %s", b, err, letterboxInfo)
	}
}

// No pictureArea where it cannot be said: no area, the whole frame, no
// rectangle inside its frame, a frame served of no size, or of another shape
// than the frame measured - cropped to the picture, 4:3, more than 1%
// narrower (1920x1092 is 1.1% narrower than 16:9). A crop that cuts the
// picture is far worse than a frame shown whole.
func TestNoPictureAreaWhereItCannotBeSaid(t *testing.T) {
	for _, tc := range []struct {
		name          string
		area          *catalog.PictureArea
		width, height int
	}{
		{"no area", nil, 1920, 1080},
		{"the whole frame", &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, Width: 1920, Height: 1080}, 1920, 1080},
		{"the whole frame measured in 4K", &catalog.PictureArea{FrameWidth: 3840, FrameHeight: 2160, Width: 3840, Height: 2160}, 1920, 1080},
		{"a frame cropped to the picture", letterbox, 1920, 800},
		{"a 4:3 frame", letterbox, 1440, 1080},
		{"a frame 1.1% narrower", letterbox, 1920, 1092},
		{"a frame of no size", letterbox, 0, 0},
		{"past the frame's foot", &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, Y: 138, Width: 1920, Height: 1000}, 1920, 1080},
		{"left of the frame", &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, X: -1, Width: 1440, Height: 1080}, 1920, 1080},
		{"no width", &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, Y: 138, Height: 804}, 1920, 1080},
		{"no frame", &catalog.PictureArea{Y: 138, Width: 1920, Height: 804}, 1920, 1080},
	} {
		if got := infoPictureArea(tc.area, tc.width, tc.height); got != nil {
			t.Errorf("%s: %+v, want none", tc.name, got)
		}
	}
}

// infoField is the field name of GET /api/play/{id}/info{query} on h as it is
// on the wire ("" where the answer has none), and the answer's mode.
func infoField(t *testing.T, h *Handler, id, query, name string) (field, mode string) {
	t.Helper()
	w := getPlay(h, "/api/play/"+id+"/info"+query)
	var info map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil {
		t.Fatalf("info: %d %q", w.Code, w.Body)
	}
	_ = json.Unmarshal(info["mode"], &mode)
	return string(info[name]), mode
}

// A packaged title's /play/info says where its picture is in the frame of the
// rung the client starts on, the same fractions on every rung of the frame:
// an HEVC client's 1080p, an H.264 client's 720p. Nothing where katalog-api
// answers no area, where the area is the whole frame, or where it was
// measured in a frame of another shape than the rung's.
func TestPlayInfoOfAPackageSaysWhereThePictureIs(t *testing.T) {
	l := stageLibrary(t)
	root, src := mediaFile(t, "film.mkv")
	for _, tc := range []struct {
		name, version, query string
		area                 *catalog.PictureArea
		want                 string
	}{
		{"an HEVC client's 1080p", libNew, "?caps=avc,hvc,aac", letterbox, letterboxInfo},
		{"an H.264 client's 720p", libOld, "?caps=avc,aac", letterbox, letterboxInfo},
		{"no area", libNew, "?caps=avc,hvc,aac", nil, ""},
		{"the whole frame", libNew, "?caps=avc,hvc,aac", &catalog.PictureArea{FrameWidth: 1920, FrameHeight: 1080, Width: 1920, Height: 1080}, ""},
		{"measured in a 4:3 frame", libNew, "?caps=avc,hvc,aac", &catalog.PictureArea{FrameWidth: 1440, FrameHeight: 1080, Y: 60, Width: 1440, Height: 960}, ""},
	} {
		l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(tc.version)), PictureArea: tc.area})
		h := &Handler{Catalog: fakeKatalog(t, src), Packages: l.katalog.resolver(), MediaRoot: root}
		if got, mode := infoField(t, h, libItem, tc.query, "pictureArea"); got != tc.want || mode != "packaged" {
			t.Errorf("%s: pictureArea %s in mode %s, want %q, packaged", tc.name, got, mode, tc.want)
		}
	}
}

// On the fly from the original the area is of the original's frame, from
// katalog-api's answer for the item as the packaged check had it: said for an
// original of the frame measured, not for one cropped to the picture; and
// while katalog-api cannot say, /play/info is answered as before, without it.
func TestPlayInfoOfAnOriginalSaysWhereThePictureIs(t *testing.T) {
	captureLog(t)
	root, src := mediaFile(t, "film.mkv")
	f := newFakeLibrary(t)
	f.setItem(catalog.Playback{ItemID: "i1", Original: &catalog.Original{Path: src}, PictureArea: letterbox})
	probe := func(width, height int) string {
		return `{"streams":[{"codec_type":"video","codec_name":"hevc","width":` + itoa(width) + `,"height":` + itoa(height) + `},` +
			`{"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"eng"}}],` +
			`"format":{"format_name":"matroska,webm","duration":"600.0","bit_rate":"4000000"}}`
	}
	for _, tc := range []struct {
		name          string
		width, height int
		down          bool
		want          string
	}{
		{"of the frame measured", 1920, 1080, false, letterboxInfo},
		{"cropped to the picture", 1920, 800, false, ""},
		{"katalog-api cannot say", 1920, 1080, true, ""},
	} {
		f.setDown(tc.down)
		h := &Handler{Catalog: fakeKatalog(t, src), Packages: f.resolver(), MediaRoot: root, FFprobeBin: probeFFprobe(t, probe(tc.width, tc.height))}
		if got, mode := infoField(t, h, "i1", "?caps=avc,aac", "pictureArea"); got != tc.want || mode != "transcode" {
			t.Errorf("%s: pictureArea %s in mode %s, want %q, transcode", tc.name, got, mode, tc.want)
		}
	}
}

// On the fly from a package - a title whose original is retired, for a
// client that decodes none of its rungs - the area is of the package's top
// rendition, which the transcode reads.
func TestPlayInfoOfAPackageOnTheFlySaysWhereThePictureIs(t *testing.T) {
	l, _, ph := retiredLibrary(t, windowFFmpeg(t, t.TempDir(), 0, 4, ""))
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew)), PictureArea: letterbox})
	if got, mode := infoField(t, ph, libItem, "?caps=avc,aac", "pictureArea"); got != letterboxInfo || mode != "transcode" {
		t.Errorf("pictureArea %s in mode %s, want %s, transcode", got, mode, letterboxInfo)
	}
}

// The area is that of the answer whose package plays. When the version a
// cached answer names is gone (removed after its grace) and katalog-api now
// answers a version of another file, measured or not, /play/info says the
// new answer's area, never the one of the file that is gone.
func TestPlayInfoSaysTheAreaOfTheAnswerThatPlays(t *testing.T) {
	l := stageLibrary(t)
	now := time.Now()
	l.packages.now = func() time.Time { return now }
	root, src := mediaFile(t, "film.mkv")
	h := &Handler{Catalog: fakeKatalog(t, src), Packages: l.packages, MediaRoot: root}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libOld)), PictureArea: letterbox})
	if got, _ := infoField(t, h, libItem, "?caps=avc,hvc,aac", "pictureArea"); got != letterboxInfo {
		t.Fatalf("pictureArea %s, want %s", got, letterboxInfo)
	}
	if err := os.RemoveAll(l.version(libOld)); err != nil {
		t.Fatal(err)
	}
	l.katalog.setItem(catalog.Playback{ItemID: libItem, Package: ptr(l.ref(libNew))})
	now = now.Add(resolveRefetchMin) // asked again once the answer is a second old
	if got, mode := infoField(t, h, libItem, "?caps=avc,hvc,aac", "pictureArea"); got != "" || mode != "packaged" {
		t.Errorf("the gone version's area: pictureArea %s in mode %s, want none, packaged", got, mode)
	}
}
