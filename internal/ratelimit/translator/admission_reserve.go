// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"k8s.io/utils/ptr"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
)

// QuotaBucketCostExpression resolves the CEL expression a token bucket is
// charged by: the bucket's own expression, else the model-level expression,
// else "total_tokens".
func QuotaBucketCostExpression(quota *aigv1a1.QuotaDefinition, v *aigv1a1.QuotaValue) string {
	if v.CostExpression != nil {
		return *v.CostExpression
	}
	if quota.CostExpression != nil {
		return *quota.CostExpression
	}
	return "total_tokens"
}

// QuotaAdmissionReserveSpec is a bucket's admission reserve with the CRD
// defaults of its optional fields applied, together with the cost expression
// the reserve is estimated by.
type QuotaAdmissionReserveSpec struct {
	CostExpression    string
	EstimateByHeader  string
	Percent           uint32
	Window            time.Duration
	MaxFailurePercent uint32
	MinSamples        uint32
}

// ResolveQuotaAdmissionReserve resolves the admission reserve of bucket value v
// in a model quota. v.AdmissionReserve must be set.
func ResolveQuotaAdmissionReserve(quota *aigv1a1.QuotaDefinition, v *aigv1a1.QuotaValue) (QuotaAdmissionReserveSpec, error) {
	r := v.AdmissionReserve
	window := cmp.Or(ptr.Deref(r.Window, ""), aigv1a1.DefaultQuotaAdmissionReserveWindow)
	windowDuration, err := time.ParseDuration(string(window))
	if err != nil || windowDuration <= 0 {
		return QuotaAdmissionReserveSpec{}, fmt.Errorf("admission reserve window %q is not a positive duration", window)
	}
	return QuotaAdmissionReserveSpec{
		CostExpression:    QuotaBucketCostExpression(quota, v),
		EstimateByHeader:  r.EstimateByHeader,
		Percent:           r.Percent,
		Window:            windowDuration,
		MaxFailurePercent: ptr.Deref(r.MaxFailurePercent, aigv1a1.DefaultQuotaAdmissionReserveMaxFailurePercent),
		MinSamples:        ptr.Deref(r.MinSamples, aigv1a1.DefaultQuotaAdmissionReserveMinSamples),
	}, nil
}

// MetadataKey derives the dynamic metadata key under which the router-level
// ext_proc stores the reserve of one bucket.
//
// The router-level ext_proc computes reserves before Envoy has selected the
// route, so it cannot tell which route's buckets apply to the request: it
// writes one reserve per distinct key in its configuration. The key therefore
// carries a digest of everything the reserve value depends on, so buckets that
// share a bucket key across routes but differ in expression or settings never
// read each other's reserve.
func (s QuotaAdmissionReserveSpec) MetadataKey(bucketKey string) string {
	h := fnv.New64a()
	for _, part := range []string{
		s.CostExpression,
		s.EstimateByHeader,
		strconv.FormatUint(uint64(s.Percent), 10),
		s.Window.String(),
		strconv.FormatUint(uint64(s.MaxFailurePercent), 10),
		strconv.FormatUint(uint64(s.MinSamples), 10),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("quota_reserve_%s_%016x", bucketKey, h.Sum64())
}
