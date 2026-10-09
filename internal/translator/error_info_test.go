// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIErrorInfo(t *testing.T) {
	tests := []struct {
		name string
		body string
		want LLMErrorInfo
	}{
		{
			name: "string code",
			body: `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "context_length_exceeded"},
		},
		{
			name: "numeric code",
			body: `{"error":{"type":"invalid_request_error","code":400}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "400"},
		},
		{
			name: "float code preserves literal",
			body: `{"error":{"type":"server_error","code":500.0}}`,
			want: LLMErrorInfo{Type: "server_error", Code: "500.0"},
		},
		{
			name: "null code",
			body: `{"error":{"type":"server_error","code":null}}`,
			want: LLMErrorInfo{Type: "server_error"},
		},
		{
			name: "absent code",
			body: `{"error":{"type":"server_error"}}`,
			want: LLMErrorInfo{Type: "server_error"},
		},
		{
			name: "string param",
			body: `{"error":{"message":"bad value","type":"invalid_request_error","param":"body.messages.0.content","code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value", Param: "body.messages.0.content"},
		},
		{
			name: "null param",
			body: `{"error":{"type":"invalid_request_error","param":null,"code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value"},
		},
		{
			name: "empty string param",
			body: `{"error":{"type":"invalid_request_error","param":"","code":"400"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "400"},
		},
		{
			name: "numeric param is dropped and keeps type and code",
			body: `{"error":{"type":"invalid_request_error","param":3,"code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value"},
		},
		{
			name: "object param is dropped and keeps type and code",
			body: `{"error":{"type":"invalid_request_error","param":{"field":"messages"},"code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value"},
		},
		{
			name: "array param is dropped and keeps type and code",
			body: `{"error":{"type":"invalid_request_error","param":["messages"],"code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value"},
		},
		{
			name: "boolean param is dropped and keeps type and code",
			body: `{"error":{"type":"invalid_request_error","param":true,"code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value"},
		},
		// Extraction returns any string param verbatim; buildErrorDynamicMetadata in
		// internal/extproc applies the length and character rule before recording it.
		{
			name: "long param is returned verbatim",
			body: `{"error":{"type":"invalid_request_error","param":"` + strings.Repeat("a", 129) + `","code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value", Param: strings.Repeat("a", 129)},
		},
		{
			name: "param outside the field-name characters is returned verbatim",
			body: `{"error":{"type":"invalid_request_error","param":"messages 0\ncontent","code":"invalid_value"}}`,
			want: LLMErrorInfo{Type: "invalid_request_error", Code: "invalid_value", Param: "messages 0\ncontent"},
		},
		{
			name: "unparseable yields zero value",
			body: `not json at all`,
			want: LLMErrorInfo{},
		},
		{
			name: "empty body yields zero value",
			body: ``,
			want: LLMErrorInfo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractOpenAIErrorInfo([]byte(tt.body)))
		})
	}
}
