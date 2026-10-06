package play

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// A title's extras: its trailers, teasers, featurettes and other bonus
// material, each a file of its own that katalog-manager has packaged apart
// from the title. An extra's package is in the package store at
//
//	extras/{shard2}/{extraId}/{manifest.json, .complete, hls/…}
//
// laid out as an item's package, without trickplay; its manifest names the
// title it belongs to (parentId) and its kind (extraKind). It is served
// under that title's id, with the routes of an item's packaged renditions:
//
//	/api/play/{itemId}/extras/{extraId}/master.m3u8
//	/api/play/{itemId}/extras/{extraId}/{vN|aN|sN}/playlist.m3u8
//	/api/play/{itemId}/extras/{extraId}/{vN|aN}/iframes.m3u8, init.mp4, seg-{n}.m4s
//	/api/play/{itemId}/extras/{extraId}/{sN}/seg-{n}.vtt
//
// Every one of them answers 404 unless extraId is a UUID, the package is
// complete (.complete) and its manifest's parentId is itemId. chino-api holds
// a viewer to the rating of the title in the URL; the parentId check makes
// that the extra's own title, so an extra never plays under another one.
//
// The master is the client's share of the extra's ladder (serveLadder: the
// rungs it decodes at its heights, or the one ?q= names), every URI in it
// carrying the request's query (the stream token, caps, q). There is no
// on-the-fly fallback as for an item: the extras ladder is H.264 and stereo
// AAC, which every client decodes, and a client that claims to decode none
// of it is served the master as packaged.
//
// An extra is no item: katalog-api lists it in neither the packaged ids nor
// answers it as an item, so it is not in the Zap pool, and
// /api/play/{extraId}/… finds no package.

// extrasCategory is the package store folder of the extras' packages.
const extrasCategory = "extras"

// uuidPattern is the text form of a UUID, the only form an extra's id takes.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// extraRoutes are the routes of one extra, mounted under
// /api/play/{itemId}/extras/{extraId}.
func (h *HLSHandler) extraRoutes(r chi.Router) {
	r.Get("/master.m3u8", h.ExtraMaster)
	r.Get("/{rendId:[vas][0-9]+}/playlist.m3u8", h.ExtraRenditionPlaylist)
	r.Get("/{rendId:[va][0-9]+}/iframes.m3u8", h.ExtraIframesPlaylist)
	r.Get("/{rendId:[va][0-9]+}/init.mp4", h.ExtraInitSegment)
	r.Get("/{rendId:[va][0-9]+}/seg-{seg:[0-9]+}.m4s", h.ExtraSegment)
	r.Get("/{rendId:s[0-9]+}/seg-{seg:[0-9]+}.vtt", h.ExtraSubtitleSegment)
}

// extraRoot is the package folder of the extra extraID of the title itemID,
// or "" when there is none to serve: extraID is no UUID, the package is not
// complete, or its manifest cannot be read or names another title. The
// manifest is read through manifestCache, as an item's is.
func extraRoot(itemID, extraID string) string {
	if !uuidPattern.MatchString(extraID) {
		return ""
	}
	root := filepath.Join(PackagesRoot, extrasCategory, strings.ToLower(extraID[:2]), extraID)
	if st, err := os.Stat(filepath.Join(root, ".complete")); err != nil || st.IsDir() {
		return ""
	}
	mf, err := readManifest(filepath.Join(root, "manifest.json"), decodeManifest)
	if err != nil || mf.ParentID == "" || mf.ParentID != itemID {
		return ""
	}
	return root
}

// extraFile is the file rel of the package of the extra r names, or "" when
// that extra is not one to serve (extraRoot), which it has answered 404.
func extraFile(w http.ResponseWriter, r *http.Request, rel ...string) string {
	root := extraRoot(chi.URLParam(r, "itemId"), chi.URLParam(r, "extraId"))
	if root == "" {
		http.Error(w, "extra not found", http.StatusNotFound)
		return ""
	}
	return filepath.Join(append([]string{root}, rel...)...)
}

// ExtraMaster serves an extra's master as the client is served it: its
// share of the ladder for ?caps= and ?q=, every URI carrying the query.
func (h *HLSHandler) ExtraMaster(w http.ResponseWriter, r *http.Request) {
	path := extraFile(w, r, "hls", "master.m3u8")
	if path == "" {
		return
	}
	caps := ParseCaps(r.URL.Query().Get("caps"))
	q := r.URL.Query().Get("q")
	servePlaylistCachedTransform(w, r, path, r.URL.RawQuery, nil, func(body string) string {
		return serveLadder(body, caps, q).body
	})
}

// ExtraRenditionPlaylist serves the media playlist of an extra's video,
// audio or WebVTT rendition, its URIs carrying the query.
func (h *HLSHandler) ExtraRenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	if path := extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "playlist.m3u8"); path != "" {
		servePlaylistCached(w, r, path, nil)
	}
}

// ExtraIframesPlaylist serves the I-frame playlist of an extra's video rung.
func (h *HLSHandler) ExtraIframesPlaylist(w http.ResponseWriter, r *http.Request) {
	if path := extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "iframes.m3u8"); path != "" {
		serveIframesPlaylist(w, r, path, nil)
	}
}

// ExtraInitSegment serves the init.mp4 of an extra's video or audio
// rendition.
func (h *HLSHandler) ExtraInitSegment(w http.ResponseWriter, r *http.Request) {
	if path := extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "init.mp4"); path != "" {
		servePackagedStatic(w, r, path, "video/mp4", "init not found")
	}
}

// ExtraSegment serves one media segment of an extra's video or audio
// rendition (seg-NNNNN.m4s, as shaka names them), ranges and all.
func (h *HLSHandler) ExtraSegment(w http.ResponseWriter, r *http.Request) {
	if path := extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".m4s"); path != "" {
		servePackagedStatic(w, r, path, "video/iso.segment", "segment not found")
	}
}

// ExtraSubtitleSegment serves one WebVTT segment of an extra's subtitle
// rendition (sN).
func (h *HLSHandler) ExtraSubtitleSegment(w http.ResponseWriter, r *http.Request) {
	if path := extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".vtt"); path != "" {
		servePackagedStatic(w, r, path, "text/vtt; charset=utf-8", "subtitle segment not found")
	}
}
