package play

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The quality of the on-the-fly ladder is one of three words, whole, for an
// item and for an extra alike: chi anchors a route's pattern as ^…$ without
// grouping it, so an ungrouped alternation let "highx" or "xlow" through.
func TestAQualityIsOneOfThreeWordsWhole(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/api/play/{itemId}", (&HLSHandler{}).Routes)
	const item = "/api/play/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	const extra = item + "/extras/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	for _, base := range []string{item, extra} {
		for _, q := range []string{"high", "medium", "low"} {
			for _, f := range []string{"index.m3u8", "init.mp4", "3.m4s"} {
				if p := base + "/" + q + "/" + f; !r.Match(chi.NewRouteContext(), http.MethodGet, p) {
					t.Errorf("%s does not route", p)
				}
			}
		}
		for _, q := range []string{"highx", "xlow", "xmediumx", "HIGH"} {
			for _, f := range []string{"index.m3u8", "init.mp4", "3.m4s"} {
				if p := base + "/" + q + "/" + f; r.Match(chi.NewRouteContext(), http.MethodGet, p) {
					t.Errorf("%s routes", p)
				}
			}
		}
	}
}
