// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"log/slog"
	"strings"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	anthropicschema "github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/endpointspec"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"
)

func Test_upstreamReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		exp  string
	}{
		{name: "chat completions", body: `{"reasoning_effort":"high"}`, exp: "high"},
		{name: "responses", body: `{"reasoning":{"effort":"low"}}`, exp: "low"},
		{name: "anthropic messages", body: `{"output_config":{"effort":"max"}}`, exp: "max"},
		{name: "none is a level", body: `{"reasoning_effort":"none"}`, exp: "none"},
		{name: "absent", body: `{"model":"m"}`, exp: ""},
		{name: "null is absent", body: `{"reasoning_effort":null,"output_config":{"effort":"low"}}`, exp: "low"},
		{name: "thinking alone carries no level", body: `{"thinking":{"type":"enabled","budget_tokens":31999}}`, exp: ""},
		{name: "first member wins", body: `{"output_config":{"effort":"low"},"reasoning_effort":"high"}`, exp: "high"},
		{name: "not a token", body: `{"reasoning_effort":"High Effort"}`, exp: invalidReasoningEffort},
		{name: "too long", body: `{"reasoning_effort":"` + strings.Repeat("a", maxReasoningEffortLength+1) + `"}`, exp: invalidReasoningEffort},
		{name: "longest token", body: `{"reasoning_effort":"` + strings.Repeat("a", maxReasoningEffortLength) + `"}`, exp: strings.Repeat("a", maxReasoningEffortLength)},
		{name: "empty string", body: `{"reasoning_effort":""}`, exp: invalidReasoningEffort},
		{name: "not a string", body: `{"reasoning_effort":3}`, exp: invalidReasoningEffort},
		{name: "empty body", body: ``, exp: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.exp, upstreamReasoningEffort([]byte(tc.body)))
		})
	}
}

func Test_buildUpstreamReasoningEffortDynamicMetadata(t *testing.T) {
	require.Nil(t, buildUpstreamReasoningEffortDynamicMetadata(""))
	md := buildUpstreamReasoningEffortDynamicMetadata("high")
	require.Equal(t, "high", md.Fields[internalapi.AIGatewayFilterMetadataNamespace].
		GetStructValue().Fields["upstream_reasoning_effort"].GetStringValue())
}

// aiGatewayMetadataFields returns the io.envoy.ai_gateway fields of a response.
func aiGatewayMetadataFields(resp *extprocv3.ProcessingResponse) map[string]*structpb.Value {
	return resp.DynamicMetadata.GetFields()[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().GetFields()
}

// Test_messagesProcessor_UpstreamReasoningEffortDynamicMetadata pins that an
// Anthropic client's effort is logged as the level the backend receives, which on
// an OpenAI-schema backend is the translated reasoning_effort.
func Test_messagesProcessor_UpstreamReasoningEffortDynamicMetadata(t *testing.T) {
	send := func(t *testing.T, schema filterapi.APISchemaName, bodyMutation *filterapi.HTTPBodyMutation, rawBody string) map[string]*structpb.Value {
		t.Helper()
		headers := map[string]string{
			":path":                               "/v1/messages",
			internalapi.ModelNameHeaderKeyDefault: "glm",
		}
		r := &messagesProcessorRouterFilter{
			eh:             endpointspec.MessagesEndpointSpec{},
			config:         &filterapi.RuntimeConfig{},
			requestHeaders: headers,
			logger:         slog.Default(),
			tracer: tracingapi.NoopTracer[anthropicschema.MessagesRequest,
				anthropicschema.MessagesResponse, anthropicschema.MessagesStreamChunk]{},
		}
		_, err := r.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: []byte(rawBody)})
		require.NoError(t, err)

		p := &messagesProcessorUpstreamFilter{requestHeaders: headers, metrics: &mockMetrics{}, logger: slog.Default()}
		require.NoError(t, p.SetBackend(t.Context(), &filterapi.RuntimeBackend{
			Backend: &filterapi.Backend{
				Name:         "backend",
				Schema:       filterapi.VersionedAPISchema{Name: schema},
				BodyMutation: bodyMutation,
			},
		}, "test-route", r))

		resp, err := p.ProcessRequestHeaders(t.Context(), nil)
		require.NoError(t, err)
		return aiGatewayMetadataFields(resp)
	}
	request := func(thinking string) string {
		return `{"model":"glm","max_tokens":10,"messages":[{"role":"user","content":"hi"}]` + thinking + `}`
	}

	t.Run("adaptive output_config effort to an OpenAI backend", func(t *testing.T) {
		fields := send(t, filterapi.APISchemaOpenAI, nil,
			request(`,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`))
		require.Equal(t, "max", fields["upstream_reasoning_effort"].GetStringValue())
		require.Equal(t, "backend", fields["backend_name"].GetStringValue())
	})

	t.Run("thinking budget to an OpenAI backend", func(t *testing.T) {
		fields := send(t, filterapi.APISchemaOpenAI, nil,
			request(`,"thinking":{"type":"enabled","budget_tokens":31999}`))
		require.Equal(t, "high", fields["upstream_reasoning_effort"].GetStringValue())
	})

	t.Run("output_config effort to an Anthropic backend", func(t *testing.T) {
		fields := send(t, filterapi.APISchemaAnthropic, nil,
			request(`,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}`))
		require.Equal(t, "low", fields["upstream_reasoning_effort"].GetStringValue())
	})

	t.Run("route default fills an effort the request does not set", func(t *testing.T) {
		fields := send(t, filterapi.APISchemaOpenAI,
			&filterapi.HTTPBodyMutation{SetDefault: []filterapi.HTTPBodyField{{Path: "reasoning_effort", Value: `"none"`}}},
			request(""))
		require.Equal(t, "none", fields["upstream_reasoning_effort"].GetStringValue())
	})

	t.Run("no effort", func(t *testing.T) {
		fields := send(t, filterapi.APISchemaOpenAI, nil, request(""))
		require.NotContains(t, fields, "upstream_reasoning_effort")
		require.Equal(t, "backend", fields["backend_name"].GetStringValue())
	})
}

