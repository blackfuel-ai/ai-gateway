// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"strconv"
	"strings"

	"github.com/envoyproxy/ai-gateway/internal/json"
)

// extractOpenAIErrorInfo best-effort extracts the error classification from an
// OpenAI-style error body of the form {"error": {"type": ..., "code": ...}}.
//
// It never returns an error: unparseable input yields the zero LLMErrorInfo so
// that passthrough error paths can call it without risking a translation
// failure. The "code" field is tolerated as a JSON string, number, or null,
// since OpenAI-compatible backends are inconsistent about its type.
func extractOpenAIErrorInfo(buf []byte) LLMErrorInfo {
	var parsed struct {
		Error struct {
			Type string          `json:"type"`
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf, &parsed); err != nil {
		return LLMErrorInfo{}
	}
	return LLMErrorInfo{
		Type: parsed.Error.Type,
		Code: normalizeJSONCode(parsed.Error.Code),
	}
}

// normalizeJSONCode renders a JSON "code" value (string, number, or null) as a
// string. It returns "" for null, absent, or unparseable values.
func normalizeJSONCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Numeric (or other scalar) code: use the raw JSON literal.
	lit := strings.TrimSpace(string(raw))
	if _, err := strconv.ParseFloat(lit, 64); err == nil {
		return lit
	}
	return ""
}
