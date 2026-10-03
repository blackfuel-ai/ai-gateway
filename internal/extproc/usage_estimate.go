// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"log/slog"
	"sync/atomic"

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
	// finishUsageEstimates is called when the router stream of the request ends.
	finishUsageEstimates()
}

// usageEstimateState is the usage estimate state of one request.
type usageEstimateState struct {
	shared *usageEstimates
	// requestBytes is the size of the request body the client sent.
	requestBytes int
	// keys are the keys the outcome of the request is recorded under.
	keys map[usageestimate.Key]struct{}
	// admitted are the estimates computed at admission, for the ratio metric.
	admitted []admittedUsageEstimate
	// upstreamStarted is set when a primary upstream leg sends the request
	// upstream, not when the gateway answers or fails it itself. It is set from
	// the upstream filter stream, hence atomic.
	upstreamStarted atomic.Bool
	// outcomeRecorded guards against recording the outcome of the request twice.
	outcomeRecorded atomic.Bool
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
			s = st.shared.store.Stats(key, requestBytes)
			stats[key] = s
		}
		fields[e.MetadataKey+"_samples"] = structpb.NewNumberValue(float64(s.Samples))
		fields[e.MetadataKey+"_failures"] = structpb.NewNumberValue(float64(s.Failures))

		outcome := metrics.UsageEstimateOutcomeCold
		if s.Estimated {
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
// usage is not a success: the end of the router stream records it as a failure.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) recordUsageEstimateSuccess(ctx context.Context, usage *metrics.TokenUsage, requestHeaders map[string]string, backendName, routeName string) {
	st := &r.usageEstimate
	if len(st.keys) == 0 {
		return
	}
	input, ok := usage.InputTokens()
	if !ok || !st.outcomeRecorded.CompareAndSwap(false, true) {
		return
	}
	cached, _ := usage.CachedInputTokens()
	st.record(usageestimate.Outcome{RequestBytes: st.requestBytes, InputTokens: input, CachedInputTokens: cached})

	for _, a := range st.admitted {
		if !a.estimate.EmitMetric {
			continue
		}
		actual, err := evalCost(filterapi.LLMRequestCostTypeCEL, a.estimate.CELProg, usage, requestHeaders, backendName, routeName)
		if err != nil || actual == 0 {
			continue
		}
		st.shared.metrics.RecordRatio(ctx, a.estimate.MetadataKey, r.originalModel, float64(a.value)/float64(actual))
	}
}

// finishUsageEstimates implements [usageEstimateProcessor]. A request that reached
// an upstream but recorded no successful response failed: an error status, a
// response without usage, failed retries, or a stream aborted early. A request
// that never reached an upstream records nothing, including one the gateway
// answered or failed itself (missing upstream credential, upstream auth error,
// request rejected by the translator).
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) finishUsageEstimates() {
	st := &r.usageEstimate
	if len(st.keys) == 0 || !st.upstreamStarted.Load() || !st.outcomeRecorded.CompareAndSwap(false, true) {
		return
	}
	st.record(usageestimate.Outcome{RequestBytes: st.requestBytes, Failed: true})
}

func (st *usageEstimateState) record(o usageestimate.Outcome) {
	for key := range st.keys {
		st.shared.store.Record(key, o)
	}
}
