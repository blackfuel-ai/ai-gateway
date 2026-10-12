// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// nolint: godot
const (
	// Usage Estimate Requests is a counter metric that records the requests a
	// usage estimate was computed for, by outcome: estimated, cold (no successful
	// response in the last completed period) or error (the CEL evaluation failed).
	//
	// Dimensions:
	// - aigw.usage_estimate.key
	// - gen_ai.original.model
	// - aigw.usage_estimate.outcome
	usageEstimateMetricRequests = "aigw.usage_estimate.requests"
	// Usage Estimate Ratio is a histogram metric that records the estimate of a
	// successful request divided by the same CEL expression evaluated on the
	// usage its response reported.
	//
	// Dimensions:
	// - aigw.usage_estimate.key
	// - gen_ai.original.model
	usageEstimateMetricRatio = "aigw.usage_estimate.ratio"

	usageEstimateAttributeKey     = "aigw.usage_estimate.key"
	usageEstimateAttributeOutcome = "aigw.usage_estimate.outcome"
)

// UsageEstimateOutcome is what computing a usage estimate for a request ended with.
type UsageEstimateOutcome string

const (
	// UsageEstimateOutcomeEstimated is an estimate drawn from the last completed period.
	UsageEstimateOutcomeEstimated UsageEstimateOutcome = "estimated"
	// UsageEstimateOutcomeCold is no estimate: the last completed period holds no
	// successful response of the key.
	UsageEstimateOutcomeCold UsageEstimateOutcome = "cold"
	// UsageEstimateOutcomeError is no estimate: the CEL expression failed on the
	// estimated usage.
	UsageEstimateOutcomeError UsageEstimateOutcome = "error"
)

// usageEstimateRatioBuckets are centered on 1, an exact estimate, and spread
// evenly on a log scale from a quarter to four times the actual value.
var usageEstimateRatioBuckets = []float64{0.25, 0.5, 0.67, 0.8, 0.9, 0.95, 1, 1.05, 1.1, 1.25, 1.5, 2, 4}

// UsageEstimateMetrics records how often the usage estimates exist and how close
// they are to the actual usage.
//
// The value of the header grouping the requests is never an attribute: it is
// typically an API key identity, whose cardinality is unbounded.
type UsageEstimateMetrics interface {
	// RecordRequest records that the usage estimate metadataKey was computed for a
	// request of originalModel, and its outcome.
	RecordRequest(ctx context.Context, metadataKey, originalModel string, outcome UsageEstimateOutcome)
	// RecordRatio records the estimate divided by the actual value of a request.
	RecordRatio(ctx context.Context, metadataKey, originalModel string, ratio float64)
}

type usageEstimate struct {
	requests metric.Float64Counter
	ratio    metric.Float64Histogram
}

// NewUsageEstimate creates the usage estimate metrics.
func NewUsageEstimate(meter metric.Meter) UsageEstimateMetrics {
	return &usageEstimate{
		requests: mustRegisterCounter(meter,
			usageEstimateMetricRequests,
			metric.WithDescription("Number of requests a usage estimate was computed for"),
		),
		ratio: mustRegisterHistogram(meter,
			usageEstimateMetricRatio,
			metric.WithDescription("Usage estimate divided by the actual value"),
			metric.WithExplicitBucketBoundaries(usageEstimateRatioBuckets...),
		),
	}
}

// RecordRequest implements [UsageEstimateMetrics.RecordRequest].
func (u *usageEstimate) RecordRequest(ctx context.Context, metadataKey, originalModel string, outcome UsageEstimateOutcome) {
	u.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.Key(usageEstimateAttributeKey).String(metadataKey),
		attribute.Key(genaiAttributeOriginalModel).String(originalModel),
		attribute.Key(usageEstimateAttributeOutcome).String(string(outcome)),
	))
}

// RecordRatio implements [UsageEstimateMetrics.RecordRatio].
func (u *usageEstimate) RecordRatio(ctx context.Context, metadataKey, originalModel string, ratio float64) {
	u.ratio.Record(ctx, ratio, metric.WithAttributes(
		attribute.Key(usageEstimateAttributeKey).String(metadataKey),
		attribute.Key(genaiAttributeOriginalModel).String(originalModel),
	))
}
