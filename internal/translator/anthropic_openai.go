// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"

	"github.com/tidwall/sjson"

	"github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"
)

// NewAnthropicToChatCompletionOpenAITranslator implements [Factory] for Anthropic to OpenAI ChatCompletion translation.
// This translator converts Anthropic API format to OpenAI ChatCompletion API requests.
// The prefix parameter is the prefix field set in the OpenAI VersionAPISchema used to construct the translated path
// (e.g., "v1" produces "/v1/chat/completions", "gateway/v1" produces "/gateway/v1/chat/completions").
func NewAnthropicToChatCompletionOpenAITranslator(prefix string, modelNameOverride internalapi.ModelNameOverride) AnthropicMessagesTranslator {
	passthroughTranslator := NewAnthropicToAnthropicTranslator(prefix, modelNameOverride)
	return &anthropicToOpenAIV1ChatCompletionTranslator{
		passthroughTranslator: &passthroughTranslator,
		modelNameOverride:     modelNameOverride,
		path:                  path.Join("/", prefix, "chat/completions"),
	}
}

type anthropicToOpenAIV1ChatCompletionTranslator struct {
	passthroughTranslator *AnthropicMessagesTranslator
	modelNameOverride     internalapi.ModelNameOverride
	requestModel          internalapi.RequestModel
	stream                bool
	streamState           *openAIStreamToAnthropicState
	// The path of the chat completions endpoint, prefixed with the OpenAI path prefix.
	path string
	// Redaction configuration for debug logging
	debugLogEnabled bool
	enableRedaction bool
	logger          *slog.Logger
}

// RequestBody implements [AnthropicMessagesTranslator.RequestBody].
func (a *anthropicToOpenAIV1ChatCompletionTranslator) RequestBody(_ []byte, body *anthropic.MessagesRequest, _ bool) (
	newHeaders []internalapi.Header, newBody []byte, err error,
) {
	// Set translator config based on Anthropic message request
	a.stream = body.Stream
	// Store the request model to use as fallback for response model
	a.requestModel = cmp.Or(a.modelNameOverride, body.Model)

	// Convert Anthropic message request body to OpenAI format.
	openAIReq := buildOpenAIChatCompletionRequest(body, a.modelNameOverride)

	newBody, err = json.Marshal(openAIReq)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal OpenAI request: %w", err)
	}

	// Add stop sequences via sjson because ChatCompletionNewParamsStopUnion (from the external openai-go SDK)
	// requires importing the external package. Using sjson avoids that dependency.
	if len(body.StopSequences) > 0 {
		newBody, err = sjson.SetBytesOptions(newBody, "stop", body.StopSequences, sjsonOptions)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to set stop sequences: %w", err)
		}
	}

	if body.Stream {
		a.streamState = &openAIStreamToAnthropicState{
			activeTools:  make(map[int64]*streamToolCall),
			requestModel: a.requestModel,
		}
	}

	// Only :path is rewritten. The x-ai-eg-original-path / x-envoy-original-path
	// headers set by the router phase deliberately keep the client's original path:
	// nothing selects a processor from them (the upstream filter derives its
	// processor from the router processor of the request, see
	// internal/extproc/server.go), and access logs read x-envoy-original-path to
	// report what the client actually requested. A two-tier deployment's second
	// gateway resolves its processor from :path and overwrites the original-path
	// headers with its own values.
	newHeaders = []internalapi.Header{
		{pathHeaderName, a.path},
		{contentLengthHeaderName, strconv.Itoa(len(newBody))},
	}
	return
}

// ResponseHeaders implements [AnthropicMessagesTranslator.ResponseHeaders].
func (a *anthropicToOpenAIV1ChatCompletionTranslator) ResponseHeaders(_ map[string]string) (
	newHeaders []internalapi.Header, err error,
) {
	return nil, nil
}

