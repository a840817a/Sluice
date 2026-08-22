// Package metrics provides Prometheus metrics for the DASH gateway.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// SegmentsFetched counts completed segment fetches, labelled by channel and
	// result status ("ok", "error", "write_error").
	SegmentsFetched = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mpd_segments_fetched_total",
		Help: "Total number of segment fetch attempts, by channel and status.",
	}, []string{"channel", "status"})

	// SegmentsPublished counts segments that passed the A/V barrier and were
	// published, labelled by channel and media type.
	SegmentsPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mpd_segments_published_total",
		Help: "Total number of segments published through the A/V barrier.",
	}, []string{"channel", "media_type"})

	// BrokerDepth tracks the current task queue depth per channel.
	BrokerDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mpd_broker_queue_depth",
		Help: "Current number of tasks in the broker queue.",
	}, []string{"channel"})

	// SegmentWriteDuration measures disk write latency for media segments.
	SegmentWriteDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mpd_segment_write_duration_seconds",
		Help:    "Latency of atomic segment writes to disk.",
		Buckets: prometheus.DefBuckets,
	})
)
