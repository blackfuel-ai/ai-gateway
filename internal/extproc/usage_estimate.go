// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/usageestimate"
)

// maxHitsAddend is the largest hits_addend Envoy accepts. Envoy ignores a
// descriptor whose hits_addend resolves above it, so a larger reserve would be
// subtracted from the cost at completion without ever having been charged.
const maxHitsAddend = 1_000_000_000

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
	// reserves are the admission reserves charged at admission, by metadata key. The
	// quota costs on a reserve are charged the remainder at completion, and settle
	// the part the reserve covers.
	reserves map[string]uint64
}

type admittedUsageEstimate struct {
	estimate *filterapi.RuntimeUsageEstimate
	value    float64
}

// setUsageEstimates implements [usageEstimateProcessor].
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) setUsageEstimates(ue *usageEstimates) {
	r.usageEstimate.shared = ue
}

// estimateUsage computes the usage estimates of the request at admission. It returns
// them as dynamic metadata, nil when no estimate applies, and as the request header
// mutations of the items with EmitHeader: an estimate sets its header, and an item
// without an estimate removes it, so no client-sent value of the header goes
// upstream. It must be called once the original model is known. A request to an
// endpoint that consumes no model usage has no estimate.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) estimateUsage(ctx context.Context, requestBytes int, logger *slog.Logger) (metadata *structpb.Struct, setHeaders []*corev3.HeaderValueOption, removeHeaders []string) {
	st := &r.usageEstimate
	st.period = r.config.UsageEstimatePeriod
	st.requestBytes = requestBytes
	stats := make(map[usageestimate.Key]usageestimate.Stats)
	fields := make(map[string]*structpb.Value)
	for i := range r.config.UsageEstimates {
		e := &r.config.UsageEstimates[i]
		v, estimated := r.estimateOne(ctx, e, stats, logger)
		if estimated {
			fields[e.MetadataKey] = structpb.NewNumberValue(v)
		}
		if e.Header == "" {
			continue
		}
		if !estimated {
			delete(r.requestHeaders, e.Header)
			removeHeaders = append(removeHeaders, e.Header)
			continue
		}
		value := strconv.FormatFloat(v, 'f', -1, 64)
		r.requestHeaders[e.Header] = value
		setHeaders = append(setHeaders, &corev3.HeaderValueOption{
			// Overwrite so that a client-sent value is replaced, not appended to.
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			Header:       &corev3.HeaderValue{Key: e.Header, RawValue: []byte(value)},
		})
	}
	if st.shared != nil {
		r.reserveAdmission(fields)
	}
	if len(fields) > 0 {
		metadata = &structpb.Struct{Fields: map[string]*structpb.Value{
			internalapi.AIGatewayFilterMetadataNamespace: structpb.NewStructValue(&structpb.Struct{Fields: fields}),
		}}
	}
	return metadata, setHeaders, removeHeaders
}

// estimateOne computes the estimate of e for the request and reports whether the
// request has one. stats caches the statistics of the keys already read for the
// request.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) estimateOne(ctx context.Context, e *filterapi.RuntimeUsageEstimate, stats map[usageestimate.Key]usageestimate.Stats, logger *slog.Logger) (float64, bool) {
	st := &r.usageEstimate
	if st.shared == nil || !r.eh.EstimatesUsage() {
		return 0, false
	}
	value := r.requestHeaders[e.ByHeader]
	if value == "" {
		return 0, false
	}
	key := usageestimate.Key{Header: e.ByHeader, Value: value, Model: r.originalModel}
	if st.keys == nil {
		st.keys = make(map[usageestimate.Key]struct{})
	}
	st.keys[key] = struct{}{}
	s, ok := stats[key]
	if !ok {
		s = st.shared.store.Stats(key, st.period, st.requestBytes)
		stats[key] = s
	}
	outcome := metrics.UsageEstimateOutcomeCold
	var v float64
	if s.Estimated {
		var err error
		v, err = llmcostcel.EvaluateEstimateProgram(e.CELProg, llmcostcel.EstimateInputs{
			Model:              r.requestHeaders[internalapi.ModelNameHeaderKeyDefault],
			InputTokens:        s.InputTokens,
			CachedInputTokens:  s.CachedInputTokens,
			TotalTokens:        s.InputTokens,
			InputTokensPerByte: s.InputTokensPerByte,
			CacheRate:          s.CacheRate,
		})
		if err != nil {
			outcome = metrics.UsageEstimateOutcomeError
			logger.Warn("cannot evaluate usage estimate", slog.String("metadata_key", e.MetadataKey), slog.String("error", err.Error()))
		} else {
			outcome = metrics.UsageEstimateOutcomeEstimated
			st.admitted = append(st.admitted, admittedUsageEstimate{estimate: e, value: v})
		}
	}
	if e.EmitMetric {
		st.shared.metrics.RecordRequest(ctx, e.MetadataKey, r.originalModel, outcome)
	}
	return v, outcome == metrics.UsageEstimateOutcomeEstimated
}

