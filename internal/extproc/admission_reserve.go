// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/quotareserve"
)

// admissionReserveFinisher is implemented by router-level processors that
// record the outcome of an admitted request when its stream ends.
type admissionReserveFinisher interface {
	finishAdmission()
}

// estimateAdmissionReserves computes every admission reserve of the
// configuration for this request and returns them as dynamic metadata fields,
// one per reserve metadata key. The route is not selected yet, so each reserve
// is computed whichever route the request lands on; the quota charge filter of
// the selected route reads only its own buckets' keys.
//
// Every key is written, zero when there is no estimate, so the charge filter's
// hits_addend always resolves. It returns nil when no reserve is configured.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) estimateAdmissionReserves(requestBytes int, logger *slog.Logger) map[string]*structpb.Value {
	store := r.config.AdmissionReserveStore
	if len(r.config.AdmissionReserves) == 0 || store == nil {
		return nil
	}
	r.admissionRequestBytes = requestBytes
	r.admissionReserves = make(map[string]uint64, len(r.config.AdmissionReserves))
	fields := make(map[string]*structpb.Value, len(r.config.AdmissionReserves))
	for i := range r.config.AdmissionReserves {
		ar := &r.config.AdmissionReserves[i]
		res := ar.Reserve
		var reserve uint64
		if value := r.requestHeaders[res.EstimateByHeader]; value != "" {
			key := quotareserve.Key{Header: res.EstimateByHeader, Value: value, Model: r.originalModel}
			if r.admissionKeys == nil {
				r.admissionKeys = make(map[quotareserve.Key]time.Duration)
			}
			r.admissionKeys[key] = max(r.admissionKeys[key], res.Window)
			if est, ok := store.Estimate(key, requestBytes, quotareserve.Params{
				Window:            res.Window,
				MaxFailurePercent: res.MaxFailurePercent,
				MinSamples:        res.MinSamples,
			}); ok {
				var usage metrics.TokenUsage
				usage.SetInputTokens(est.InputTokens)
				usage.SetCachedInputTokens(est.CachedInputTokens)
				usage.SetTotalTokens(est.InputTokens)
				usage.SetOutputTokens(0)
				cost, err := evalRuntimeRequestCost(ar.Cost, &usage, r.requestHeaders, "", "")
				if err != nil {
					// A reserve is an optimisation of when the cost is charged: on an
					// evaluation error the whole cost is charged at completion.
					logger.Warn("cannot evaluate admission reserve, reserving nothing",
						slog.String("reserve", res.MetadataKey), slog.String("error", err.Error()))
				} else {
					reserve = cost * uint64(res.Percent) / 100
				}
			}
		}
		r.admissionReserves[res.MetadataKey] = reserve
		fields[res.MetadataKey] = structpb.NewNumberValue(float64(reserve))
	}
	return fields
}

// recordAdmissionSuccess records the usage of a successful response under the
// request's estimate keys. A response without input usage is not a success; the
// stream end records it as a failure.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) recordAdmissionSuccess(usage *metrics.TokenUsage) {
	if len(r.admissionKeys) == 0 {
		return
	}
	input, ok := usage.InputTokens()
	if !ok {
		return
	}
	if !r.admissionOutcomeRecorded.CompareAndSwap(false, true) {
		return
	}
	cached, _ := usage.CachedInputTokens()
	r.recordAdmissionOutcome(quotareserve.Outcome{
		RequestBytes:      r.admissionRequestBytes,
		InputTokens:       input,
		CachedInputTokens: cached,
	})
}

// finishAdmission implements [admissionReserveFinisher]. A request that reached
// an upstream but recorded no successful response failed: an error status, a
// response without usage, failed retries, or a stream aborted early. A request
// that never reached an upstream, such as one refused by the quota filter, was
// charged no reserve and records nothing.
func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) finishAdmission() {
	if len(r.admissionKeys) == 0 || !r.admissionUpstreamStarted.Load() {
		return
	}
	if !r.admissionOutcomeRecorded.CompareAndSwap(false, true) {
		return
	}
	r.recordAdmissionOutcome(quotareserve.Outcome{RequestBytes: r.admissionRequestBytes, Failed: true})
}

func (r *routerProcessor[ReqT, RespT, RespChunkT, EndpointSpecT]) recordAdmissionOutcome(o quotareserve.Outcome) {
	store := r.config.AdmissionReserveStore
	for key, retain := range r.admissionKeys {
		store.Record(key, o, retain)
	}
}
