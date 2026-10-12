// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
)

const (
	testReserveKey   = "quota_reserve_estimated_fresh_input_token_90"
	testReleaseKey   = "quota_release_estimated_fresh_input_token_90"
	testFreshCost    = "quota_cost_rule-0"
	testFreshSettle  = "quota_settle_rule-0"
	testTotalCost    = "quota_cost_default"
	testReserveRoute = "route"
)

// newAdmissionReserveTestConfig returns a configuration reserving percent of the
// usage estimate estimate, released under releaseKey, with a fresh input cost on
// that reserve and a total token cost without one, both on the route the test
// upstream legs use.
func newAdmissionReserveTestConfig(t *testing.T, estimate filterapi.UsageEstimate, percent uint32, reserveKey, releaseKey string) *filterapi.RuntimeConfig {
	cfg := newUsageEstimateTestConfig(t, estimate)
	cfg.AdmissionReserves = []filterapi.AdmissionReserve{{MetadataKey: reserveKey, ReleaseMetadataKey: releaseKey, UsageEstimate: estimate.MetadataKey, Percent: percent}}
	costs := []filterapi.LLMRequestCost{
		{
			MetadataKey: testFreshCost, RouteName: testReserveRoute, Type: filterapi.LLMRequestCostTypeCEL,
			CEL:                         "input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)",
			AdmissionReserveMetadataKey: reserveKey,
			AdmissionSettleMetadataKey:  testFreshSettle,
		},
		{MetadataKey: testTotalCost, RouteName: testReserveRoute, Type: filterapi.LLMRequestCostTypeCEL, CEL: "total_tokens"},
	}
	for i := range costs {
		prog, err := llmcostcel.NewProgram(costs[i].CEL)
		require.NoError(t, err)
		cfg.RequestCosts = append(cfg.RequestCosts, filterapi.RuntimeRequestCost{LLMRequestCost: &costs[i], CELProg: prog})
	}
	return cfg
}

func TestAdmissionReserve_ReserveThenRemainder(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	// A cold key reserves 0, and the whole cost is charged at completion.
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Equal(t, 0.0, fields[testReserveKey].GetNumberValue())
	require.NotContains(t, fields, testEstimateFresh.MetadataKey)
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 40))
	require.Equal(t, 60.0, fields[testFreshCost].GetNumberValue())
	require.Equal(t, 100.0, fields[testTotalCost].GetNumberValue())
	clock.nextPeriod()

	// 90% of the estimated 60 fresh input tokens is reserved, and only the
	// remainder of the bucket that reserves is charged at completion.
	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u = admitAndDispatch(t, rp)
	fields = usageEstimateFields(t, resp)
	require.Equal(t, 60.0, fields[testEstimateFresh.MetadataKey].GetNumberValue())
	require.Equal(t, 54.0, fields[testReserveKey].GetNumberValue())
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 40))
	require.Equal(t, 6.0, fields[testFreshCost].GetNumberValue())
	require.Equal(t, 100.0, fields[testTotalCost].GetNumberValue())

	// A reserve above the actual cost leaves nothing to charge: it is never refunded.
	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	_, u = admitAndDispatch(t, rp)
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 100))
	require.Equal(t, 0.0, fields[testFreshCost].GetNumberValue())
}

func TestAdmissionReserve_NoEstimateReservesZero(t *testing.T) {
	t.Run("no header", func(t *testing.T) {
		ue, _, _ := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey), ue, nil)
		resp, _ := admitAndDispatch(t, rp)
		fields := usageEstimateFields(t, resp)
		require.Len(t, fields, 2)
		require.Equal(t, 0.0, fields[testReserveKey].GetNumberValue())
		require.Equal(t, 0.0, fields[testReleaseKey].GetNumberValue())
	})

	t.Run("estimate CEL error", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		failing := filterapi.UsageEstimate{
			MetadataKey: "failing", CEL: "input_tokens > uint(0) ? input_tokens - uint(1000000) : uint(0)", ByHeader: usageEstimateTestHeader,
		}
		cfg := newAdmissionReserveTestConfig(t, failing, 90, "quota_reserve_failing_90", "quota_release_failing_90")
		headers := map[string]string{usageEstimateTestHeader: "key-a"}
		rp := newUsageEstimateTestRouter(cfg, ue, headers)
		_, u := admitAndDispatch(t, rp)
		completeWithUsage(t, rp, u, 100, 0)
		clock.nextPeriod()

		rp = newUsageEstimateTestRouter(cfg, ue, headers)
		resp, _ := admitAndDispatch(t, rp)
		fields := usageEstimateFields(t, resp)
		require.Len(t, fields, 2)
		require.Equal(t, 0.0, fields["quota_reserve_failing_90"].GetNumberValue())
		require.Equal(t, 0.0, fields["quota_release_failing_90"].GetNumberValue())
	})
}

