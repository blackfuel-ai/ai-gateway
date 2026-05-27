// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package bodymutator

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
)

type BodyMutator struct {
	// originalBody is the original request body for retry scenarios
	originalBody []byte

	// bodyMutations is the body mutations to apply
	bodyMutations *filterapi.HTTPBodyMutation
}

func NewBodyMutator(bodyMutations *filterapi.HTTPBodyMutation, originalBody []byte) *BodyMutator {
	return &BodyMutator{
		originalBody:  originalBody,
		bodyMutations: bodyMutations,
	}
}

// isJSONValue checks if a string represents a JSON value (not a plain string)
func isJSONValue(value string) bool {
	value = strings.TrimSpace(value)

	// Check for quoted strings (JSON strings)
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		return true
	}

	// Check for numbers (integers or floats)
	if value == "0" || value == "true" || value == "false" || value == "null" {
		return true
	}

	// Check for positive/negative numbers
	if len(value) > 0 {
		first := value[0]
		if (first >= '0' && first <= '9') || first == '-' || first == '+' {
			// Simple number check - contains only digits, dots, +, -, e, E
			isNumber := true
			for _, r := range value {
				if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != 'e' && r != 'E' {
					isNumber = false
					break
				}
			}
			if isNumber {
				return true
			}
		}
	}

	// Check for objects or arrays
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		return true
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return true
	}

	// Default to plain string
	return false
}

// Mutate mutates the request body based on the body mutations.
func (b *BodyMutator) Mutate(requestBody []byte) ([]byte, error) {
	if b.bodyMutations == nil {
		return requestBody, nil
	}

	mutatedBody := requestBody
	var err error

	// Apply removals first
	if len(b.bodyMutations.Remove) > 0 {
		for _, fieldName := range b.bodyMutations.Remove {
			if fieldName != "" {
				// TODO optimize by using SetBytesOption to avoid underlying sjson copy.
				mutatedBody, err = sjson.DeleteBytes(mutatedBody, fieldName)
				if err != nil {
					return nil, fmt.Errorf("failed to remove field %s: %w", fieldName, err)
				}
			}
		}
	}

	// Apply sets
	if len(b.bodyMutations.Set) > 0 {
		for _, field := range b.bodyMutations.Set {
			if field.Path != "" {
				mutatedBody, err = writeField(mutatedBody, field.Path, field.Value)
				if err != nil {
					return nil, fmt.Errorf("failed to set field %s: %w", field.Path, err)
				}
			}
		}
	}

	// Apply set-defaults last so they only fill paths still absent after
	// Remove and Set. An explicit Set on the same path therefore always wins.
	// gjson.Exists returns true for explicit JSON null, matching the
	// documented "explicit null suppresses the default" semantics.
	if len(b.bodyMutations.SetDefault) > 0 {
		for _, field := range b.bodyMutations.SetDefault {
			if field.Path == "" {
				continue
			}
			if gjson.GetBytes(mutatedBody, field.Path).Exists() {
				continue
			}
			mutatedBody, err = writeField(mutatedBody, field.Path, field.Value)
			if err != nil {
				return nil, fmt.Errorf("failed to set default for field %s: %w", field.Path, err)
			}
		}
	}

	return mutatedBody, nil
}

// writeField writes a value to the given path, choosing between raw JSON and
// plain-string sjson encoders based on isJSONValue.
// TODO handle JSON value check in configuration load time too.
func writeField(body []byte, path, value string) ([]byte, error) {
	if isJSONValue(value) {
		// Use SetRawBytes for JSON values (quoted strings, numbers, booleans, objects, arrays)
		return sjson.SetRawBytesOptions(body, path, []byte(value), &sjson.Options{ReplaceInPlace: true})
	}
	// Use SetBytes for plain string values
	return sjson.SetBytesOptions(body, path, value, &sjson.Options{ReplaceInPlace: true})
}
