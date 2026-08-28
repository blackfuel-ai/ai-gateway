// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
)

const (
	mimeTypeImageJPEG       = "image/jpeg"
	mimeTypeImagePNG        = "image/png"
	mimeTypeImageGIF        = "image/gif"
	mimeTypeImageWEBP       = "image/webp"
	mimeTypeTextPlain       = "text/plain"
	mimeTypeApplicationJSON = "application/json"
	mimeTypeApplicationEnum = "text/x.enum"
)

var (
	// sseDataPrefixSpace is the canonical form the gateway emits; reads go through
	// cutSSEDataPrefix, which also accepts the field without the space.
	sseDataPrefixSpace = []byte("data: ")
	sseDataPrefix      = []byte("data:")
	sseDoneMessage     = []byte("[DONE]")
	sseDoneFullLine    = append(append(sseDataPrefixSpace, sseDoneMessage...), '\n')
)

// cutSSEFieldPrefix strips an SSE field-name prefix such as "data:" from a line,
// accepting the optional space after the colon:
// https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation
func cutSSEFieldPrefix(line, fieldName []byte) ([]byte, bool) {
	after, ok := bytes.CutPrefix(line, fieldName)
	if !ok {
		return nil, false
	}
	// Only one leading space belongs to the framing; anything beyond it is data.
	return bytes.TrimPrefix(after, []byte{' '}), true
}

// cutSSEDataPrefix is cutSSEFieldPrefix for the "data" field.
func cutSSEDataPrefix(line []byte) ([]byte, bool) {
	return cutSSEFieldPrefix(line, sseDataPrefix)
}

// regDataURI follows the web uri regex definition.
// https://developer.mozilla.org/en-US/docs/Web/URI/Schemes/data#syntax
var regDataURI = regexp.MustCompile(`\Adata:(.+?)?(;base64)?,`)

// parseDataURI parse data uri example: data:image/jpeg;base64,/9j/4AAQSkZJRgABAgAAZABkAAD.
func parseDataURI(uri string) (string, []byte, error) {
	matches := regDataURI.FindStringSubmatch(uri)
	if len(matches) != 3 {
		return "", nil, fmt.Errorf("data uri does not have a valid format")
	}
	l := len(matches[0])
	contentType := matches[1]
	bin, err := base64.StdEncoding.DecodeString(uri[l:])
	if err != nil {
		return "", nil, err
	}
	return contentType, bin, nil
}

// forceOriginalBodyIfEmpty returns original when forceBodyMutation is set and newBody
// hasn't been populated by the caller, so that a body is always written to Envoy
// when body mutation is forced. Otherwise, newBody is returned unchanged.
func forceOriginalBodyIfEmpty(forceBodyMutation bool, newBody, original []byte) []byte {
	if forceBodyMutation && len(newBody) == 0 {
		return original
	}
	return newBody
}

// systemMsgToDeveloperMsg converts OpenAI system message to developer message.
// Since systemMsg is deprecated, this function is provided to maintain backward compatibility.
func systemMsgToDeveloperMsg(msg openai.ChatCompletionSystemMessageParam) openai.ChatCompletionDeveloperMessageParam {
	// Convert OpenAI system message to developer message.
	return openai.ChatCompletionDeveloperMessageParam{
		Name:    msg.Name,
		Role:    openai.ChatMessageRoleDeveloper,
		Content: msg.Content,
	}
}

// setOpenAIStreamUsage copies OpenAI streaming usage into the metrics TokenUsage using
// OpenAI accounting, where prompt_tokens already includes cached tokens (cached and
// reasoning are informational subsets, not added to input). Shared by the OpenAI-native
// and the anthropic→OpenAI streaming paths so usage is read identically from any chunk
// that carries it — including the final content chunk (non-empty choices + finish_reason),
// the framing OpenRouter/GLM-5.2 uses. No-op when usage is nil.
func setOpenAIStreamUsage(tu *metrics.TokenUsage, usage *openai.Usage) {
	if usage == nil {
		return
	}
	tu.SetInputTokens(uint32(usage.PromptTokens))      //nolint:gosec
	tu.SetOutputTokens(uint32(usage.CompletionTokens)) //nolint:gosec
	tu.SetTotalTokens(uint32(usage.TotalTokens))       //nolint:gosec
	if usage.PromptTokensDetails != nil {
		tu.SetCachedInputTokens(uint32(usage.PromptTokensDetails.CachedTokens))               //nolint:gosec
		tu.SetCacheCreationInputTokens(uint32(usage.PromptTokensDetails.CacheCreationTokens)) //nolint:gosec
	}
	if usage.CompletionTokensDetails != nil {
		tu.SetReasoningTokens(uint32(usage.CompletionTokensDetails.ReasoningTokens)) //nolint:gosec
	}
}

// serialize a ChatCompletionResponseChunk, this is common for all chat completion request
func serializeOpenAIChatCompletionChunk(chunk *openai.ChatCompletionResponseChunk, buf *[]byte) error {
	var chunkBytes []byte
	chunkBytes, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("failed to marshal stream chunk: %w", err)
	}
	*buf = append(*buf, sseDataPrefixSpace...)
	*buf = append(*buf, chunkBytes...)
	*buf = append(*buf, '\n', '\n')
	return nil
}

// anthropicErrorTypeForStatus maps an HTTP status code to the Anthropic error
// type name that goes in an error response's `error.type` field. It is the
// fallback used when a non-Anthropic backend fails without a structured JSON
// body to translate, so all that is left to classify the failure is the status.
// https://platform.claude.com/docs/en/api/errors#http-errors
func anthropicErrorTypeForStatus(statusCode string) string {
	switch statusCode {
	case "400":
		return "invalid_request_error"
	case "401":
		return "authentication_error"
	case "403":
		return "permission_error"
	case "404":
		return "not_found_error"
	case "413":
		return "request_too_large"
	case "429":
		return "rate_limit_error"
	case "500":
		return "internal_server_error"
	case "503":
		return "service_unavailable_error"
	case "529":
		return "overloaded_error"
	default:
		return "internal_server_error"
	}
}
