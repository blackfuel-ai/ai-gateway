// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/internalapi"
)

// upstreamReasoningEffortMembers are the top-level request body members that carry
// a reasoning effort level, in precedence order, one per backend schema:
// reasoning_effort on chat completions, reasoning.effort on responses and
// output_config.effort on Anthropic messages. effortPath locates the level inside
// the member, "" being the member itself.
var upstreamReasoningEffortMembers = [...]struct{ name, effortPath string }{
	{name: "reasoning_effort"},
	{name: "reasoning", effortPath: "effort"},
	{name: "output_config", effortPath: "effort"},
}

// invalidReasoningEffort stands for an effort member the access log must not carry
// as sent: a value of another JSON type, or a string that is not a short token.
const invalidReasoningEffort = "invalid"

// maxReasoningEffortLength bounds a logged effort value.
const maxReasoningEffortLength = 32

// upstreamReasoningEffort reads the reasoning effort carried by the request body
// the gateway sends the backend, after translation and route body mutation. That
// body says which level the backend was asked for whatever the client's schema:
// an Anthropic client's output_config.effort or thinking budget arrives at an
// OpenAI-schema backend as reasoning_effort.
//
// It returns the first member of upstreamReasoningEffortMembers the body sets, ""
// when it sets none (null included), and invalidReasoningEffort for a value that
// is not a string of 1 to 32 characters of [a-z0-9_-], so the access log never
// carries free text from a request body.
//
// The body runs to megabytes on a long conversation, so it is walked once, member
// by member, over a string view that shares its bytes: gjson.ParseBytes would copy
// it and a lookup per path would walk it once per path.
func upstreamReasoningEffort(body []byte) string {
	// The view lives only for this call, which neither mutates the body nor lets a
	// result escape uncloned; gjson.GetBytes reads bytes the same way.
	view := unsafe.String(unsafe.SliceData(body), len(body)) //nolint:gosec // G103: read-only view, see above.
	var found [len(upstreamReasoningEffortMembers)]gjson.Result
	gjson.Parse(view).ForEach(func(key, value gjson.Result) bool {
		for i, m := range upstreamReasoningEffortMembers {
			if key.Str != m.name || found[i].Exists() {
				continue
			}
			if m.effortPath != "" {
				value = value.Get(m.effortPath)
			}
			found[i] = value
		}
		return true
	})
	for _, r := range found {
		switch r.Type {
		case gjson.Null:
			continue
		case gjson.String:
			if isReasoningEffortToken(r.Str) {
				// r.Str shares the body's bytes; the metadata outlives this call.
				return strings.Clone(r.Str)
			}
			return invalidReasoningEffort
		default:
			return invalidReasoningEffort
		}
	}
	return ""
}

func isReasoningEffortToken(s string) bool {
	if s == "" || len(s) > maxReasoningEffortLength {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// buildUpstreamReasoningEffortDynamicMetadata emits the reasoning effort sent to
// the backend as upstream_reasoning_effort, for the access log. A request whose
// body carries no effort emits nothing.
func buildUpstreamReasoningEffortDynamicMetadata(effort string) *structpb.Struct {
	if effort == "" {
		return nil
	}
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			internalapi.AIGatewayFilterMetadataNamespace: {
				Kind: &structpb.Value_StructValue{
					StructValue: &structpb.Struct{
						Fields: map[string]*structpb.Value{
							"upstream_reasoning_effort": structpb.NewStringValue(effort),
						},
					},
				},
			},
		},
	}
}
