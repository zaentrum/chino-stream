package play

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// A title's extras: its trailers, teasers, featurettes and other bonus
// material, each a file of its own that katalog-manager has packaged apart
// from the title. katalog-api says where an extra's package is
// (/api/v1/extras/{extraId}/playback, resolve.go) and whose extra it is:
// in the library <title>/extras/<extraId>/, read by its package.json; before
// it the package store's extras/{shard2}/{extraId}/, read by its
// manifest.json. Either is laid out as an item's package, without trickplay.
// It is served under its title's id, with the routes of an item's packaged
// renditions:
//
//	/api/play/{itemId}/extras/{extraId}/master.m3u8
//	/api/play/{itemId}/extras/{extraId}/{vN|aN|sN}/playlist.m3u8
//	/api/play/{itemId}/extras/{extraId}/{vN|aN}/iframes.m3u8, init.mp4, seg-{n}.m4s
//	/api/play/{itemId}/extras/{extraId}/{sN}/seg-{n}.vtt
//
// Every one of them answers 404 unless extraId is a UUID, katalog-api
// answers it as an extra of itemId (it answers only extras packaged and not
// removed), and its package is complete (.complete, which only the folder
// can say). chino-api holds a viewer to the rating of the title in the URL;
// the title check makes that the extra's own title, so an extra never plays
// under another one. A file missing from the folder answered resolves the
// extra once more, as an item's does.
//
// The master is the client's share of the extra's ladder (serveLadder: the
// rungs it decodes at its heights, or the one ?q= names), every URI in it
// carrying the request's query (the stream token, caps, q).
//
// An extra is no item: katalog-api lists it in neither the packaged ids nor
// answers it as an item, so it is not in the Zap pool, and
// /api/play/{extraId}/… finds no package.

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

// extraPackage is the package of the extra extraID of the title itemID, nil
// when there is none to serve: extraID is no UUID, katalog-api knows no such
// packaged extra or answers it as another title's, or its package is not
// complete. err when katalog-api could not say. force asks katalog-api
// again (a file was missing from the folder it answered).
func (h *HLSHandler) extraPackage(ctx context.Context, itemID, extraID string, force bool) (*pkgDir, error) {
	if !uuidPattern.MatchString(extraID) {
		return nil, nil
	}
	x, err := h.Packages.extra(ctx, extraID, force)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if x.ItemID == "" || x.ItemID != itemID {
		return nil, nil
	}
	ref := catalog.PackageRef{Dir: x.Dir, Record: x.Record}
	if !h.Packages.ready(ref, true) {
		return nil, nil
	}
	return pkgDirOf(ref), nil
}

// extraFile is the file rel of the package of the extra r names, and the
// retry to serve it with should it be missing there (packagedFile); "" when
// that extra is not one to serve (extraPackage), which it has answered 404,
// or 502 when katalog-api could not say.
func (h *HLSHandler) extraFile(w http.ResponseWriter, r *http.Request, rel ...string) (string, func() string) {
	itemID, extraID := chi.URLParam(r, "itemId"), chi.URLParam(r, "extraId")
	p, err := h.extraPackage(r.Context(), itemID, extraID, false)
	if err != nil {
		packagedError(w, err)
		return "", nil
	}
	if p == nil {
		http.Error(w, "extra not found", http.StatusNotFound)
		return "", nil
	}
	return p.path(rel...), func() string {
		h.Packages.forgetComplete(p.dir)
		again, err := h.extraPackage(r.Context(), itemID, extraID, true)
		if err != nil || again == nil || again.dir == p.dir {
			return ""
		}
		return again.path(rel...)
	}
}

// ExtraMaster serves an extra's master as the client is served it: its
// share of the ladder for ?caps= and ?q=, every URI carrying the query.
func (h *HLSHandler) ExtraMaster(w http.ResponseWriter, r *http.Request) {
	path, retry := h.extraFile(w, r, "hls", "master.m3u8")
	if path == "" {
		return
	}
	caps := ParseCaps(r.URL.Query().Get("caps"))
	q := r.URL.Query().Get("q")
	servePlaylistCachedTransform(w, r, path, r.URL.RawQuery, retry, func(body string) string {
		return serveLadder(body, caps, q).body
	})
}

// ExtraRenditionPlaylist serves the media playlist of an extra's video,
// audio or WebVTT rendition, its URIs carrying the query.
func (h *HLSHandler) ExtraRenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	if path, retry := h.extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "playlist.m3u8"); path != "" {
		servePlaylistCached(w, r, path, retry)
	}
}

// ExtraIframesPlaylist serves the I-frame playlist of an extra's video rung.
func (h *HLSHandler) ExtraIframesPlaylist(w http.ResponseWriter, r *http.Request) {
	if path, retry := h.extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "iframes.m3u8"); path != "" {
		serveIframesPlaylist(w, r, path, retry)
	}
}

// ExtraInitSegment serves the init.mp4 of an extra's video or audio
// rendition.
func (h *HLSHandler) ExtraInitSegment(w http.ResponseWriter, r *http.Request) {
	if path, retry := h.extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "init.mp4"); path != "" {
		servePackagedStaticRetry(w, r, path, retry, "video/mp4", "init not found")
	}
}

// ExtraSegment serves one media segment of an extra's video or audio
// rendition (seg-NNNNN.m4s, as shaka names them), ranges and all.
func (h *HLSHandler) ExtraSegment(w http.ResponseWriter, r *http.Request) {
	if path, retry := h.extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".m4s"); path != "" {
		servePackagedStaticRetry(w, r, path, retry, "video/iso.segment", "segment not found")
	}
}

// ExtraSubtitleSegment serves one WebVTT segment of an extra's subtitle
// rendition (sN).
func (h *HLSHandler) ExtraSubtitleSegment(w http.ResponseWriter, r *http.Request) {
	if path, retry := h.extraFile(w, r, "hls", chi.URLParam(r, "rendId"), "seg-"+chi.URLParam(r, "seg")+".vtt"); path != "" {
		servePackagedStaticRetry(w, r, path, retry, "text/vtt; charset=utf-8", "subtitle segment not found")
	}
}
