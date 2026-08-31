// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"

	"k8s.io/utils/ptr"

	anthropicschema "github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai/tokenize"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"
)

// NewCountTokensToOpenAITokenizeTranslator creates a translator from Anthropic's
// /v1/messages/count_tokens to the /tokenize endpoint of an OpenAI-schema backend.
//
// /tokenize is a vLLM extension rather than part of the OpenAI API. It is the only
// way an OpenAI-schema backend can answer a token count with the model's own
// tokenizer, so a backend that does not implement it answers 404 and the client
// sees a not_found_error instead of a count. That is intentional: the alternative
// for such a backend is no count at all.
//
// The path is deliberately NOT prefixed with the backend's OpenAI prefix. vLLM
// serves /tokenize at the server root, and the extproc registers it unprefixed;
// meanwhile schemaToFilterAPI coerces an OpenAI schema's prefix to "v1", so
// honouring the prefix here would emit /v1/tokenize and 404 on every request.
// NewTokenizeTranslator ignores the prefix for the same reason.
//
// Count fidelity: vLLM renders /tokenize through the same
// online_renderer.preprocess_chat as /v1/chat/completions, with the same chat
// template, template kwargs and tool_dicts, so the count is what the equivalent
// request would really consume. Two known divergences, both on vLLM's side:
//   - Harmony models (model_type == "gpt_oss") render chat completions through
//     the harmony path, which tokenize's serving code has no branch for, so
//     counts for those models are approximate.
//   - tokenize ignores tool_choice, while the chat path drops tools when
//     tool_choice is "none" and the server runs with
//     --exclude-tools-when-tool-choice-none. That flag defaults off and we do
//     not set it, so tools are counted the same on both paths today.
func NewCountTokensToOpenAITokenizeTranslator(modelNameOverride internalapi.ModelNameOverride) AnthropicCountTokensTranslator {
	return &countTokensToOpenAITokenizeTranslator{modelNameOverride: modelNameOverride}
}

type countTokensToOpenAITokenizeTranslator struct {
	modelNameOverride internalapi.ModelNameOverride
	// requestModel is the fallback response model: the /tokenize response body
	// carries no model field, so without this metrics and tracing would record none.
	requestModel internalapi.RequestModel
}

// RequestBody implements [AnthropicCountTokensTranslator.RequestBody].
func (t *countTokensToOpenAITokenizeTranslator) RequestBody(_ []byte, body *anthropicschema.CountTokensRequest, _ bool) (
	newHeaders []internalapi.Header, newBody []byte, err error,
) {
	t.requestModel = cmp.Or(t.modelNameOverride, body.Model)

	// CountTokensRequest reuses MessagesRequest's leaf types, so the shared
	// Anthropic->OpenAI helpers apply directly once the countable fields are
	// lifted across. Only system, messages and tools contribute to the count;
	// the sampling parameters MessagesRequest carries have no bearing on it and
	// /tokenize would reject them.
	messages := anthropicMessagesToOpenAI(&anthropicschema.MessagesRequest{
		Model:    body.Model,
		Messages: body.Messages,
		System:   body.System,
	})

	req := &tokenize.ChatRequest{
		Model:    t.requestModel,
		Messages: messages,
		Tools:    anthropicToolsToOpenAI(body.Tools),
		// Count what the equivalent /v1/messages request would actually consume.
		// The chat-completions path renders the same conversation with the
		// generation prompt appended, so omitting it here would under-report by
		// the width of the model's generation header.
		AddGenerationPrompt: ptr.To(true),
	}

	newBody, err = json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal tokenize request: %w", err)
	}

	newHeaders = []internalapi.Header{
		{pathHeaderName, openAITokenizePath},
		{contentLengthHeaderName, strconv.Itoa(len(newBody))},
		// Keep the original-path headers consistent with the rewritten :path so that a
		// downstream gateway tier and the backend see the translated path everywhere.
		// Note that this gateway's own upstream filter does NOT select its processor
		// from these headers (it derives it from the router processor of the request,
		// see internal/extproc/server.go), so Envoy retrying this leg with the rewritten
		// headers stays on the Messages processor. In a two-tier deployment the second
		// gateway's router phase reads :path and must resolve the translated
		// /tokenize path, not the client's original /v1/messages/count_tokens —
		// otherwise the second tier returns "unsupported path".
		{internalapi.OriginalPathHeader, openAITokenizePath},
		{internalapi.EnvoyOriginalPathHeader, openAITokenizePath},
	}
	return
}

