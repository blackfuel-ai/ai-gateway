// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

// TestParseDataURI tests the parseDataURI function with various inputs.
func TestParseDataURI(t *testing.T) {
	tests := []struct {
		name          string
		uri           string
		wantType      string
		wantData      []byte
		expectErr     bool
		expectedError string
	}{
		{
			name:      "Valid JPEG Data URI",
			uri:       "data:image/jpeg;base64,dGVzdF9kYXRh", // "test_data" in base64.
			wantType:  "image/jpeg",
			wantData:  []byte("test_data"),
			expectErr: false,
		},
		{
			name:      "Valid PNG Data URI",
			uri:       "data:image/png;base64,dGVzdF9wbmc=", // "test_png" in base64.
			wantType:  "image/png",
			wantData:  []byte("test_png"),
			expectErr: false,
		},
		{
			name:          "Invalid URI Format",
			uri:           "not-a-data-uri",
			expectErr:     true,
			expectedError: "data uri does not have a valid format",
		},
		{
			name:          "Malformed Base64",
			uri:           "data:image/jpeg;base64,invalid-base64-string",
			expectErr:     true,
			expectedError: "illegal base64 data at input byte 7",
		},
		{
			name:      "Data URI without base64 encoding specified",
			uri:       "data:text/plain,SGVsbG8sIFdvcmxkIQ==",
			wantType:  "text/plain",
			wantData:  []byte("Hello, World!"),
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contentType, data, err := parseDataURI(tc.uri)

			if tc.expectErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedError)
				require.Nil(t, data)
				require.Empty(t, contentType)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantType, contentType)
				require.Equal(t, tc.wantData, data)
			}
		})
	}
}

// TestSystemMsgToDeveloperMsg tests the systemMsgToDeveloperMsg function.
func TestSystemMsgToDeveloperMsg(t *testing.T) {
	systemMsg := openai.ChatCompletionSystemMessageParam{
		Name:    "test-system",
		Content: openai.ContentUnion{Value: "You are a helpful assistant."},
	}
	developerMsg := systemMsgToDeveloperMsg(systemMsg)
	require.Equal(t, "test-system", developerMsg.Name)
	require.Equal(t, openai.ChatMessageRoleDeveloper, developerMsg.Role)
	require.Equal(t, openai.ContentUnion{Value: "You are a helpful assistant."}, developerMsg.Content)
}

// TestStreamedDeltaTokens covers the one-delta-one-token count that stands in for
// output usage on a stream severed before its usage frame.
func TestStreamedDeltaTokens(t *testing.T) {
	ptr := func(s string) *string { return &s }
	chunk := func(deltas ...*openai.ChatCompletionResponseChunkChoiceDelta) *openai.ChatCompletionResponseChunk {
		c := &openai.ChatCompletionResponseChunk{}
		for i, d := range deltas {
			c.Choices = append(c.Choices, openai.ChatCompletionResponseChunkChoice{Index: int64(i), Delta: d})
		}
		return c
	}

	tests := []struct {
		name  string
		chunk *openai.ChatCompletionResponseChunk
		want  uint32
	}{
		{
			name:  "no choices (the usage-only frame)",
			chunk: chunk(),
			want:  0,
		},
		{
			name:  "absent delta",
			chunk: chunk(nil),
			want:  0,
		},
		{
			name:  "role-only opening delta carries no output",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{Role: "assistant", Content: ptr("")}),
			want:  0,
		},
		{
			name:  "finish_reason-only closing delta carries no output",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{}),
			want:  0,
		},
		{
			name:  "text content",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{Content: ptr(" test")}),
			want:  1,
		},
		{
			name:  "reasoning_content, the vLLM/DeepSeek framing",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{ReasoningContent: &openai.StreamReasoningContent{Text: "hmm"}}),
			want:  1,
		},
		{
			name:  "reasoning, the OpenRouter framing",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{Reasoning: ptr("hmm")}),
			want:  1,
		},
		{
			name: "empty reasoning is not output",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{
				Reasoning:        ptr(""),
				ReasoningContent: &openai.StreamReasoningContent{},
			}),
			want: 0,
		},
		{
			name: "a tool-call delta counts once however many fragments it carries",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
					{Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: "get_weather"}},
					{Function: openai.ChatCompletionMessageToolCallFunctionParam{Arguments: `{"city":`}},
				},
			}),
			want: 1,
		},
		{
			name: "an index-only tool-call delta carries no output",
			chunk: chunk(&openai.ChatCompletionResponseChunkChoiceDelta{
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0}},
			}),
			want: 0,
		},
		{
			name: "every content-bearing choice counts",
			chunk: chunk(
				&openai.ChatCompletionResponseChunkChoiceDelta{Content: ptr("a")},
				&openai.ChatCompletionResponseChunkChoiceDelta{Content: ptr("b")},
				&openai.ChatCompletionResponseChunkChoiceDelta{Role: "assistant"},
			),
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, streamedDeltaTokens(tt.chunk))
		})
	}
}
