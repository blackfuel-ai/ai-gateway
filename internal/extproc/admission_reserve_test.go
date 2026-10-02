// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"log/slog"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/quotareserve"
	tracingapi "github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"
)

const (
	testFreshInputExpr = "input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)"
	testReserveKey     = "quota_reserve_rule-1_test"
)

// reserveRuntimeConfig returns a runtime config with one fresh-input bucket
// carrying an admission reserve, backed by store.
func reserveRuntimeConfig(t *testing.T, store *quotareserve.Store) *filterapi.RuntimeConfig {
	t.Helper()
	rc, err := filterapi.NewRuntimeConfig(t.Context(), &filterapi.Config{
		LLMRequestCosts: []filterapi.LLMRequestCost{
			{
				MetadataKey: "quota_cost_rule-1", RouteName: "ns/route", Backend: "ns/backend", Model: "deepseek-0731",
				Type: filterapi.LLMRequestCostTypeCEL, CEL: testFreshInputExpr,
				AdmissionReserve: &filterapi.LLMRequestCostAdmissionReserve{
					MetadataKey: testReserveKey, EstimateByHeader: "x-client-id", Percent: 90,
					Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5,
				},
			},
			{
				MetadataKey: "quota_cost_rule-2", RouteName: "ns/route", Backend: "ns/backend", Model: "deepseek-0731",
				Type: filterapi.LLMRequestCostTypeCEL, CEL: "output_tokens",
			},
		},
	}, nil)
	require.NoError(t, err)
	rc.AdmissionReserveStore = store
	return rc
}

func reserveTestRouter(config *filterapi.RuntimeConfig, headers map[string]string) *chatCompletionProcessorRouterFilter {
	return &chatCompletionProcessorRouterFilter{
		config:         config,
		requestHeaders: headers,
		logger:         slog.Default(),
		tracer:         tracingapi.NoopTracer[openai.ChatCompletionRequest, openai.ChatCompletionResponse, openai.ChatCompletionResponseChunk]{},
	}
}

func reserveMetadata(t *testing.T, resp *extprocv3.ProcessingResponse) (float64, bool) {
	t.Helper()
	ns := resp.GetDynamicMetadata().GetFields()[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().GetFields()
	v, ok := ns[testReserveKey]
	return v.GetNumberValue(), ok
}

func TestRouterProcessor_AdmissionReserve(t *testing.T) {
	estimateKey := quotareserve.Key{Header: "x-client-id", Value: "key-1", Model: "deepseek"}

	t.Run("reserve from the key's recent responses", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		body := bodyFromModel(t, "deepseek", false, nil)
		// The key's last response: same size, 10,000 input tokens, 8,192 cached.
		store.Record(estimateKey, quotareserve.Outcome{RequestBytes: len(body), InputTokens: 10_000, CachedInputTokens: 8192}, time.Minute)

		p := reserveTestRouter(reserveRuntimeConfig(t, store), map[string]string{":path": "/v1/chat/completions", "x-client-id": "key-1"})
		resp, err := p.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: body})
		require.NoError(t, err)

		// 90% of the estimated fresh input: floor(0.9 * (10,000 - 8,192)).
		v, ok := reserveMetadata(t, resp)
		require.True(t, ok)
		require.Equal(t, float64(1627), v)
		require.Equal(t, map[string]uint64{testReserveKey: 1627}, p.admissionReserves)
	})

	t.Run("a key with no recent response reserves zero", func(t *testing.T) {
		p := reserveTestRouter(reserveRuntimeConfig(t, quotareserve.NewStore(time.Now)),
			map[string]string{":path": "/v1/chat/completions", "x-client-id": "key-1"})
		resp, err := p.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, "deepseek", false, nil)})
		require.NoError(t, err)
		v, ok := reserveMetadata(t, resp)
		require.True(t, ok, "the reserve is always written so the hits_addend resolves")
		require.Zero(t, v)
	})

	t.Run("a request without the estimate header reserves zero and records nothing", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		p := reserveTestRouter(reserveRuntimeConfig(t, store), map[string]string{":path": "/v1/chat/completions"})
		resp, err := p.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, "deepseek", false, nil)})
		require.NoError(t, err)
		v, ok := reserveMetadata(t, resp)
		require.True(t, ok)
		require.Zero(t, v)
		require.Empty(t, p.admissionKeys)
	})

	t.Run("no reserve configured, no metadata", func(t *testing.T) {
		p := reserveTestRouter(&filterapi.RuntimeConfig{AdmissionReserveStore: quotareserve.NewStore(time.Now)},
			map[string]string{":path": "/v1/chat/completions", "x-client-id": "key-1"})
		resp, err := p.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, "deepseek", false, nil)})
		require.NoError(t, err)
		require.Nil(t, resp.GetDynamicMetadata())
	})
}