// reserveAdmission computes every admission reserve of the configuration into
// fields and the request state. The route is not selected yet, so all of them are
// computed; each charge entry reads its own. A reserve is its percent of the usage
// estimate, rounded to the nearest integer and capped at maxHitsAddend, and 0 when
// the request has no estimate: every reserve key is written, so that its
// hits_addend always resolves. Each reserve is also written under its release key,
// the amount the release entries take back at stream end from every counter the
// reserve was charged to. A request to an endpoint that consumes no model usage
// reserves nothing and writes no reserve or release key.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) reserveAdmission(fields map[string]*structpb.Value) {
	if len(r.config.AdmissionReserves) == 0 || !r.eh.EstimatesUsage() {
		return
	}
	st := &r.usageEstimate
	st.reserves = make(map[string]uint64, len(r.config.AdmissionReserves))
	for i := range r.config.AdmissionReserves {
		res := &r.config.AdmissionReserves[i]
		var reserve uint64
		for _, a := range st.admitted {
			if a.estimate.MetadataKey == res.UsageEstimate {
				reserve = uint64(min(math.Round(a.value*float64(res.Percent)/100), maxHitsAddend))
				break
			}
		}
		st.reserves[res.MetadataKey] = reserve
		fields[res.MetadataKey] = structpb.NewNumberValue(float64(reserve))
		if res.ReleaseMetadataKey != "" {
			fields[res.ReleaseMetadataKey] = structpb.NewNumberValue(float64(reserve))
		}
	}
}

// recordUsageEstimateSuccess records the usage of a successful response, and the
// ratio of each admitted estimate to its actual value. A response without input
// usage, or to an endpoint that consumes no model usage, records nothing.
//
// The actual value is the estimate's expression evaluated on the actual usage and
// on the inputs of the estimate (the client's model, no backend, no route), so the
// ratio measures the usage estimation alone.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) recordUsageEstimateSuccess(ctx context.Context, usage *metrics.TokenUsage) {
	st := &r.usageEstimate
	if len(st.keys) == 0 || !r.eh.EstimatesUsage() {
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

	if len(st.admitted) == 0 {
		return
	}
	// The ratios of the response, defined as those of a period in [usageestimate.Store.Stats].
	var inputTokensPerByte, cacheRate float64
	if st.requestBytes > 0 {
		inputTokensPerByte = float64(input) / float64(st.requestBytes)
	}
	if input > 0 {
		cacheRate = float64(cached) / float64(input)
	}
	cacheCreation, _ := usage.CacheCreationInputTokens()
	output, _ := usage.OutputTokens()
	total, _ := usage.TotalTokens()
	reasoning, _ := usage.ReasoningTokens()
	in := llmcostcel.EstimateInputs{
		Model:                    r.requestHeaders[internalapi.ModelNameHeaderKeyDefault],
		InputTokens:              input,
		CachedInputTokens:        cached,
		CacheCreationInputTokens: cacheCreation,
		OutputTokens:             output,
		TotalTokens:              total,
		ReasoningTokens:          reasoning,
		InputTokensPerByte:       inputTokensPerByte,
		CacheRate:                cacheRate,
	}
	for _, a := range st.admitted {
		if !a.estimate.EmitMetric {
			continue
		}
		actual, err := llmcostcel.EvaluateEstimateProgram(a.estimate.CELProg, in)
		if err != nil || actual == 0 {
			continue
		}
		st.shared.metrics.RecordRatio(ctx, a.estimate.MetadataKey, r.originalModel, a.value/actual)
	}
}