// ResponseBody implements [AnthropicMessagesTranslator.ResponseBody].
func (a *anthropicToOpenAIV1ChatCompletionTranslator) ResponseBody(_ map[string]string, body io.Reader, endOfStream bool, span tracingapi.MessageSpan) (
	newHeaders []internalapi.Header, newBody []byte, tokenUsage metrics.TokenUsage, responseModel string, err error,
) {
	if a.stream {
		return a.responseBodyStreaming(body, endOfStream)
	}
	return a.responseBodyNonStreaming(body, span)
}

// responseBodyNonStreaming converts an OpenAI ChatCompletionResponse to Anthropic MessagesResponse format.
func (a *anthropicToOpenAIV1ChatCompletionTranslator) responseBodyNonStreaming(body io.Reader, span tracingapi.MessageSpan) (
	newHeaders []internalapi.Header, newBody []byte, tokenUsage metrics.TokenUsage, responseModel string, err error,
) {
	responseModel = a.requestModel

	openAIResp := &openai.ChatCompletionResponse{}
	if err = json.NewDecoder(body).Decode(openAIResp); err != nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("failed to unmarshal OpenAI response: %w", err)
	}

	responseModel = cmp.Or(openAIResp.Model, a.requestModel)

	// The cache arguments stay nil: an OpenAI upstream already counts cached
	// and cache-creation tokens inside prompt_tokens, and the helper adds what
	// it is given to the input total, so passing them here would count them
	// twice.
	tokenUsage = metrics.ExtractTokenUsageFromExplicitCaching(
		int64(openAIResp.Usage.PromptTokens),
		int64(openAIResp.Usage.CompletionTokens),
		nil,
		nil,
	)
	// The cache split is still reported, as the streaming path does through
	// setOpenAIStreamUsage. Without this a non-streaming /v1/messages response
	// carries no cached-token count at all, and every consumer of the usage
	// record reads a fully uncached prompt.
	if details := openAIResp.Usage.PromptTokensDetails; details != nil {
		tokenUsage.SetCachedInputTokens(uint32(details.CachedTokens))               //nolint:gosec
		tokenUsage.SetCacheCreationInputTokens(uint32(details.CacheCreationTokens)) //nolint:gosec
	}
	// Reasoning tokens are part of the same split, counted inside
	// completion_tokens by the upstream and reported alongside it by every
	// other non-streaming translator.
	if details := openAIResp.Usage.CompletionTokensDetails; details != nil {
		tokenUsage.SetReasoningTokens(uint32(details.ReasoningTokens)) //nolint:gosec
	}

	anthropicResp := openAIResponseToAnthropic(openAIResp, responseModel)

	// Redact and log response when enabled
	if a.debugLogEnabled && a.enableRedaction && a.logger != nil {
		redactedResp := a.RedactAnthropicBody(anthropicResp)
		if jsonBody, marshalErr := json.Marshal(redactedResp); marshalErr == nil {
			a.logger.Debug("response body processing", slog.Any("response", string(jsonBody)))
		}
	}

	if span != nil {
		span.RecordResponse(anthropicResp)
	}

	newBody, err = json.Marshal(anthropicResp)
	if err != nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("failed to marshal Anthropic response: %w", err)
	}
	newHeaders = []internalapi.Header{{contentLengthHeaderName, strconv.Itoa(len(newBody))}}
	return
}

