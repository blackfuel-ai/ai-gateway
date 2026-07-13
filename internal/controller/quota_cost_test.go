// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
)

func TestQuotaCostBuckets(t *testing.T) {
	t.Run("default bucket falls back to model expression then total_tokens", func(t *testing.T) {
		buckets := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{Limit: 10, Duration: "1h"},
		})
		require.Equal(t, []quotaCostBucket{{key: "default", expr: "total_tokens"}}, buckets)

		buckets = quotaCostBuckets(&aigv1a1.QuotaDefinition{
			CostExpression: ptr.To("input_tokens"),
			DefaultBucket:  &aigv1a1.QuotaValue{Limit: 10, Duration: "1h"},
		})
		require.Equal(t, []quotaCostBucket{{key: "default", expr: "input_tokens"}}, buckets)
	})

	t.Run("bucket expression wins over model expression", func(t *testing.T) {
		buckets := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			CostExpression: ptr.To("input_tokens"),
			BucketRules: []aigv1a1.QuotaRule{
				{Quota: aigv1a1.QuotaValue{Limit: 1, Duration: "1m", CostExpression: ptr.To("output_tokens")}},
				{Quota: aigv1a1.QuotaValue{Limit: 2, Duration: "1h"}},
			},
		})
		require.Equal(t, []quotaCostBucket{
			{key: "rule-0", expr: "output_tokens"},
			{key: "rule-1", expr: "input_tokens"},
		}, buckets)
	})

	t.Run("requests-metric buckets are skipped", func(t *testing.T) {
		buckets := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{Limit: 10, Duration: "1h", CostMetric: aigv1a1.QuotaCostMetricRequests},
			BucketRules: []aigv1a1.QuotaRule{
				{Quota: aigv1a1.QuotaValue{Limit: 1, Duration: "1m", CostMetric: aigv1a1.QuotaCostMetricRequests}},
				{Quota: aigv1a1.QuotaValue{Limit: 2, Duration: "1d", CostMetric: aigv1a1.QuotaCostMetricTokens, CostExpression: ptr.To("output_tokens")}},
			},
		})
		require.Equal(t, []quotaCostBucket{{key: "rule-1", expr: "output_tokens"}}, buckets)
	})
}
