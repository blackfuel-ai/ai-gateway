// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/ratelimit/translator"
)

func TestQuotaCostBuckets(t *testing.T) {
	t.Run("default bucket falls back to model expression then total_tokens", func(t *testing.T) {
		buckets, err := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{Limit: 10, Duration: "1h"},
		})
		require.NoError(t, err)
		require.Equal(t, []quotaCostBucket{{key: "default", expr: "total_tokens"}}, buckets)

		buckets, err = quotaCostBuckets(&aigv1a1.QuotaDefinition{
			CostExpression: ptr.To("input_tokens"),
			DefaultBucket:  &aigv1a1.QuotaValue{Limit: 10, Duration: "1h"},
		})
		require.NoError(t, err)
		require.Equal(t, []quotaCostBucket{{key: "default", expr: "input_tokens"}}, buckets)
	})

	t.Run("bucket expression wins over model expression", func(t *testing.T) {
		buckets, err := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			CostExpression: ptr.To("input_tokens"),
			BucketRules: []aigv1a1.QuotaRule{
				{Quota: aigv1a1.QuotaValue{Limit: 1, Duration: "1m", CostExpression: ptr.To("output_tokens")}},
				{Quota: aigv1a1.QuotaValue{Limit: 2, Duration: "1h"}},
			},
		})
		require.NoError(t, err)
		require.Equal(t, []quotaCostBucket{
			{key: "rule-0", expr: "output_tokens"},
			{key: "rule-1", expr: "input_tokens"},
		}, buckets)
	})

	t.Run("requests-metric buckets are skipped", func(t *testing.T) {
		buckets, err := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{Limit: 10, Duration: "1h", CostMetric: aigv1a1.QuotaCostMetricRequests},
			BucketRules: []aigv1a1.QuotaRule{
				{Quota: aigv1a1.QuotaValue{Limit: 1, Duration: "1m", CostMetric: aigv1a1.QuotaCostMetricRequests}},
				{Quota: aigv1a1.QuotaValue{Limit: 2, Duration: "1d", CostMetric: aigv1a1.QuotaCostMetricTokens, CostExpression: ptr.To("output_tokens")}},
			},
		})
		require.NoError(t, err)
		require.Equal(t, []quotaCostBucket{{key: "rule-1", expr: "output_tokens"}}, buckets)
	})

	t.Run("admission reserve is carried with its bucket", func(t *testing.T) {
		buckets, err := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{
				Limit: 10, Duration: "1m",
				AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{EstimateByHeader: "x-client-id", Percent: 100},
			},
			BucketRules: []aigv1a1.QuotaRule{
				{Quota: aigv1a1.QuotaValue{Limit: 1, Duration: "1m", CostExpression: ptr.To("output_tokens")}},
				{Quota: aigv1a1.QuotaValue{
					Limit: 2, Duration: "1m", CostExpression: ptr.To("input_tokens - cached_input_tokens"),
					AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{
						EstimateByHeader:  "x-client-id",
						Percent:           85,
						Window:            ptr.To(gwapiv1.Duration("30s")),
						MaxFailurePercent: ptr.To(uint32(10)),
						MinSamples:        ptr.To(uint32(3)),
					},
				}},
			},
		})
		require.NoError(t, err)
		defaultKey := translator.QuotaAdmissionReserveSpec{
			CostExpression: "total_tokens", EstimateByHeader: "x-client-id", Percent: 100,
			Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5,
		}.MetadataKey("default")
		ruleKey := translator.QuotaAdmissionReserveSpec{
			CostExpression: "input_tokens - cached_input_tokens", EstimateByHeader: "x-client-id", Percent: 85,
			Window: 30 * time.Second, MaxFailurePercent: 10, MinSamples: 3,
		}.MetadataKey("rule-1")
		require.Equal(t, []quotaCostBucket{
			{key: "default", expr: "total_tokens", reserve: &filterapi.LLMRequestCostAdmissionReserve{
				MetadataKey: defaultKey, EstimateByHeader: "x-client-id", Percent: 100,
				Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5,
			}},
			{key: "rule-0", expr: "output_tokens"},
			{key: "rule-1", expr: "input_tokens - cached_input_tokens", reserve: &filterapi.LLMRequestCostAdmissionReserve{
				MetadataKey: ruleKey, EstimateByHeader: "x-client-id", Percent: 85,
				Window: 30 * time.Second, MaxFailurePercent: 10, MinSamples: 3,
			}},
		}, buckets)
	})

	t.Run("malformed admission reserve window is an error", func(t *testing.T) {
		_, err := quotaCostBuckets(&aigv1a1.QuotaDefinition{
			DefaultBucket: &aigv1a1.QuotaValue{
				Limit: 10, Duration: "1m",
				AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{
					EstimateByHeader: "x-client-id", Percent: 90, Window: ptr.To(gwapiv1.Duration("soon")),
				},
			},
		})
		require.ErrorContains(t, err, `admission reserve window "soon"`)
	})
}
