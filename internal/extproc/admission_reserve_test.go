// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
)

const (
	testReserveKey   = "quota_reserve_estimated_fresh_input_token_90"
	testFreshCost    = "quota_cost_rule-0"
	testTotalCost    = "quota_cost_default"
	testReserveRoute = "route"
)

// newAdmissionReserveTestConfig returns a configuration reserving percent of the
// usage estimate estimate, with a fresh input cost on that reserve and a total
// token cost without one, both on the route the test upstream legs use.
func newAdmissionReserveTestConfig(t *testing.T, estimate filterapi.UsageEstimate, percent uint32, reserveKey string) *filterapi.RuntimeConfig {
	cfg := newUsageEstimateTestConfig(t, estimate)
	cfg.AdmissionReserves = []filterapi.AdmissionReserve{{MetadataKey: reserveKey, UsageEstimate: estimate.MetadataKey, Percent: percent}}
	for _, c := range []filterapi.LLMRequestCost{
		{
			MetadataKey: testFreshCost, RouteName: testReserveRoute, Type: filterapi.LLMRequestCostTypeCEL,
			CEL:                         "input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)",
			AdmissionReserveMetadataKey: reserveKey,
		},
		{MetadataKey: testTotalCost, RouteName: testReserveRoute, Type: filterapi.LLMRequestCostTypeCEL, CEL: "total_tokens"},
	} {
		prog, err := llmcostcel.NewProgram(c.CEL)
		require.NoError(t, err)
		cfg.RequestCosts = append(cfg.RequestCosts, filterapi.RuntimeRequestCost{LLMRequestCost: &c, CELProg: prog})
	}
	return cfg
}

func TestAdmissionReserve_ReserveThenRemainder(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey)
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
		rp := newUsageEstimateTestRouter(newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey), ue, nil)
		resp, _ := admitAndDispatch(t, rp)
		fields := usageEstimateFields(t, resp)
		require.Len(t, fields, 1)
		require.Equal(t, 0.0, fields[testReserveKey].GetNumberValue())
	})

	t.Run("estimate CEL error", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		failing := filterapi.UsageEstimate{
			MetadataKey: "failing", CEL: "input_tokens > uint(0) ? input_tokens - uint(1000000) : uint(0)", ByHeader: usageEstimateTestHeader,
		}
		cfg := newAdmissionReserveTestConfig(t, failing, 90, "quota_reserve_failing_90")
		headers := map[string]string{usageEstimateTestHeader: "key-a"}
		rp := newUsageEstimateTestRouter(cfg, ue, headers)
		_, u := admitAndDispatch(t, rp)
		completeWithUsage(t, rp, u, 100, 0)
		clock.nextPeriod()

		rp = newUsageEstimateTestRouter(cfg, ue, headers)
		resp, _ := admitAndDispatch(t, rp)
		fields := usageEstimateFields(t, resp)
		require.Len(t, fields, 1)
		require.Equal(t, 0.0, fields["quota_reserve_failing_90"].GetNumberValue())
	})
}

// A fractional estimate is rounded to the nearest token: 1000 input tokens with a
// cache rate of 0.8 leave 199.99999999999997 fresh tokens in floating point.
func TestAdmissionReserve_RoundsToNearest(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	fresh := filterapi.UsageEstimate{
		MetadataKey: "fresh", CEL: "double(input_tokens) * (1.0 - cache_rate)", ByHeader: usageEstimateTestHeader,
	}
	cfg := newAdmissionReserveTestConfig(t, fresh, 100, "quota_reserve_fresh_100")
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
	cfg := newAdmissionReserveTestConfig(t, testEstimateFresh, 90, testReserveKey)
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

// A reserve above Envoy's hits_addend maximum is charged the maximum, and only
// that is subtracted from the cost at completion.
func TestAdmissionReserve_ClampedToHitsAddendMax(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	huge := filterapi.UsageEstimate{
		MetadataKey: "huge", CEL: "double(input_tokens) * 1e12", ByHeader: usageEstimateTestHeader,
	}
	cfg := newAdmissionReserveTestConfig(t, huge, 100, "quota_reserve_huge_100")
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
	fields = usageEstimateFields(t, completeWithUsage(t, rp, u, 1_500_000_000, 0))
	require.Equal(t, 5e8, fields[testFreshCost].GetNumberValue())
}
