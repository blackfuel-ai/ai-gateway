// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/usageestimate"
)

// usageEstimates is the per-process state of the usage estimates. It outlives the
// runtime configuration, which is rebuilt on every configuration change.
type usageEstimates struct {
	store   *usageestimate.Store
	metrics metrics.UsageEstimateMetrics
}

// usageEstimateProcessor is implemented by router-level processors that estimate
// the usage of a request when it is admitted.
type usageEstimateProcessor interface {
	// setUsageEstimates hands the processor the per-process usage estimate state.
	setUsageEstimates(*usageEstimates)
}

// usageEstimateState is the usage estimate state of one request. Its fields are
// written at admission and read at completion, both on the router stream: the
// router processor delegates the response phases to its upstream processor.
type usageEstimateState struct {
	shared *usageEstimates
	// period is the length of the periods the request is estimated from and its
	// usage is recorded in.
	period time.Duration
	// requestBytes is the size of the request body the client sent.
	requestBytes int
	// keys are the keys the usage of the request is recorded under.
	keys map[usageestimate.Key]struct{}
	// admitted are the estimates computed at admission, for the ratio metric.
	admitted []admittedUsageEstimate
}

type admittedUsageEstimate struct {
	estimate *filterapi.RuntimeUsageEstimate
	value    uint64
}

// setUsageEstimates implements [usageEstimateProcessor].
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) setUsageEstimates(ue *usageEstimates) {
	r.usageEstimate.shared = ue
}

// estimateUsage computes the usage estimates of the request at admission and
// returns them as dynamic metadata, or nil when no estimate applies. It must be
// called once the original model is known.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) estimateUsage(ctx context.Context, requestBytes int, logger *slog.Logger) *structpb.Struct {
	st := &r.usageEstimate
	if st.shared == nil || len(r.config.UsageEstimates) == 0 {
		return nil
	}
	st.period = r.config.UsageEstimatePeriod
	st.requestBytes = requestBytes
	stats := make(map[usageestimate.Key]usageestimate.Stats)
	fields := make(map[string]*structpb.Value)
	for i := range r.config.UsageEstimates {
		e := &r.config.UsageEstimates[i]
		value := r.requestHeaders[e.ByHeader]
		if value == "" {
			continue
		}
		key := usageestimate.Key{Header: e.ByHeader, Value: value, Model: r.originalModel}
		if st.keys == nil {
			st.keys = make(map[usageestimate.Key]struct{})
		}
		st.keys[key] = struct{}{}
		s, ok := stats[key]
		if !ok {
			s = st.shared.store.Stats(key, st.period, requestBytes)
			stats[key] = s
		}
		outcome := metrics.UsageEstimateOutcomeCold
		if s.Estimated {
			fields[e.MetadataKey+filterapi.UsageEstimateInputTokensPerByteSuffix] = structpb.NewNumberValue(s.InputTokensPerByte)
			fields[e.MetadataKey+filterapi.UsageEstimateCacheRateSuffix] = structpb.NewNumberValue(s.CacheRate)
			var usage metrics.TokenUsage
			usage.SetInputTokens(s.InputTokens)
			usage.SetCachedInputTokens(s.CachedInputTokens)
			usage.SetTotalTokens(s.InputTokens)
			v, err := evalCost(filterapi.LLMRequestCostTypeCEL, e.CELProg, &usage, r.requestHeaders, "", "")
			if err != nil {
				outcome = metrics.UsageEstimateOutcomeError
				logger.Warn("cannot evaluate usage estimate", slog.String("metadata_key", e.MetadataKey), slog.String("error", err.Error()))
			} else {
				outcome = metrics.UsageEstimateOutcomeEstimated
				fields[e.MetadataKey] = structpb.NewNumberValue(float64(v))
				st.admitted = append(st.admitted, admittedUsageEstimate{estimate: e, value: v})
			}
		}
		if e.EmitMetric {
			st.shared.metrics.RecordRequest(ctx, e.MetadataKey, r.originalModel, outcome)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return &structpb.Struct{Fields: map[string]*structpb.Value{
		internalapi.AIGatewayFilterMetadataNamespace: structpb.NewStructValue(&structpb.Struct{Fields: fields}),
	}}
}

// recordUsageEstimateSuccess records the usage of a successful response, and the
// ratio of each admitted estimate to its actual value. A response without input
// usage records nothing.
//
// The actual value is the estimate's expression evaluated on the actual usage and
// on the inputs of the estimate (the client's model, no backend, no route), so the
// ratio measures the usage estimation alone.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) recordUsageEstimateSuccess(ctx context.Context, usage *metrics.TokenUsage) {
	st := &r.usageEstimate
	if len(st.keys) == 0 {
		return
	}
	input, ok := usage.InputTokens()
	if !ok {
		return
	}
	cached, _ := usage.CachedInputTokens()
	o := usageestimate.Outcome{RequestBytes: st.requestBytes, InputTokens: input, CachedInputTokens: cached}
	for key := range st.keys {
		st.shared.store.Record(key, st.period, o)
	}

	for _, a := range st.admitted {
		if !a.estimate.EmitMetric {
			continue
		}
		actual, err := evalCost(filterapi.LLMRequestCostTypeCEL, a.estimate.CELProg, usage, r.requestHeaders, "", "")
		if err != nil || actual == 0 {
			continue
		}
		st.shared.metrics.RecordRatio(ctx, a.estimate.MetadataKey, r.originalModel, float64(a.value)/float64(actual))
	}
}

