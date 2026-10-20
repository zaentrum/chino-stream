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

	// The live worker (ADR-0013, phase 1): every call chino-stream makes to
	// it, one per attempt. kind is segment, init or subtitle; track video,
	// audio or subtitle; priority playhead or prefetch; result ok, the
	// worker's error class (source, circuit, busy, deadline, bad-request,
	// unauthorized, internal), unreachable (no answer: refused, reset, timed
	// out) or canceled (the caller went away).
	LiveWorkerRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_live_worker_requests_total",
		Help: "Calls to the live worker, by kind, track, priority and result.",
	}, []string{"kind", "track", "priority", "result"})

	// LiveWorkerDuration is how long one call took, queueing on the worker
	// and its own retries included.
	LiveWorkerDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "katalog_stream_live_worker_request_duration_seconds",
		Help:    "Duration of one call to the live worker.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 12, 20, 30, 60, 120},
	}, []string{"kind", "track", "priority", "result"})

	// LiveWorkerRetries counts the calls chino-stream made again: busy (a
	// playhead call the worker had no slot for, after Retry-After),
	// unreachable (a refused or reset connection, after 200 ms) and codec
	// (a fragment whose parameter sets were not those of the rung's init).
	LiveWorkerRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_live_worker_retries_total",
		Help: "Calls to the live worker made once more, by kind and reason.",
	}, []string{"kind", "reason"})

	// LiveWorkerFallbacks counts what was not served by the live worker
	// although it would have been: unready (a new session sent to ffmpeg
	// because the worker was not ready) and refused (a segment the worker
	// cannot make, answered 404 so the player falls back).
	LiveWorkerFallbacks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_live_worker_fallbacks_total",
		Help: "Live encodes not served by the live worker, by reason.",
	}, []string{"reason"})

	// LiveWorkerInflight is the calls to the live worker under way.
	LiveWorkerInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "katalog_stream_live_worker_inflight",
		Help: "Calls to the live worker under way, by priority.",
	}, []string{"priority"})

	// LiveWorkerShadow counts the checks of LIVE_WORKER_MODE shadow: the
	// worker asked for a segment ffmpeg served (video, audio), or the Go
	// remux of one (copy, package-audio), and what came back compared and
	// dropped. result ok, frames (a frame count unlike ffmpeg's), codec (a
	// rung's parameter sets changed), span (not the grid's segment), busy
	// (no slot to spare), skipped (too many checks under way) or an error.
	LiveWorkerShadow = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_live_worker_shadow_checks_total",
		Help: "Shadow checks of the live pipeline, by track and result.",
	}, []string{"track", "result"})

	// Remuxes counts what chino-stream remuxes in Go, with no encoder (the
	// live pipeline's stream copy and package audio, and a package's
	// subtitles shifted): kind and result (ok or error).
	Remuxes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_stream_remux_total",
		Help: "Go remuxes of the live pipeline, by kind and result.",
	}, []string{"kind", "result"})
)

// Handler returns the Prometheus scrape handler. Mounted at /metrics
// (un-authed; service is cluster-internal).
func Handler() http.Handler {
	return promhttp.Handler()
}
