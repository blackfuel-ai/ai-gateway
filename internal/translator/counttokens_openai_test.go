// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	anthropicschema "github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai/tokenize"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

// headerValue returns the value of the named header, and whether it was present.
func headerValue(headers []internalapi.Header, key string) (string, bool) {
	for _, h := range headers {
		if h.Key() == key {
			return h.Value(), true
		}
	}
	return "", false
}

func TestCountTokensToOpenAITokenize_RequestBody(t *testing.T) {
	for _, tc := range []struct {
		name              string
		original          string
		modelNameOverride string
		expModel          string
		expMessages       []string
		expToolNames      []string
	}{
		{
			name:        "messages only",
			original:    `{"model":"deepseek-ai/DeepSeek-V4-Flash-0731","messages":[{"role":"user","content":"hello"}]}`,
			expModel:    "deepseek-ai/DeepSeek-V4-Flash-0731",
			expMessages: []string{"hello"},
		},
		{
			name:        "system prompt is prepended as a system message",
			original:    `{"model":"m","system":"be brief","messages":[{"role":"user","content":"hello"}]}`,
			expModel:    "m",
			expMessages: []string{"be brief", "hello"},
		},
		{
			name: "tools are carried across so they are counted",
			original: `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}]}`,
			expModel:     "m",
			expMessages:  []string{"hi"},
			expToolNames: []string{"Bash"},
		},
		{
			name:              "model override",
			original:          `{"model":"m","messages":[{"role":"user","content":"hello"}]}`,
			modelNameOverride: "override-model",
			expModel:          "override-model",
			expMessages:       []string{"hello"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body anthropicschema.CountTokensRequest
			require.NoError(t, json.Unmarshal([]byte(tc.original), &body))

			translator := NewCountTokensToOpenAITokenizeTranslator(tc.modelNameOverride)
			require.NotNil(t, translator)

			headers, newBody, err := translator.RequestBody([]byte(tc.original), &body, false)
			require.NoError(t, err)
			require.NotNil(t, newBody)

			// Never prefixed: vLLM serves /tokenize at the root and the extproc
			// registers it unprefixed, while an OpenAI schema's prefix is coerced
			// to "v1". /v1/tokenize would 404 on every request.
			got, ok := headerValue(headers, pathHeaderName)
			require.Truef(t, ok, "missing header %s", pathHeaderName)
			assert.Equal(t, "/tokenize", got)
			// The original-path headers are left to the router phase: they keep the
			// client's original path and are never used for processor selection.
			for _, key := range []string{internalapi.OriginalPathHeader, internalapi.EnvoyOriginalPathHeader} {
				_, found := headerValue(headers, key)
				require.Falsef(t, found, "unexpected header %s", key)
			}

			var req tokenize.ChatRequest
			require.NoError(t, json.Unmarshal(newBody, &req))
			assert.Equal(t, tc.expModel, req.Model)

			// The generation prompt is part of what the equivalent /v1/messages
			// request would consume, so it must be counted.
			require.NotNil(t, req.AddGenerationPrompt)
			assert.True(t, *req.AddGenerationPrompt)

			require.Len(t, req.Messages, len(tc.expMessages))
			for i, want := range tc.expMessages {
				assert.Contains(t, string(mustMarshal(t, req.Messages[i])), want)
			}

			require.Len(t, req.Tools, len(tc.expToolNames))
			for i, want := range tc.expToolNames {
				require.NotNil(t, req.Tools[i].Function)
				assert.Equal(t, want, req.Tools[i].Function.Name)
			}

			// content-length must match the body actually emitted.
			cl, ok := headerValue(headers, contentLengthHeaderName)
			require.True(t, ok)
			assert.Equal(t, len(newBody), mustAtoi(t, cl))
		})
	}
}