// Test_chatCompletionProcessor_UpstreamReasoningEffortDynamicMetadata pins the
// passthrough case, where the body is sent as it arrived, and the route body
// mutation case, where the mutated value is the one logged.
func Test_chatCompletionProcessor_UpstreamReasoningEffortDynamicMetadata(t *testing.T) {
	send := func(t *testing.T, bodyMutation *filterapi.HTTPBodyMutation, rawBody string) map[string]*structpb.Value {
		t.Helper()
		headers := map[string]string{":path": "/v1/chat/completions", internalapi.ModelNameHeaderKeyDefault: "m"}
		r := &chatCompletionProcessorRouterFilter{
			config:         &filterapi.RuntimeConfig{},
			requestHeaders: headers,
			logger:         slog.Default(),
			tracer: tracingapi.NoopTracer[openai.ChatCompletionRequest,
				openai.ChatCompletionResponse, openai.ChatCompletionResponseChunk]{},
		}
		_, err := r.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: []byte(rawBody)})
		require.NoError(t, err)

		p := &chatCompletionProcessorUpstreamFilter{requestHeaders: headers, metrics: &mockMetrics{}, logger: slog.Default()}
		require.NoError(t, p.SetBackend(t.Context(), &filterapi.RuntimeBackend{
			Backend: &filterapi.Backend{
				Name:         "backend",
				Schema:       filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI},
				BodyMutation: bodyMutation,
			},
		}, "test-route", r))

		resp, err := p.ProcessRequestHeaders(t.Context(), nil)
		require.NoError(t, err)
		return aiGatewayMetadataFields(resp)
	}
	const request = `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`

	t.Run("passthrough", func(t *testing.T) {
		fields := send(t, nil, request)
		require.Equal(t, "low", fields["upstream_reasoning_effort"].GetStringValue())
	})

	t.Run("route set overrides the request", func(t *testing.T) {
		fields := send(t,
			&filterapi.HTTPBodyMutation{Set: []filterapi.HTTPBodyField{{Path: "reasoning_effort", Value: `"high"`}}},
			request)
		require.Equal(t, "high", fields["upstream_reasoning_effort"].GetStringValue())
	})

	t.Run("free text is not logged", func(t *testing.T) {
		fields := send(t, nil, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"Think Hard"}`)
		require.Equal(t, invalidReasoningEffort, fields["upstream_reasoning_effort"].GetStringValue())
	})
}
