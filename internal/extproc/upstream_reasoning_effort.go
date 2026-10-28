// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/internalapi"
)

// upstreamReasoningEffortPaths are the request body members that carry a reasoning
// effort level, one per backend schema: reasoning_effort on chat completions,
// reasoning.effort on responses and output_config.effort on Anthropic messages.
var upstreamReasoningEffortPaths = []string{"reasoning_effort", "reasoning.effort", "output_config.effort"}

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
// It returns the first member of upstreamReasoningEffortPaths the body sets, ""
// when it sets none (null included), and invalidReasoningEffort for a value that
// is not a string of 1 to 32 characters of [a-z0-9_-], so the access log never
// carries free text from a request body.
func upstreamReasoningEffort(body []byte) string {
	for _, r := range gjson.GetManyBytes(body, upstreamReasoningEffortPaths...) {
		switch r.Type {
		case gjson.Null:
			continue
		case gjson.String:
			if isReasoningEffortToken(r.Str) {
				return r.Str
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
