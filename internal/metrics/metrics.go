// Package metrics is chino-stream's Prometheus surface. Counters and
// histograms incremented from the play handler so the Media Platform
// dashboard sees what the streaming layer is doing.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// PlayRequests counts every /api/play call by dispatch mode.
	// Cardinality is bounded: passthrough / remux / transcode.
	PlayRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_play_requests_total",
		Help: "Play requests dispatched by chino-stream, labelled by mode.",
	}, []string{"mode"})

	// TranscodesActive is a per-pod gauge of in-flight transcode
	// pipelines. Incremented when ffmpeg starts, decremented when it
	// finishes (or the client aborts). Pod-local — the dashboard sums
	// across replicas to get cluster total.
	TranscodesActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "katalog_stream_transcodes_active",
		Help: "Currently active ffmpeg pipelines on this pod.",
	})

	// BytesServed tracks egress per mode. Histograms-of-bytes would be
	// noisier than the counter; use rate() in the dashboard.
	BytesServed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_bytes_served_total",
		Help: "Bytes streamed to clients by mode.",
	}, []string{"mode"})

	// FFprobeDuration is the time we spend on the codec probe for each
	// new request. The play endpoint runs ffprobe before deciding
	// dispatch mode, so this is on the cold-start path.
	FFprobeDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "katalog_stream_ffprobe_duration_seconds",
		Help:    "ffprobe execution time (called once per play request).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	})

	// TranscodeDuration: full lifetime of a transcode/remux session
	// (start → ffmpeg.Wait()). Captures total session length, not
	// processing latency — long sessions are healthy here, just informative.
	TranscodeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "katalog_stream_transcode_duration_seconds",
		Help:    "Total session duration in transcode/remux modes.",
		Buckets: []float64{1, 10, 60, 300, 600, 1800, 3600, 7200},
	}, []string{"mode"})
)

// Handler returns the Prometheus scrape handler. Mounted at /metrics
// (un-authed; service is cluster-internal).
func Handler() http.Handler {
	return promhttp.Handler()
}
