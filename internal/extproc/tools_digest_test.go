// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

func testTool(t *testing.T, name string) anthropic.ToolUnion {
	t.Helper()
	var tool anthropic.ToolUnion
	require.NoError(t, json.Unmarshal([]byte(
		`{"type":"custom","name":"`+name+`","description":"does `+name+`",`+
			`"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}`), &tool))
	require.NotNil(t, tool.Tool)
	return tool
}

func TestComputeToolsDigest_Empty(t *testing.T) {
	d := computeToolsDigest(nil)
	assert.True(t, d.present)
	assert.Zero(t, d.count)
	assert.Zero(t, d.dropped)
	assert.Empty(t, d.fp)
	assert.Empty(t, d.prefixFP)
}

func TestComputeToolsDigest_StableAcrossRepeats(t *testing.T) {
	tools := []anthropic.ToolUnion{testTool(t, "Write"), testTool(t, "Read")}

	want := computeToolsDigest(tools)
	require.NotEmpty(t, want.fp)
	for i := 0; i < 50; i++ {
		assert.Equal(t, want, computeToolsDigest(tools), "digest must be stable (iteration %d)", i)
	}
}

// TestComputeToolsDigest_PrefixMatchesShorterSet is the property the diagnostic
// rests on: an appended tool leaves the fingerprint of everything before it
// unchanged, so request N's prefixFP identifies request N-1's tool set exactly.
func TestComputeToolsDigest_PrefixMatchesShorterSet(t *testing.T) {
	before := []anthropic.ToolUnion{testTool(t, "Write"), testTool(t, "Read")}
	after := append(append([]anthropic.ToolUnion{}, before...), testTool(t, "Bash"))

	assert.Equal(t, computeToolsDigest(before).fp, computeToolsDigest(after).prefixFP)
}

// TestComputeToolsDigest_ReorderChangesFingerprint shows the case the diagnostic
// must not miss: same tools, same count, different order is expensive upstream
// and has to look different here.
func TestComputeToolsDigest_ReorderChangesFingerprint(t *testing.T) {
	a := computeToolsDigest([]anthropic.ToolUnion{testTool(t, "Write"), testTool(t, "Read")})
	b := computeToolsDigest([]anthropic.ToolUnion{testTool(t, "Read"), testTool(t, "Write")})

	assert.Equal(t, a.count, b.count)
	assert.NotEqual(t, a.fp, b.fp)
}

func TestComputeToolsDigest_SchemaEditChangesFingerprint(t *testing.T) {
	original := testTool(t, "Write")
	edited := testTool(t, "Write")
	edited.Tool.InputSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"number"}}}`)

	assert.NotEqual(t,
		computeToolsDigest([]anthropic.ToolUnion{original}).fp,
		computeToolsDigest([]anthropic.ToolUnion{edited}).fp)
}

// TestComputeToolsDigest_CountsDroppedTools covers both kinds of union member
// that anthropicToolsToOpenAI discards, so the access log shows tools going
// missing rather than hiding it.
func TestComputeToolsDigest_CountsDroppedTools(t *testing.T) {
	var unknown anthropic.ToolUnion
	require.NoError(t, json.Unmarshal([]byte(`{"type":"some_future_tool_20990101","name":"x"}`), &unknown))
	require.Nil(t, unknown.Tool)

	tools := []anthropic.ToolUnion{
		testTool(t, "Write"),
		{BashTool: &anthropic.BashTool{Type: "bash_20250124", Name: "bash"}},
		unknown,
		testTool(t, "Read"),
	}

	d := computeToolsDigest(tools)
	assert.Equal(t, 2, d.count)
	assert.Equal(t, 2, d.dropped)
	// Dropped members must not perturb the fingerprint of what is actually sent.
	assert.Equal(t, computeToolsDigest([]anthropic.ToolUnion{testTool(t, "Write"), testTool(t, "Read")}).fp, d.fp)
}

func TestComputeToolsDigest_OnlyDroppedTools(t *testing.T) {
	d := computeToolsDigest([]anthropic.ToolUnion{
		{BashTool: &anthropic.BashTool{Type: "bash_20250124", Name: "bash"}},
	})
	assert.True(t, d.present)
	assert.Zero(t, d.count)
	assert.Equal(t, 1, d.dropped)
	assert.Empty(t, d.fp)
}

func TestBuildToolsDigestDynamicMetadata(t *testing.T) {
	t.Run("absent for endpoints without tools", func(t *testing.T) {
		assert.Nil(t, buildToolsDigestDynamicMetadata(toolsDigest{}))
	})

	t.Run("counts only when no tool survives", func(t *testing.T) {
		md := buildToolsDigestDynamicMetadata(toolsDigest{present: true, dropped: 1})
		require.NotNil(t, md)
		fields := md.Fields[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().GetFields()
		assert.Equal(t, "0", fields["tools_count"].GetStringValue())
		assert.Equal(t, "1", fields["tools_dropped"].GetStringValue())
		assert.NotContains(t, fields, "tools_fp")
	})

	t.Run("full digest", func(t *testing.T) {
		md := buildToolsDigestDynamicMetadata(computeToolsDigest([]anthropic.ToolUnion{
			testTool(t, "Write"), testTool(t, "Read"),
		}))
		require.NotNil(t, md)
		fields := md.Fields[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().GetFields()
		assert.Equal(t, "2", fields["tools_count"].GetStringValue())
		assert.Equal(t, "0", fields["tools_dropped"].GetStringValue())
		// Every value is a string, so Envoy's formatter cannot render a count in
		// exponent form the way it does for NumberValue.
		for k, v := range fields {
			assert.IsType(t, &structpb.Value_StringValue{}, v.GetKind(), "field %s must be a string", k)
		}
		assert.Len(t, fields["tools_fp"].GetStringValue(), contentHashLen)
		assert.Len(t, fields["tools_prefix_fp"].GetStringValue(), contentHashLen)
	})
}
