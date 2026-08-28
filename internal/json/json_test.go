// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package json // nolint: revive

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMarshal_MapKeyOrderIsStable guards the SortMapKeys setting on the shared
// config. Without it, Go's randomised map iteration order reaches upstream
// request bodies and destroys backend prefix caching, which cost ~15x the
// necessary prefill on agentic Anthropic traffic before it was found.
//
// The loop is load-bearing: map order is randomised per range, so a single
// comparison can pass by luck. Over many iterations on a multi-key map,
// detection is effectively certain.
func TestMarshal_MapKeyOrderIsStable(t *testing.T) {
	in := map[string]any{
		"file_path": "/tmp/a.txt",
		"content":   "hello",
		"offset":    1,
		"limit":     2,
		"replace":   true,
	}

	want := `{"content":"hello","file_path":"/tmp/a.txt","limit":2,"offset":1,"replace":true}`
	for i := 0; i < 200; i++ {
		got, err := Marshal(in)
		require.NoError(t, err)
		require.Equal(t, want, string(got), "map key order must not vary between marshals (iteration %d)", i)
	}
}

// TestMarshal_NestedMapKeyOrderIsStable covers maps reached through a struct
// field, which is how tool arguments and JSON schemas actually travel.
func TestMarshal_NestedMapKeyOrderIsStable(t *testing.T) {
	type wrapper struct {
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	in := wrapper{
		Name: "Write",
		Input: map[string]any{
			"zeta": 1, "alpha": 2, "mu": 3, "beta": 4, "omega": 5,
		},
	}

	want := `{"name":"Write","input":{"alpha":2,"beta":4,"mu":3,"omega":5,"zeta":1}}`
	for i := 0; i < 200; i++ {
		got, err := Marshal(in)
		require.NoError(t, err)
		require.Equal(t, want, string(got), "nested map key order must not vary (iteration %d)", i)
	}
}

// TestMarshal_RawMessagePassesThroughVerbatim pins the property that makes tool
// schemas byte-stable end to end: RawMessage bytes are spliced as-is, with no
// re-ordering, re-compaction or escaping. Switching the config to ConfigStd
// would break this.
func TestMarshal_RawMessagePassesThroughVerbatim(t *testing.T) {
	// Deliberately non-canonical: unsorted keys and interior whitespace.
	const schema = `{"required":["b"],"type":"object","properties":{"b": {"type":"string"},"a":{"type":"number"}}}`

	type wrapper struct {
		Schema RawMessage `json:"input_schema"`
	}
	got, err := Marshal(wrapper{Schema: RawMessage(schema)})
	require.NoError(t, err)
	//nolint:testifylint // JSONEq is order-insensitive; byte equality is the property under test.
	require.Equal(t, `{"input_schema":`+schema+`}`, string(got))
}