// A fractional estimate is rounded to the nearest token: 1000 input tokens with a
// cache rate of 0.8 leave 199.99999999999997 fresh tokens in floating point.
func TestAdmissionReserve_RoundsToNearest(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	fresh := filterapi.UsageEstimate{
		MetadataKey: "fresh", CEL: "double(input_tokens) * (1.0 - cache_rate)", ByHeader: usageEstimateTestHeader,
	}
	cfg := newAdmissionReserveTestConfig(t, fresh, 100, "quota_reserve_fresh_100", "quota_release_fresh_100")
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 1000, 800)
	clock.nextPeriod()

	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Less(t, fields["fresh"].GetNumberValue(), 200.0)
	require.Equal(t, 200.0, fields["quota_reserve_fresh_100"].GetNumberValue())
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 1000, 800))
	require.Equal(t, 0.0, fields[testFreshCost].GetNumberValue())
}

// A retried request is reserved once at admission and charged its remainder once,
// on the leg that completes.
func TestAdmissionReserve_RetryThenSuccess(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 40)
	clock.nextPeriod()

	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	admit(t, rp, false)
	_, err := dispatch(t, setBackend(t, rp, false), &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	u = setBackend(t, rp, false)
	_, err = dispatch(t, u, &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	fields := usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 40))
	require.Equal(t, 6.0, fields[testFreshCost].GetNumberValue())
}

// A streamed response writes the quota costs on every chunk that carries usage, not
// only at end of stream. Each write derives the remainder and the settlement from the
// cumulative usage, so the reserve is subtracted once whatever the number of writes,
// and the last one wins.
func TestAdmissionReserve_StreamingRepeatedWrites(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	admitStream := func(rp *chatCompletionProcessorRouterFilter) (*extprocv3.ProcessingResponse, *chatCompletionProcessorUpstreamFilter) {
		resp := admit(t, rp, true)
		u := setBackend(t, rp, false)
		_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		return resp, u
	}
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitStream(rp)
	completeWithUsage(t, rp, u, 100, 40)
	clock.nextPeriod()

	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitStream(rp)
	require.Equal(t, 54.0, usageEstimateFields(t, resp)[testReserveKey].GetNumberValue())
	u.responseHeaders = map[string]string{":status": "200"}

	for _, c := range []struct {
		input, cached       uint32
		eos                 bool
		expFresh, expSettle float64
	}{
		{input: 50, cached: 40, expFresh: 0, expSettle: 10},
		{input: 100, cached: 40, expFresh: 6, expSettle: 54},
		{input: 100, cached: 40, eos: true, expFresh: 6, expSettle: 54},
	} {
		chunk := &extprocv3.HttpBody{Body: []byte("chunk"), EndOfStream: c.eos}
		mt := &mockTranslator{t: t, expResponseBody: chunk}
		mt.retUsedToken.SetInputTokens(c.input)
		mt.retUsedToken.SetCachedInputTokens(c.cached)
		mt.retUsedToken.SetTotalTokens(c.input)
		u.translator = mt
		resp, err := rp.ProcessResponseBody(t.Context(), chunk)
		require.NoError(t, err)
		fields := usageEstimateFields(t, resp)
		require.Equal(t, c.expFresh, fields[testFreshCost].GetNumberValue())
		require.Equal(t, c.expSettle, fields[testFreshSettle].GetNumberValue())
		require.Equal(t, float64(c.input), fields[testTotalCost].GetNumberValue())
	}
}

// A stream aborted after a chunk reported usage, before end of stream, keeps the
// metadata of that chunk: the remainder and the settlement of the usage reported so
// far. With the reserve released at stream done, the counter is charged that usage.
func TestAdmissionReserve_StreamAbortedAfterUsage(t *testing.T) {
	rp := newWarmReserveRouter(t)()
	fields := usageEstimateFields(t, admit(t, rp, true))
	reserve := fields[testReserveKey].GetNumberValue()
	require.Greater(t, reserve, 10.0, "the reserve covers the usage reported below")
	require.Equal(t, reserve, fields[testReleaseKey].GetNumberValue())
	u := setBackend(t, rp, false)
	_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	u.responseHeaders = map[string]string{":status": "200"}

	chunk := &extprocv3.HttpBody{Body: []byte("chunk")}
	mt := &mockTranslator{t: t, expResponseBody: chunk}
	mt.retUsedToken.SetInputTokens(50)
	mt.retUsedToken.SetCachedInputTokens(40)
	mt.retUsedToken.SetTotalTokens(50)
	u.translator = mt
	resp, err := rp.ProcessResponseBody(t.Context(), chunk)
	require.NoError(t, err)
	// No end of stream follows: these are the last values the charge filter reads.
	fields = usageEstimateFields(t, resp)
	require.Equal(t, 0.0, fields[testFreshCost].GetNumberValue())
	require.Equal(t, 10.0, fields[testFreshSettle].GetNumberValue())
	require.Equal(t, 50.0, fields[testTotalCost].GetNumberValue())
}