// ResponseHeaders implements [AnthropicCountTokensTranslator.ResponseHeaders].
func (t *countTokensToOpenAITokenizeTranslator) ResponseHeaders(_ map[string]string) (
	newHeaders []internalapi.Header, err error,
) {
	return nil, nil
}

// ResponseBody implements [AnthropicCountTokensTranslator.ResponseBody].
func (t *countTokensToOpenAITokenizeTranslator) ResponseBody(_ map[string]string, body io.Reader, _ bool, span tracingapi.CountTokensSpan) (
	newHeaders []internalapi.Header, newBody []byte, tokenUsage metrics.TokenUsage, responseModel internalapi.ResponseModel, err error,
) {
	responseModel = t.requestModel

	resp := &tokenize.Response{}
	if err = json.NewDecoder(body).Decode(resp); err != nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("failed to unmarshal body: %w", err)
	}

	anthropicResp := &anthropicschema.CountTokensResponse{InputTokens: int64(resp.Count)}
	if span != nil {
		span.RecordResponse(anthropicResp)
	}
	tokenUsage.SetInputTokens(uint32(resp.Count)) //nolint:gosec

	newBody, err = json.Marshal(anthropicResp)
	if err != nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("failed to marshal Anthropic response: %w", err)
	}
	newHeaders = []internalapi.Header{{contentLengthHeaderName, strconv.Itoa(len(newBody))}}
	return
}

// ResponseError implements [AnthropicCountTokensTranslator.ResponseError].
// The client asked in Anthropic format, so an OpenAI-schema backend's error has to
// be restated in Anthropic format on the way out.
func (t *countTokensToOpenAITokenizeTranslator) ResponseError(respHeaders map[string]string, body io.Reader) (
	newHeaders []internalapi.Header, mutatedBody []byte, errInfo LLMErrorInfo, err error,
) {
	statusCode := respHeaders[statusHeaderName]
	buf, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to read error body: %w", err)
	}

	var anthropicError anthropicschema.ErrorResponse
	if strings.Contains(respHeaders[contentTypeHeaderName], jsonContentType) {
		var openaiErr openai.Error
		if err = json.Unmarshal(buf, &openaiErr); err != nil {
			return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to unmarshal OpenAI error body: %w", err)
		}
		if openaiErr.Error.Type == "" && openaiErr.Error.Message == "" {
			// Unmarshalling any JSON object into openai.Error succeeds, so a body
			// that simply isn't OpenAI-shaped yields a zero struct rather than an
			// error. A backend without /tokenize is exactly this case: FastAPI
			// answers 404 with {"detail":"Not Found"}. Classify it from the status
			// and keep the raw body, or the client gets an empty error.
			typ := anthropicErrorTypeForStatus(statusCode)
			anthropicError = anthropicschema.ErrorResponse{
				Type:  "error",
				Error: anthropicschema.ErrorResponseMessage{Type: typ, Message: string(buf)},
			}
			errInfo = LLMErrorInfo{Type: typ}
		} else {
			anthropicError = anthropicschema.ErrorResponse{
				Type: "error",
				Error: anthropicschema.ErrorResponseMessage{
					Type:    openaiErr.Error.Type,
					Message: openaiErr.Error.Message,
				},
			}
			errInfo = extractOpenAIErrorInfo(buf)
		}
	} else {
		typ := anthropicErrorTypeForStatus(statusCode)
		anthropicError = anthropicschema.ErrorResponse{
			Type:  "error", // Always "error" at the top level.
			Error: anthropicschema.ErrorResponseMessage{Type: typ, Message: string(buf)},
		}
		errInfo = LLMErrorInfo{Type: typ}
	}

	mutatedBody, err = json.Marshal(anthropicError)
	if err != nil {
		return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to marshal error body: %w", err)
	}
	newHeaders = append(newHeaders,
		internalapi.Header{contentTypeHeaderName, jsonContentType},
		internalapi.Header{contentLengthHeaderName, strconv.Itoa(len(mutatedBody))},
	)
	return
}