func TestRouterProcessor_AdmissionOutcome(t *testing.T) {
	estimateKey := quotareserve.Key{Header: "x-client-id", Value: "key-1", Model: "deepseek"}
	params := quotareserve.Params{Window: time.Minute, MaxFailurePercent: 20, MinSamples: 1}
	admitted := func(t *testing.T, store *quotareserve.Store) *chatCompletionProcessorRouterFilter {
		p := reserveTestRouter(reserveRuntimeConfig(t, store), map[string]string{":path": "/v1/chat/completions", "x-client-id": "key-1"})
		_, err := p.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, "deepseek", false, nil)})
		require.NoError(t, err)
		return p
	}

	t.Run("a successful response is recorded once", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		p := admitted(t, store)
		p.admissionUpstreamStarted.Store(true)
		var usage metrics.TokenUsage
		usage.SetInputTokens(12_000)
		usage.SetCachedInputTokens(8192)
		p.recordAdmissionSuccess(&usage)
		p.recordAdmissionSuccess(&usage)
		p.finishAdmission()

		est, ok := store.Estimate(estimateKey, p.admissionRequestBytes, params)
		require.True(t, ok)
		require.Equal(t, quotareserve.Estimate{InputTokens: 12_000, CachedInputTokens: 8192}, est)
		// MinSamples 1 and 0% failures: one success and no failure.
		_, ok = store.Estimate(estimateKey, p.admissionRequestBytes, quotareserve.Params{Window: time.Minute, MaxFailurePercent: 0, MinSamples: 1})
		require.True(t, ok)
	})

	t.Run("an admitted request that ends without a successful response is a failure", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		p := admitted(t, store)
		p.admissionUpstreamStarted.Store(true)
		p.finishAdmission()
		_, ok := store.Estimate(estimateKey, p.admissionRequestBytes, quotareserve.Params{Window: time.Minute, MaxFailurePercent: 0, MinSamples: 1})
		require.False(t, ok)
	})

	t.Run("a response without usage is a failure", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		p := admitted(t, store)
		p.admissionUpstreamStarted.Store(true)
		p.recordAdmissionSuccess(&metrics.TokenUsage{})
		p.finishAdmission()
		_, ok := store.Estimate(estimateKey, p.admissionRequestBytes, quotareserve.Params{Window: time.Minute, MaxFailurePercent: 0, MinSamples: 1})
		require.False(t, ok)
	})

	t.Run("a request refused before any upstream records nothing", func(t *testing.T) {
		store := quotareserve.NewStore(time.Now)
		p := admitted(t, store)
		p.finishAdmission()
		// A success recorded afterwards is the window's only outcome.
		store.Record(estimateKey, quotareserve.Outcome{RequestBytes: 100, InputTokens: 100}, time.Minute)
		_, ok := store.Estimate(estimateKey, 100, quotareserve.Params{Window: time.Minute, MaxFailurePercent: 0, MinSamples: 1})
		require.True(t, ok)
	})
}

