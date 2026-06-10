// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
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