func TestCountTokensToOpenAITokenize_ResponseBody(t *testing.T) {
	translator := NewCountTokensToOpenAITokenizeTranslator("")

	// Prime requestModel so the response model falls back to it: /tokenize
	// responses carry no model field of their own.
	var body anthropicschema.CountTokensRequest
	original := `{"model":"deepseek-ai/DeepSeek-V4-Flash-0731","messages":[{"role":"user","content":"hello"}]}`
	require.NoError(t, json.Unmarshal([]byte(original), &body))
	_, _, err := translator.RequestBody([]byte(original), &body, false)
	require.NoError(t, err)

	headers, newBody, tokenUsage, responseModel, err := translator.ResponseBody(
		nil,
		strings.NewReader(`{"count":1234,"max_model_len":163840,"tokens":[1,2,3]}`),
		true,
		nil,
	)
	require.NoError(t, err)

	// vLLM's `count` becomes Anthropic's `input_tokens`.
	var resp anthropicschema.CountTokensResponse
	require.NoError(t, json.Unmarshal(newBody, &resp))
	assert.Equal(t, int64(1234), resp.InputTokens)
	// max_model_len and tokens must not leak into the Anthropic response.
	assert.NotContains(t, string(newBody), "max_model_len")
	assert.NotContains(t, string(newBody), "tokens\":[")

	gotTokens, ok := tokenUsage.InputTokens()
	require.True(t, ok)
	assert.Equal(t, uint32(1234), gotTokens)
	assert.Equal(t, internalapi.ResponseModel("deepseek-ai/DeepSeek-V4-Flash-0731"), responseModel)

	cl, ok := headerValue(headers, contentLengthHeaderName)
	require.True(t, ok)
	assert.Equal(t, len(newBody), mustAtoi(t, cl))
}

func TestCountTokensToOpenAITokenize_ResponseBody_Malformed(t *testing.T) {
	translator := NewCountTokensToOpenAITokenizeTranslator("")
	_, _, _, _, err := translator.ResponseBody(nil, strings.NewReader(`not json`), true, nil)
	require.ErrorContains(t, err, "failed to unmarshal body")
}

func TestCountTokensToOpenAITokenize_ResponseError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		respHeaders map[string]string
		body        string
		expType     string
		expMessage  string
	}{
		{
			name: "structured openai error is restated in anthropic format",
			respHeaders: map[string]string{
				statusHeaderName:      "400",
				contentTypeHeaderName: jsonContentType,
			},
			body:       `{"error":{"type":"invalid_request_error","message":"bad tokenize request","code":"400"}}`,
			expType:    "invalid_request_error",
			expMessage: "bad tokenize request",
		},
		{
			// FastAPI's own 404 body. It is JSON but not OpenAI-shaped, so
			// unmarshalling into openai.Error succeeds with a zero struct —
			// the client must still get a classified, non-empty error.
			name: "json body that is not openai-shaped is classified from the status",
			respHeaders: map[string]string{
				statusHeaderName:      "404",
				contentTypeHeaderName: jsonContentType,
			},
			body:       `{"detail":"Not Found"}`,
			expType:    "not_found_error",
			expMessage: `{"detail":"Not Found"}`,
		},
		{
			name: "a backend without /tokenize answers 404, not a 500",
			respHeaders: map[string]string{
				statusHeaderName:      "404",
				contentTypeHeaderName: "text/plain",
			},
			body:       "Not Found",
			expType:    "not_found_error",
			expMessage: "Not Found",
		},
		{
			name: "unmapped status falls back to internal_server_error",
			respHeaders: map[string]string{
				statusHeaderName:      "418",
				contentTypeHeaderName: "text/plain",
			},
			body:       "teapot",
			expType:    "internal_server_error",
			expMessage: "teapot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			translator := NewCountTokensToOpenAITokenizeTranslator("")
			headers, mutatedBody, errInfo, err := translator.ResponseError(tc.respHeaders, strings.NewReader(tc.body))
			require.NoError(t, err)

			var got anthropicschema.ErrorResponse
			require.NoError(t, json.Unmarshal(mutatedBody, &got))
			assert.Equal(t, "error", got.Type)
			assert.Equal(t, tc.expType, got.Error.Type)
			assert.Equal(t, tc.expMessage, got.Error.Message)
			assert.Equal(t, tc.expType, errInfo.Type)

			ct, ok := headerValue(headers, contentTypeHeaderName)
			require.True(t, ok)
			assert.Equal(t, jsonContentType, ct) //nolint:testifylint
			cl, ok := headerValue(headers, contentLengthHeaderName)
			require.True(t, ok)
			assert.Equal(t, len(mutatedBody), mustAtoi(t, cl))
		})
	}
}

func TestAnthropicErrorTypeForStatus(t *testing.T) {
	for status, want := range map[string]string{
		"400": "invalid_request_error",
		"401": "authentication_error",
		"403": "permission_error",
		"404": "not_found_error",
		"413": "request_too_large",
		"429": "rate_limit_error",
		"500": "internal_server_error",
		"503": "service_unavailable_error",
		"529": "overloaded_error",
		"":    "internal_server_error",
		"418": "internal_server_error",
	} {
		assert.Equal(t, want, anthropicErrorTypeForStatus(status), "status %q", status)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}