func Test_buildDynamicMetadata_admissionReserve(t *testing.T) {
	rc := reserveRuntimeConfig(t, quotareserve.NewStore(time.Now))
	headers := map[string]string{internalapi.ModelNameHeaderKeyDefault: "deepseek-0731"}
	var usage metrics.TokenUsage
	usage.SetInputTokens(10_000)
	usage.SetCachedInputTokens(8192)
	usage.SetOutputTokens(500)

	for _, tc := range []struct {
		name     string
		reserves map[string]uint64
		wantCost float64
	}{
		{name: "no reserve charged", reserves: nil, wantCost: 1808},
		{name: "the remainder after the reserve", reserves: map[string]uint64{testReserveKey: 1627}, wantCost: 181},
		{name: "a reserve above the cost is not refunded", reserves: map[string]uint64{testReserveKey: 5000}, wantCost: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, err := buildDynamicMetadata(nil, rc.RequestCosts, &usage, headers, "ns/backend/route/rule/ref", "ns/route", "", tc.reserves)
			require.NoError(t, err)
			ns := md.Fields[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().Fields
			require.Equal(t, tc.wantCost, ns["quota_cost_rule-1"].GetNumberValue())
			require.Equal(t, float64(500), ns["quota_cost_rule-2"].GetNumberValue(), "a bucket without a reserve is charged in full")
		})
	}
}

func TestUpstreamProcessor_ChargesTheRemainderAndRecordsTheResponse(t *testing.T) {
	store := quotareserve.NewStore(time.Now)
	body := bodyFromModel(t, "deepseek", false, nil)
	estimateKey := quotareserve.Key{Header: "x-client-id", Value: "key-1", Model: "deepseek"}
	store.Record(estimateKey, quotareserve.Outcome{RequestBytes: len(body), InputTokens: 10_000, CachedInputTokens: 8192}, time.Minute)

	router := reserveTestRouter(reserveRuntimeConfig(t, store), map[string]string{":path": "/v1/chat/completions", "x-client-id": "key-1"})
	_, err := router.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: body})
	require.NoError(t, err)
	require.Equal(t, uint64(1627), router.admissionReserves[testReserveKey])
	router.admissionUpstreamStarted.Store(true)

	inBody := &extprocv3.HttpBody{Body: []byte("some-body"), EndOfStream: true}
	mt := &mockTranslator{t: t, expResponseBody: inBody}
	// This response missed part of the cache: 10,000 input, 4,000 cached.
	mt.retUsedToken.SetInputTokens(10_000)
	mt.retUsedToken.SetCachedInputTokens(4000)
	mt.retUsedToken.SetOutputTokens(300)
	u := &chatCompletionProcessorUpstreamFilter{
		translator:      mt,
		metrics:         &mockMetrics{},
		parent:          router,
		requestHeaders:  map[string]string{internalapi.ModelNameHeaderKeyDefault: "deepseek-0731"},
		responseHeaders: map[string]string{":status": "200"},
		backendName:     "ns/backend/route/rule/ref",
		routeName:       "ns/route",
	}
	res, err := u.ProcessResponseBody(t.Context(), inBody)
	require.NoError(t, err)
	ns := res.GetDynamicMetadata().GetFields()[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().GetFields()
	// Fresh input 6,000 minus the 1,627 reserved at admission.
	require.Equal(t, float64(6000-1627), ns["quota_cost_rule-1"].GetNumberValue())
	require.Equal(t, float64(300), ns["quota_cost_rule-2"].GetNumberValue())

	router.finishAdmission()
	// Two successes (10,000/8,192 and 10,000/4,000), no failure recorded at stream end.
	est, ok := store.Estimate(estimateKey, len(body), quotareserve.Params{Window: time.Minute, MaxFailurePercent: 0, MinSamples: 1})
	require.True(t, ok)
	require.Equal(t, quotareserve.Estimate{InputTokens: 10_000, CachedInputTokens: (8192 + 4000) / 2}, est)
}
