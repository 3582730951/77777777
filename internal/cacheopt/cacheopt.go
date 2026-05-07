// Package cacheopt implements lossless request transformations that maximise
// upstream prompt-cache hit rate without changing model behaviour or output.
//
// Transforms applied (all semantics-preserving):
//  1. Tool schema canonicalisation — sort JSON keys + minify so identical
//     schemas always produce the same bytes regardless of client serialisation.
//  2. cache_control injection — mark system prompt + last tool with
//     {type:"ephemeral"} so Anthropic's KV cache anchors on the stable prefix.
//  3. Tool deduplication — not implemented here; handled by dedup/ package.
//
// Reference: sub2api gateway_tool_rewrite.go, Parrot cc_mimicry.py.
package cacheopt

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

// Apply runs all cache optimisations on req in-place.
// provider must be "claude", "chatgpt", or "gemini"; some opts are provider-specific.
func Apply(req *ir.Request, provider string) {
	if req == nil {
		return
	}
	// 1. Normalise tool schemas (all providers — keeps sticky routing stable).
	NormaliseTools(req.Tools)
	// 2. Inject cache breakpoints (Anthropic only).
	if provider == "claude" {
		InjectCacheBreakpoints(req)
	}
}

// NormaliseTools sorts JSON property keys in each tool's schema so that the
// same logical schema always produces the same bytes. This is required for
// upstream prefix-cache hits: if the client serialises properties in random
// order the provider treats each request as a new prompt even though the tools
// are semantically identical.
func NormaliseTools(tools []ir.ToolDef) {
	for i := range tools {
		if len(tools[i].Schema) == 0 {
			continue
		}
		if norm := canonicalJSON(tools[i].Schema); norm != nil {
			tools[i].Schema = norm
		}
	}
}

// InjectCacheBreakpoints marks the system prompt, last tool, and a historical
// user message for Anthropic cache_control injection. The anthropic encoder
// reads these flags and emits {cache_control:{type:"ephemeral"}} blocks.
//
// Breakpoint strategy (matching CPA/sub2api/Anthropic best practice):
//  1. System: mark if len > 800 chars (≈200+ tokens)
//  2. Tools: always mark the last tool (stable prefix across turns)
//  3. Messages: mark the second-to-last user message (caches conversation
//     history; each new turn only sends the last message uncached)
//
// Max 4 breakpoints per request (Anthropic limit).
// Breakpoint 1: system prompt (if > 800 chars)
// Breakpoint 2: last tool definition
// Breakpoint 3: second-to-last user message
// Breakpoint 4: largest tool_result > 2048 bytes (new)
func InjectCacheBreakpoints(req *ir.Request) {
	used := 0
	if len(req.System) > 800 {
		req.SystemCached = true
		used++
	}
	if len(req.Tools) > 0 {
		req.Tools[len(req.Tools)-1].CacheBreakpoint = true
		used++
	}
	if len(req.Messages) >= 3 {
		lastUserIdx := -1
		secondLastUserIdx := -1
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == ir.RoleUser {
				if lastUserIdx < 0 {
					lastUserIdx = i
				} else {
					secondLastUserIdx = i
					break
				}
			}
		}
		if secondLastUserIdx >= 0 {
			req.MessageCacheIdx = secondLastUserIdx
			used++
		}
	}
	// 4th breakpoint: cache the largest tool_result that exceeds 2048 bytes.
	if used < 4 {
		bestIdx, bestPart, bestSize := -1, -1, 0
		for i := range req.Messages {
			for j := range req.Messages[i].Parts {
				p := &req.Messages[i].Parts[j]
				if p.Kind == ir.PartToolResult && len(p.ToolResultBytes) > 2048 && len(p.ToolResultBytes) > bestSize {
					bestIdx, bestPart, bestSize = i, j, len(p.ToolResultBytes)
				}
			}
		}
		if bestIdx >= 0 {
			req.Messages[bestIdx].Parts[bestPart].CacheBreakpoint = true
		}
	}
}

// canonicalJSON recursively sorts object keys and minifies a JSON value.
// Returns nil if input is already canonical or on parse error (no mutation).
func canonicalJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	sorted := deepSort(v)
	out, err := json.Marshal(sorted)
	if err != nil {
		return nil
	}
	if bytes.Equal(out, raw) {
		return nil // already canonical, avoid allocation
	}
	return out
}

func deepSort(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(val))
		for _, k := range keys {
			out[k] = deepSort(val[k])
		}
		return out
	case []any:
		for i, item := range val {
			val[i] = deepSort(item)
		}
		return val
	default:
		return v
	}
}
