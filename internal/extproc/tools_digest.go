// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
)

// contentHashLen matches the truncation used by redaction.ComputeContentHash, so
// fingerprints emitted here read the same way as the hashes operators already see
// in redacted debug logs.
const contentHashLen = 16

// toolsDigest summarises the tool definitions carried by a request.
//
// Chat templates render the tool declarations near the head of the prompt - Kimi
// K2's published template emits them ahead of the system message and every
// conversation message - so growing the tools block shifts the tokens after it and
// the conversation body has to be re-prefilled. That cost is
// arithmetic, not a bug: a client that loads new tool schemas mid-session pays it
// legitimately. What operators could not previously do is tell that apart from a
// gateway bug that mutates a stable tool set, or from a request landing on a
// replica that never saw the conversation. This digest makes all three
// distinguishable from the access log.
type toolsDigest struct {
	// present is false when the endpoint carries no tools concept at all, in
	// which case nothing is emitted.
	present bool
	// count is the number of tools forwarded upstream.
	count int
	// dropped is the number of tool union members the translation discards:
	// provider-native tools (bash, text editor, web search) and any member whose
	// type did not parse. They never reach the backend.
	dropped int
	// bytes is the length of the fingerprinted material, i.e. roughly the size of
	// the tools block.
	bytes int
	// fp fingerprints every forwarded tool, in order.
	fp string
	// prefixFP fingerprints every forwarded tool except the last one. When request
	// N's prefixFP equals request N-1's fp, the client appended exactly one tool
	// and nothing else moved - the cheap case. When it does not, the tool set was
	// reordered or an existing tool was edited, which is far more expensive and is
	// worth knowing about.
	prefixFP string
}

// computeToolsDigest fingerprints the tool definitions that will reach the
// backend. It hashes incrementally from the fields the translation forwards
// (name, description and the raw input schema), so it allocates nothing and never
// marshals the tools block.
func computeToolsDigest(tools []anthropic.ToolUnion) toolsDigest {
	d := toolsDigest{present: true}
	if len(tools) == 0 {
		return d
	}

	h := sha256.New()
	var (
		prefixArr [sha256.Size]byte
		prefixSum []byte
		sep       = []byte{0}
	)
	for i := range tools {
		tool := tools[i].Tool
		if tool == nil {
			// Provider-native or unparsed union member: anthropicToolsToOpenAI
			// skips it, so it contributes nothing to the upstream prompt.
			d.dropped++
			continue
		}
		// Snapshot before each tool, so after the loop this holds the digest of
		// everything up to but excluding the final tool. Sum appends without
		// disturbing the hash state, and reusing prefixArr keeps it allocation free.
		prefixSum = h.Sum(prefixArr[:0])

		h.Write([]byte(tool.Name))
		h.Write(sep)
		h.Write([]byte(tool.Description))
		h.Write(sep)
		h.Write(tool.InputSchema)
		h.Write(sep)

		d.count++
		d.bytes += len(tool.Name) + len(tool.Description) + len(tool.InputSchema) + 3
	}

	if d.count == 0 {
		return d
	}
	var sumArr [sha256.Size]byte
	d.fp = shortHash(h.Sum(sumArr[:0]))
	d.prefixFP = shortHash(prefixSum)
	return d
}

// shortHash renders a digest the way redaction.ComputeContentHash does.
func shortHash(sum []byte) string {
	return hex.EncodeToString(sum)[:contentHashLen]
}
