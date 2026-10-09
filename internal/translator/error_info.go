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
// OpenAI-style error body of the form
// {"error": {"type": ..., "code": ..., "param": ...}}.
//
// It never returns an error: unparseable input yields the zero LLMErrorInfo so
// that passthrough error paths can call it without risking a translation
// failure. The "code" field is tolerated as a JSON string, number, or null,
// since OpenAI-compatible backends are inconsistent about its type. The "param"
// field is kept only when it is a JSON string; any other value leaves Param
// empty without affecting Type or Code.
func extractOpenAIErrorInfo(buf []byte) LLMErrorInfo {
	var parsed struct {
		Error struct {
			Type  string          `json:"type"`
			Code  json.RawMessage `json:"code"`
			Param json.RawMessage `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf, &parsed); err != nil {
		return LLMErrorInfo{}
	}
	return LLMErrorInfo{
		Type:  parsed.Error.Type,
		Code:  normalizeJSONCode(parsed.Error.Code),
		Param: jsonStringValue(parsed.Error.Param),
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

// jsonStringValue returns the value of a JSON string. It returns "" for an
// absent field and for any non-string JSON value (null, number, boolean,
// object, or array).
func jsonStringValue(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
