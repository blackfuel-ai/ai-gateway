// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
)

func TestQuotaBucketCostExpression(t *testing.T) {
	require.Equal(t, "total_tokens", QuotaBucketCostExpression(&aigv1a1.QuotaDefinition{}, &aigv1a1.QuotaValue{}))
	require.Equal(t, "input_tokens", QuotaBucketCostExpression(
		&aigv1a1.QuotaDefinition{CostExpression: ptr.To("input_tokens")}, &aigv1a1.QuotaValue{}))
	require.Equal(t, "output_tokens", QuotaBucketCostExpression(
		&aigv1a1.QuotaDefinition{CostExpression: ptr.To("input_tokens")},
		&aigv1a1.QuotaValue{CostExpression: ptr.To("output_tokens")}))
}

func TestResolveQuotaAdmissionReserve(t *testing.T) {
	t.Run("defaults of the optional fields", func(t *testing.T) {
		spec, err := ResolveQuotaAdmissionReserve(
			&aigv1a1.QuotaDefinition{CostExpression: ptr.To("input_tokens")},
			&aigv1a1.QuotaValue{AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{EstimateByHeader: "x-client-id", Percent: 90}})
		require.NoError(t, err)
		require.Equal(t, QuotaAdmissionReserveSpec{
			CostExpression: "input_tokens", EstimateByHeader: "x-client-id", Percent: 90,
			Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5,
		}, spec)
	})

	t.Run("explicit fields", func(t *testing.T) {
		spec, err := ResolveQuotaAdmissionReserve(&aigv1a1.QuotaDefinition{}, &aigv1a1.QuotaValue{
			CostExpression: ptr.To("output_tokens"),
			AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{
				EstimateByHeader: "x-key", Percent: 85, Window: ptr.To(gwapiv1.Duration("30s")),
				MaxFailurePercent: ptr.To(uint32(0)), MinSamples: ptr.To(uint32(1)),
			},
		})
		require.NoError(t, err)
		require.Equal(t, QuotaAdmissionReserveSpec{
			CostExpression: "output_tokens", EstimateByHeader: "x-key", Percent: 85,
			Window: 30 * time.Second, MaxFailurePercent: 0, MinSamples: 1,
		}, spec)
	})

	t.Run("malformed window", func(t *testing.T) {
		_, err := ResolveQuotaAdmissionReserve(&aigv1a1.QuotaDefinition{}, &aigv1a1.QuotaValue{
			AdmissionReserve: &aigv1a1.QuotaAdmissionReserve{
				EstimateByHeader: "x-key", Percent: 85, Window: ptr.To(gwapiv1.Duration("soon")),
			},
		})
		require.ErrorContains(t, err, `admission reserve window "soon"`)
	})
}

func TestQuotaAdmissionReserveSpec_MetadataKey(t *testing.T) {
	base := QuotaAdmissionReserveSpec{
		CostExpression: "input_tokens", EstimateByHeader: "x-client-id", Percent: 90,
		Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5,
	}
	key := base.MetadataKey(QuotaCostRuleBucketKey(1))
	require.Regexp(t, `^quota_reserve_rule-1_[0-9a-f]{16}$`, key)
	require.Equal(t, key, base.MetadataKey(QuotaCostRuleBucketKey(1)), "stable for the same spec")
	require.NotEqual(t, QuotaCostMetadataKey(QuotaCostRuleBucketKey(1)), key)
	require.NotEqual(t, key, base.MetadataKey(QuotaCostDefaultBucketKey()))

	for name, mutate := range map[string]func(*QuotaAdmissionReserveSpec){
		"expression":  func(s *QuotaAdmissionReserveSpec) { s.CostExpression = "output_tokens" },
		"header":      func(s *QuotaAdmissionReserveSpec) { s.EstimateByHeader = "x-other" },
		"percent":     func(s *QuotaAdmissionReserveSpec) { s.Percent = 85 },
		"window":      func(s *QuotaAdmissionReserveSpec) { s.Window = 30 * time.Second },
		"max failure": func(s *QuotaAdmissionReserveSpec) { s.MaxFailurePercent = 10 },
		"min samples": func(s *QuotaAdmissionReserveSpec) { s.MinSamples = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			require.NotEqual(t, key, other.MetadataKey(QuotaCostRuleBucketKey(1)))
		})
	}
}
