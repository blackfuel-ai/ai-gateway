// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package json // nolint: revive

import (
	"testing"

	sonicjson "github.com/bytedance/sonic" // nolint: depguard
)

var (
	config = sonicjson.Config{
		CaseSensitive: true,
		// SortMapKeys makes map[string]any encoding byte-stable. Without it Go's
		// randomised map iteration order leaks into upstream request bodies: every
		// replayed tool_use block is re-serialised with its keys in a different order
		// on every request, so the rendered prompt diverges at the first tool call and
		// the backend's prefix cache can only reuse the static preamble.
		//
		// Do not reach for sonicjson.ConfigStd instead. It also sets EscapeHTML, which
		// would change the bytes of any content containing <, > or &, and
		// CompactMarshaler, which would re-compact RawMessage payloads and destroy the
		// byte-faithful tool-schema passthrough this package relies on.
		SortMapKeys: true,
	}.Froze()

	// Unmarshal is equivalent to encoding/json.Unmarshal.
	Unmarshal = config.Unmarshal
	// Marshal is equivalent to encoding/json.Marshal.
	Marshal = config.Marshal
	// NewEncoder is equivalent to encoding/json.NewEncoder.
	NewEncoder = config.NewEncoder
	// NewDecoder is equivalent to encoding/json.NewDecoder.
	NewDecoder = config.NewDecoder
	// MarshalForDeterministicTesting marshals a value to JSON with encoding/json's exact
	// semantics, for tests that pin a wire shape byte-for-byte. Marshal above is already
	// deterministic in map key order; this differs from it in escaping and compaction.
	// It panics if called outside of tests.
	MarshalForDeterministicTesting = func(v interface{}) ([]byte, error) {
		if !testing.Testing() {
			panic("MarshalForDeterministicTesting can only be called from tests")
		}
		return sonicjson.ConfigStd.Marshal(v)
	}
)

type (
	// RawMessage is equivalent to encoding/json.RawMessage.
	RawMessage = sonicjson.NoCopyRawMessage
	// Marshaler is the function signature of encoding/json.Marshal.
	Marshaler = func(interface{}) ([]byte, error)
)
