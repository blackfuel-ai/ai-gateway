// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package filterapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
)

func TestServer_LoadConfig(t *testing.T) {
	now := time.Now()

	t.Run("ok", func(t *testing.T) {
		config := &Config{
			EmitErrorMetadata: true,
			LLMRequestCosts: []LLMRequestCost{
				{MetadataKey: "key", RouteName: "ns/test-route", Type: LLMRequestCostTypeOutputToken},
				{MetadataKey: "cel_key", RouteName: "ns/test-route", Type: LLMRequestCostTypeCEL, CEL: "1 + 1"},
			},
			Backends: []Backend{
				{Name: "kserve", Schema: VersionedAPISchema{Name: APISchemaOpenAI}},
				{Name: "awsbedrock", Schema: VersionedAPISchema{Name: APISchemaAWSBedrock}},
				{Name: "openai", Schema: VersionedAPISchema{Name: APISchemaOpenAI}, Auth: &BackendAuth{APIKey: &APIKeyAuth{Key: "dummy"}}},
			},
			Models: []Model{
				{
					Name:      "llama3.3333",
					OwnedBy:   "meta",
					CreatedAt: now,
				},
				{
					Name:      "gpt4.4444",
					OwnedBy:   "openai",
					CreatedAt: now,
				},
			},
		}
		rc, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, b *BackendAuth) (BackendAuthHandler, error) {
			require.NotNil(t, b)
			require.NotNil(t, b.APIKey)
			require.Equal(t, "dummy", b.APIKey.Key)
			return nil, nil
		})
		require.NoError(t, err)

		require.NotNil(t, rc)

		require.Len(t, rc.RequestCosts, 2)
		require.Equal(t, LLMRequestCostTypeOutputToken, rc.RequestCosts[0].Type)
		require.Equal(t, "key", rc.RequestCosts[0].MetadataKey)
		require.Equal(t, LLMRequestCostTypeCEL, rc.RequestCosts[1].Type)
		require.Equal(t, "1 + 1", rc.RequestCosts[1].CEL)
		prog := rc.RequestCosts[1].CELProg
		require.NotNil(t, prog)
		val, err := llmcostcel.EvaluateProgram(prog, "", "", "", 1, 1, 1, 1, 1, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(2), val)
		require.Equal(t, config.Models, rc.DeclaredModels)
		require.True(t, rc.EmitErrorMetadata)
	})

	t.Run("with global costs", func(t *testing.T) {
		config := &Config{
			GlobalLLMRequestCosts: []GlobalLLMRequestCost{
				{MetadataKey: "global_input", Type: LLMRequestCostTypeInputToken},
				{MetadataKey: "global_cel", Type: LLMRequestCostTypeCEL, CEL: "input_tokens + output_tokens"},
			},
			LLMRequestCosts: []LLMRequestCost{
				{MetadataKey: "route_output", RouteName: "ns/route1", Type: LLMRequestCostTypeOutputToken},
			},
		}
		rc, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.NoError(t, err)

		require.Len(t, rc.GlobalRequestCosts, 2)
		require.Equal(t, LLMRequestCostTypeInputToken, rc.GlobalRequestCosts[0].Type)
		require.Equal(t, "global_input", rc.GlobalRequestCosts[0].MetadataKey)
		require.Equal(t, LLMRequestCostTypeCEL, rc.GlobalRequestCosts[1].Type)
		require.Equal(t, "input_tokens + output_tokens", rc.GlobalRequestCosts[1].CEL)
		require.NotNil(t, rc.GlobalRequestCosts[1].CELProg)

		require.Len(t, rc.RequestCosts, 1)
		require.Equal(t, "route_output", rc.RequestCosts[0].MetadataKey)
		require.Equal(t, "ns/route1", rc.RequestCosts[0].RouteName)
	})

	t.Run("error - invalid CEL in global cost", func(t *testing.T) {
		config := &Config{
			GlobalLLMRequestCosts: []GlobalLLMRequestCost{
				{MetadataKey: "bad_cel", Type: LLMRequestCostTypeCEL, CEL: "invalid syntax ++"},
			},
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot create CEL program for global cost")
	})

	t.Run("with usage estimates", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "estimated_input", CEL: "input_tokens", ByHeader: "x-client-id", EmitMetric: true},
				{MetadataKey: "estimated_cached", CEL: "cached_input_tokens", ByHeader: "x-client-id"},
				{MetadataKey: "estimated_cache_rate", CEL: "cache_rate", ByHeader: "x-client-id"},
			},
			UsageEstimatePeriod: time.Minute,
		}
		rc, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.NoError(t, err)

		require.Equal(t, time.Minute, rc.UsageEstimatePeriod)
		require.Len(t, rc.UsageEstimates, 3)
		for i := range config.UsageEstimates {
			require.Equal(t, &config.UsageEstimates[i], rc.UsageEstimates[i].UsageEstimate)
		}
		in := llmcostcel.EstimateInputs{InputTokens: 10, CachedInputTokens: 7, TotalTokens: 10, CacheRate: 0.7}
		val, err := llmcostcel.EvaluateEstimateProgram(rc.UsageEstimates[1].CELProg, in)
		require.NoError(t, err)
		require.Equal(t, float64(7), val)
		val, err = llmcostcel.EvaluateEstimateProgram(rc.UsageEstimates[2].CELProg, in)
		require.NoError(t, err)
		require.Equal(t, 0.7, val)
	})

	t.Run("with admission reserves", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "estimated_input", CEL: "input_tokens", ByHeader: "x-client-id"},
				{MetadataKey: "estimated_fresh", CEL: "input_tokens - cached_input_tokens", ByHeader: "x-client-id"},
			},
			UsageEstimatePeriod: time.Minute,
			AdmissionReserves: []AdmissionReserve{
				{MetadataKey: "quota_reserve_estimated_fresh_90", UsageEstimate: "estimated_fresh", Percent: 90},
				{MetadataKey: "quota_reserve_estimated_input_100", UsageEstimate: "estimated_input", Percent: 100},
			},
		}
		rc, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.NoError(t, err)
		require.Equal(t, config.AdmissionReserves, rc.AdmissionReserves)
	})

	t.Run("error - admission reserve on an undeclared usage estimate", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "estimated_input", CEL: "input_tokens", ByHeader: "x-client-id"},
			},
			UsageEstimatePeriod: time.Minute,
			AdmissionReserves: []AdmissionReserve{
				{MetadataKey: "quota_reserve_estimated_fresh_90", UsageEstimate: "estimated_fresh", Percent: 90},
			},
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.ErrorContains(t, err, `admission reserve "quota_reserve_estimated_fresh_90" references undeclared usage estimate "estimated_fresh"`)
	})

	t.Run("usage estimate header", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "estimated_input_token", CEL: "input_tokens", ByHeader: "x-client-id", EmitHeader: true},
				{MetadataKey: "estimated_cache_rate", CEL: "cache_rate", ByHeader: "x-client-id"},
			},
			UsageEstimatePeriod: time.Minute,
		}
		rc, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.NoError(t, err)
		require.Equal(t, "x-ai-eg-usage-estimate-estimated-input-token", rc.UsageEstimates[0].Header)
		require.Empty(t, rc.UsageEstimates[1].Header)
	})

	t.Run("error - usage estimate header from an invalid metadata key", func(t *testing.T) {
		for _, key := range []string{"Estimated_Input", "estimated-input", "estimated.input"} {
			config := &Config{
				UsageEstimates:      []UsageEstimate{{MetadataKey: key, CEL: "input_tokens", ByHeader: "x-client-id", EmitHeader: true}},
				UsageEstimatePeriod: time.Minute,
			}
			_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
				return nil, nil
			})
			require.ErrorContains(t, err, `usage estimate "`+key+`" emits a header: its metadata key must contain only lower-case letters, digits and underscores`)
		}
	})

	t.Run("error - invalid CEL in usage estimate", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "bad_cel", CEL: "bad syntax @@", ByHeader: "x-client-id"},
			},
			UsageEstimatePeriod: time.Minute,
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.ErrorContains(t, err, `cannot create CEL program for usage estimate "bad_cel"`)
	})

	t.Run("error - usage estimates without a period", func(t *testing.T) {
		config := &Config{
			UsageEstimates: []UsageEstimate{
				{MetadataKey: "estimated_input", CEL: "input_tokens", ByHeader: "x-client-id"},
			},
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.ErrorContains(t, err, "usage estimate period must be positive, got 0s")
	})

	t.Run("error - invalid CEL in route cost", func(t *testing.T) {
		config := &Config{
			LLMRequestCosts: []LLMRequestCost{
				{MetadataKey: "bad_cel", RouteName: "ns/route1", Type: LLMRequestCostTypeCEL, CEL: "bad syntax @@"},
			},
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot create CEL program for cost")
	})

	t.Run("error - route cost with empty RouteName", func(t *testing.T) {
		config := &Config{
			LLMRequestCosts: []LLMRequestCost{
				{MetadataKey: "missing_route", RouteName: "", Type: LLMRequestCostTypeInputToken},
			},
		}
		_, err := NewRuntimeConfig(t.Context(), config, func(_ context.Context, _ *BackendAuth) (BackendAuthHandler, error) {
			return nil, nil
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "must have non-empty RouteName")
		require.Contains(t, err.Error(), "missing_route")
	})
}