// responseBodyStreaming handles converting OpenAI SSE chunks to Anthropic SSE events.
func (a *anthropicToOpenAIV1ChatCompletionTranslator) responseBodyStreaming(body io.Reader, endOfStream bool) (
	newHeaders []internalapi.Header, newBody []byte, tokenUsage metrics.TokenUsage, responseModel string, err error,
) {
	responseModel = a.requestModel

	if a.streamState == nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("stream state not initialized")
	}

	// Read body into streamState's buffer
	if _, err = a.streamState.buffer.ReadFrom(body); err != nil {
		return nil, nil, tokenUsage, responseModel, fmt.Errorf("failed to read stream body: %w", err)
	}

	// Initialize out as a non-nil empty slice so that if no Anthropic events are emitted
	// (e.g., for finish_reason-only chunks or [DONE]), we still return a non-nil newBody.
	// A non-nil empty body tells Envoy to replace the chunk with nothing, suppressing the
	// raw upstream bytes instead of passing them through unchanged.
	out := make([]byte, 0)
	pingWorthy, err := a.streamState.processBuffer(&out, endOfStream)
	if err != nil {
		return nil, nil, tokenUsage, responseModel, err
	}

	// Never turn upstream keepalive traffic into total downstream silence. When this
	// call consumed only keepalive comment blocks (e.g. OpenRouter's ": OPENROUTER
	// PROCESSING") or undecodable data, emit an Anthropic ping event instead of an
	// empty body. Streams may include any number of ping events, and every rolling
	// idle timer on the path (Cloudflare proxy read timeout, client stream-idle
	// timers) resets only on response bytes; suppressing keepalives into silence is
	// what wedges long-prefill /v1/messages requests (BLA-2721).
	if pingWorthy && len(out) == 0 {
		if err = emitPing(&out); err != nil {
			return nil, nil, tokenUsage, responseModel, err
		}
	}

	// Update responseModel if updated in streamState or take requested model
	responseModel = cmp.Or(a.streamState.model, a.requestModel)
	tokenUsage = a.streamState.tokenUsage

	// Always return newBody (even if empty) to suppress the original upstream chunk.
	newBody = out
	return
}

// ResponseError implements [AnthropicMessagesTranslator] for Anthropic to OpenAI translation.
func (a *anthropicToOpenAIV1ChatCompletionTranslator) ResponseError(respHeaders map[string]string, r io.Reader) (
	newHeaders []internalapi.Header,
	mutatedBody []byte,
	errInfo LLMErrorInfo,
	err error,
) {
	statusCode := respHeaders[statusHeaderName]
	var anthropicError anthropic.ErrorResponse

	if strings.Contains(respHeaders[contentTypeHeaderName], jsonContentType) {
		// OpenAI backend returned a structured JSON error; translate to Anthropic error format.
		// Read the raw body so we can tolerate "code" being a string or number.
		var buf []byte
		buf, err = io.ReadAll(r)
		if err != nil {
			return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to read error body: %w", err)
		}
		var openaiErr openai.Error
		if err = json.Unmarshal(buf, &openaiErr); err != nil {
			return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to unmarshal OpenAI error body: %w", err)
		}
		anthropicError = anthropic.ErrorResponse{
			Type: "error",
			Error: anthropic.ErrorResponseMessage{
				Type:    openaiErr.Error.Type,
				Message: openaiErr.Error.Message,
			},
		}
		errInfo = extractOpenAIErrorInfo(buf)
	} else {
		var buf []byte
		buf, err = io.ReadAll(r)
		if err != nil {
			return nil, nil, LLMErrorInfo{}, fmt.Errorf("failed to read error body: %w", err)
		}
		typ := anthropicErrorTypeForStatus(statusCode)
		anthropicError = anthropic.ErrorResponse{
			Type:  "error", // Always "error" at the top level.
			Error: anthropic.ErrorResponseMessage{Type: typ, Message: string(buf)},
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

// SetRedactionConfig implements [AnthropicResponseRedactor.SetRedactionConfig].
func (a *anthropicToOpenAIV1ChatCompletionTranslator) SetRedactionConfig(debugLogEnabled, enableRedaction bool, logger *slog.Logger) {
	a.debugLogEnabled = debugLogEnabled
	a.enableRedaction = enableRedaction
	a.logger = logger
}

// RedactAnthropicBody implements [AnthropicResponseRedactor.RedactAnthropicBody].
// Creates a redacted copy of the Anthropic response for safe logging without modifying the original.
func (a *anthropicToOpenAIV1ChatCompletionTranslator) RedactAnthropicBody(resp *anthropic.MessagesResponse) *anthropic.MessagesResponse {
	if resp == nil {
		return nil
	}

	// Create a shallow copy of the response
	redacted := *resp

	// Redact content blocks (contains AI-generated content)
	if len(resp.Content) > 0 {
		redacted.Content = make([]anthropic.MessagesContentBlock, len(resp.Content))
		for i := range resp.Content {
			redacted.Content[i] = redactAnthropicContent(&resp.Content[i])
		}
	}

	return &redacted
}
