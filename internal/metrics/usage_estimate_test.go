// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/envoyproxy/ai-gateway/internal/testing/testotel"
)

func TestUsageEstimate_RecordRequest(t *testing.T) {
	mr := metric.NewManualReader()
	m := NewUsageEstimate(metric.NewMeterProvider(metric.WithReader(mr)).Meter("test"))

	m.RecordRequest(t.Context(), "estimated_input_token", "model-a", UsageEstimateOutcomeEstimated)
	m.RecordRequest(t.Context(), "estimated_input_token", "model-a", UsageEstimateOutcomeEstimated)
	m.RecordRequest(t.Context(), "estimated_input_token", "model-a", UsageEstimateOutcomeCold)
	m.RecordRequest(t.Context(), "estimated_input_token", "model-a", UsageEstimateOutcomeError)

	attrs := func(outcome UsageEstimateOutcome) attribute.Set {
		return attribute.NewSet(
			attribute.Key(usageEstimateAttributeKey).String("estimated_input_token"),
			attribute.Key(genaiAttributeOriginalModel).String("model-a"),
			attribute.Key(usageEstimateAttributeOutcome).String(string(outcome)),
		)
	}
	require.Equal(t, 2.0, testotel.GetCounterValue(t, mr, usageEstimateMetricRequests, attrs(UsageEstimateOutcomeEstimated)))
	require.Equal(t, 1.0, testotel.GetCounterValue(t, mr, usageEstimateMetricRequests, attrs(UsageEstimateOutcomeCold)))
	require.Equal(t, 1.0, testotel.GetCounterValue(t, mr, usageEstimateMetricRequests, attrs(UsageEstimateOutcomeError)))
}

func TestUsageEstimate_RecordRatio(t *testing.T) {
	mr := metric.NewManualReader()
	m := NewUsageEstimate(metric.NewMeterProvider(metric.WithReader(mr)).Meter("test"))

	m.RecordRatio(t.Context(), "estimated_input_token", "model-a", 0.5)
	m.RecordRatio(t.Context(), "estimated_input_token", "model-a", 1.5)

	attrs := attribute.NewSet(
		attribute.Key(usageEstimateAttributeKey).String("estimated_input_token"),
		attribute.Key(genaiAttributeOriginalModel).String("model-a"),
	)
	count, sum := testotel.GetHistogramValues(t, mr, usageEstimateMetricRatio, attrs)
	require.Equal(t, uint64(2), count)
	require.Equal(t, 2.0, sum)

	var rm metricdata.ResourceMetrics
	require.NoError(t, mr.Collect(t.Context(), &rm))
	h := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	require.Equal(t, usageEstimateRatioBuckets, h.DataPoints[0].Bounds)
}
