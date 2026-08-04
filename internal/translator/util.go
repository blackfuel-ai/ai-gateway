// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
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
	sseDataPrefix = []byte("data: ")
	// sseDataPrefixNoSpace matches the compact "data:" form (the space after the
	// colon is optional per the SSE spec); used as a fallback when parsing events.
	sseDataPrefixNoSpace = []byte("data:")
	sseDoneMessage       = []byte("[DONE]")
	sseDoneFullLine      = append(append(sseDataPrefix, sseDoneMessage...), '\n')
)

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

// streamedDeltaTokens reports how many output tokens the deltas in one OpenAI
// streaming chunk account for, under the one-delta-one-token approximation:
// OpenAI-compatible backends (vLLM, and the providers OpenRouter normalizes)
// emit one SSE chunk per generated token, so counting content-bearing deltas
// approximates the output token count to within a chunk.
//
// This is the fallback output count for streams severed before the authoritative
// usage frame arrives — without it such a stream reports no usage at all and
// bills zero despite having delivered output. The usage frame always wins when
// it does arrive.
//
// Role-only and finish_reason-only deltas carry no generated text and are not
// counted. A tool-call delta counts once per choice however many fragments it
// carries, matching the one-chunk-one-token shape.
func streamedDeltaTokens(chunk *openai.ChatCompletionResponseChunk) uint32 {
	var n uint32
	for i := range chunk.Choices {
		if deltaCarriesOutput(chunk.Choices[i].Delta) {
			n++
		}
	}
	return n
}

// deltaCarriesOutput reports whether a streaming delta carries model-generated
// output: text, reasoning in either the reasoning_content (vLLM/DeepSeek) or the
// plain reasoning (OpenRouter) framing, or a tool-call fragment.
func deltaCarriesOutput(delta *openai.ChatCompletionResponseChunkChoiceDelta) bool {
	if delta == nil {
		return false
	}
	if delta.Content != nil && *delta.Content != "" {
		return true
	}
	if delta.Reasoning != nil && *delta.Reasoning != "" {
		return true
	}
	if delta.ReasoningContent != nil && delta.ReasoningContent.Text != "" {
		return true
	}
	for i := range delta.ToolCalls {
		fn := delta.ToolCalls[i].Function
		if fn.Name != "" || fn.Arguments != "" {
			return true
		}
	}
	return false
}

// serialize a ChatCompletionResponseChunk, this is common for all chat completion request
func serializeOpenAIChatCompletionChunk(chunk *openai.ChatCompletionResponseChunk, buf *[]byte) error {
	var chunkBytes []byte
	chunkBytes, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("failed to marshal stream chunk: %w", err)
	}
	*buf = append(*buf, sseDataPrefix...)
	*buf = append(*buf, chunkBytes...)
	*buf = append(*buf, '\n', '\n')
	return nil
}