// A reserve above Envoy's hits_addend maximum is charged the maximum, and only
// that is subtracted from the cost at completion.
func TestAdmissionReserve_ClampedToHitsAddendMax(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	huge := filterapi.UsageEstimate{
		MetadataKey: "huge", CEL: "double(input_tokens) * 1e12", ByHeader: usageEstimateTestHeader,
	}
	cfg := newAdmissionReserveTestConfig(t, huge, 100, "quota_reserve_huge_100", "quota_release_huge_100")
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 2000, 0)
	clock.nextPeriod()

	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Equal(t, 2e15, fields["huge"].GetNumberValue())
	require.Equal(t, 1e9, fields["quota_reserve_huge_100"].GetNumberValue())
	require.Equal(t, 1e9, fields["quota_release_huge_100"].GetNumberValue())
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 1_500_000_000, 0))
	require.Equal(t, 5e8, fields[testFreshCost].GetNumberValue())
	require.Equal(t, 1e9, fields[testFreshSettle].GetNumberValue())
}

// newWarmReserveRouter returns a router for a request whose client has a usage
// estimate of 60 fresh input tokens, so that it reserves 54 at 90%.
func newWarmReserveRouter(t *testing.T) func() *chatCompletionProcessorRouterFilter {
	ue, _, clock := newTestUsageEstimates()
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 40)
	clock.nextPeriod()
	return func() *chatCompletionProcessorRouterFilter { return newUsageEstimateTestRouter(cfg, ue, headers) }
}

// The release is written at admission with the reserve, whatever the outcome;
// the settlement is written with the cost, whenever a successful response reports
// usage, and is the part of the usage reported so far the reserve covers.
func TestAdmissionReserve_ReleaseAndSettle(t *testing.T) {
	for _, tc := range []struct {
		name               string
		input, cached      uint32
		expCost, expSettle float64
	}{
		{name: "reserve below the cost", input: 100, cached: 0, expCost: 46, expSettle: 54},
		{name: "reserve above the cost", input: 100, cached: 70, expCost: 0, expSettle: 30},
		{name: "fully cached response", input: 100, cached: 100, expCost: 0, expSettle: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := newWarmReserveRouter(t)()
			resp, u := admitAndDispatch(t, rp)
			fields := usageEstimateFields(t, resp)
			require.Equal(t, 54.0, fields[testReserveKey].GetNumberValue())
			require.Equal(t, 54.0, fields[testReleaseKey].GetNumberValue())
			require.NotContains(t, fields, testFreshSettle)

			fields = usageEstimateFields(t, completeWithUsage(t, rp, u, tc.input, tc.cached))
			require.Equal(t, tc.expCost, fields[testFreshCost].GetNumberValue())
			require.Equal(t, tc.expSettle, fields[testFreshSettle].GetNumberValue())
			// The cost and the settlement sum to the actual cost.
			require.Equal(t, float64(tc.input-tc.cached), tc.expCost+tc.expSettle)
			require.NotContains(t, fields, testReleaseKey)
			require.NotContains(t, fields, "quota_settle_default", "the total cost has no reserve to settle")
		})
	}

	t.Run("upstream error settles nothing", func(t *testing.T) {
		rp := newWarmReserveRouter(t)()
		_, u := admitAndDispatch(t, rp)
		inBody := &extprocv3.HttpBody{Body: []byte("error"), EndOfStream: true}
		u.translator = &mockTranslator{t: t, expResponseBody: inBody}
		u.responseHeaders = map[string]string{":status": "503"}
		resp, err := rp.ProcessResponseBody(t.Context(), inBody)
		require.NoError(t, err)
		require.Nil(t, resp.DynamicMetadata)
	})

	t.Run("retry then success settles once, on the completing leg", func(t *testing.T) {
		rp := newWarmReserveRouter(t)()
		admit(t, rp, false)
		_, err := dispatch(t, setBackend(t, rp, false), &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		u := setBackend(t, rp, false)
		_, err = dispatch(t, u, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		fields := usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 40))
		require.Equal(t, 6.0, fields[testFreshCost].GetNumberValue())
		require.Equal(t, 54.0, fields[testFreshSettle].GetNumberValue())
	})

	t.Run("mirror leg settles nothing", func(t *testing.T) {
		rp := newWarmReserveRouter(t)()
		admitAndDispatch(t, rp)
		m := setBackend(t, rp, true)
		_, err := dispatch(t, m, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		inBody := &extprocv3.HttpBody{Body: []byte("some-body"), EndOfStream: true}
		mt := &mockTranslator{t: t, expResponseBody: inBody}
		mt.retUsedToken.SetInputTokens(100)
		m.translator = mt
		m.responseHeaders = map[string]string{":status": "200"}
		resp, err := m.ProcessResponseBody(t.Context(), inBody)
		require.NoError(t, err)
		require.Nil(t, resp.DynamicMetadata)
	})

	t.Run("no estimate releases and settles 0", func(t *testing.T) {
		ue, _, _ := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey, testReleaseKey), ue, nil)
		resp, u := admitAndDispatch(t, rp)
		fields := usageEstimateFields(t, resp)
		require.Equal(t, 0.0, fields[testReleaseKey].GetNumberValue())
		fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 100, 40))
		require.Equal(t, 60.0, fields[testFreshCost].GetNumberValue())
		require.Equal(t, 0.0, fields[testFreshSettle].GetNumberValue())
	})
}
