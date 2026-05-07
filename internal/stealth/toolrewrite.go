package stealth

import (
	"bytes"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

// ToolRewriter masks non-canonical tool names to avoid third-party detection.
// Uses deterministic FNV-64a hash seeded from tool names for stable mapping.
// The rewrite is bidirectional: Apply() on request, Restore() on response bytes.
type ToolRewriter struct {
	forward map[string]string // original → fake
	reverse []replacement     // sorted longest-first for safe bytes.Replace
}

type replacement struct {
	fake string
	orig string
}

var prefixes = []string{
	"compute", "fetch", "generate", "lookup", "process",
	"analyze", "extract", "resolve", "validate", "transform",
	"inspect", "evaluate", "compile", "render", "index",
	"search", "classify", "measure", "optimize", "collect",
	"derive", "aggregate",
}

var canonicalCLIToolNames = map[string]bool{
	"agent":         true,
	"bash":          true,
	"edit":          true,
	"enterplanmode": true,
	"glob":          true,
	"grep":          true,
	"ls":            true,
	"notebookedit":  true,
	"question":      true,
	"read":          true,
	"skill":         true,
	"task":          true,
	"taskread":      true,
	"todoread":      true,
	"todowrite":     true,
	"webfetch":      true,
	"websearch":     true,
	"write":         true,
}

// NewToolRewriter creates a rewriter from the request's tool definitions.
// It is deliberately conservative: small tool sets and canonical CLI tool
// names are left untouched so model tool-selection quality is not degraded.
func NewToolRewriter(tools []ir.ToolDef) *ToolRewriter {
	if len(tools) <= 5 {
		return nil
	}
	targets := make([]ir.ToolDef, 0, len(tools))
	h := fnv.New64a()
	for _, t := range tools {
		h.Write([]byte(t.Name))
		if canonicalCLIToolNames[strings.ToLower(t.Name)] {
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return nil
	}
	seed := h.Sum64()

	tw := &ToolRewriter{forward: make(map[string]string, len(targets))}
	for i, t := range targets {
		fake := generateFakeName(t.Name, i, seed)
		tw.forward[t.Name] = fake
		tw.reverse = append(tw.reverse, replacement{fake: fake, orig: t.Name})
	}
	// Sort longest fake name first to prevent substring collision during restore.
	sort.Slice(tw.reverse, func(i, j int) bool {
		return len(tw.reverse[i].fake) > len(tw.reverse[j].fake)
	})
	return tw
}

func generateFakeName(origName string, idx int, seed uint64) string {
	pidx := int((seed + uint64(idx)) % uint64(len(prefixes)))
	head := origName
	if len(head) > 4 {
		head = head[:4]
	}
	// Capitalize first letter of head.
	if len(head) > 0 && head[0] >= 'a' && head[0] <= 'z' {
		head = string(head[0]-32) + head[1:]
	}
	buf := make([]byte, 0, 20)
	buf = append(buf, prefixes[pidx]...)
	buf = append(buf, '_')
	buf = append(buf, head...)
	buf = append(buf, byte('0'+idx/10), byte('0'+idx%10))
	return string(buf)
}

// Apply rewrites tool names in the request in-place.
func (tw *ToolRewriter) Apply(req *ir.Request) {
	if tw == nil {
		return
	}
	for i := range req.Tools {
		if fake, ok := tw.forward[req.Tools[i].Name]; ok {
			req.Tools[i].Name = fake
		}
	}
	if req.ToolChoice.Name != "" {
		if fake, ok := tw.forward[req.ToolChoice.Name]; ok {
			req.ToolChoice.Name = fake
		}
	}
	// Rewrite tool names in message parts (tool_use and tool_result references).
	for i := range req.Messages {
		for j := range req.Messages[i].Parts {
			p := &req.Messages[i].Parts[j]
			if p.Kind == ir.PartToolUse {
				if fake, ok := tw.forward[p.ToolUseName]; ok {
					p.ToolUseName = fake
				}
			}
		}
	}
}

// Restore replaces fake tool names back to originals in response bytes.
func (tw *ToolRewriter) Restore(data []byte) []byte {
	if tw == nil {
		return data
	}
	for _, r := range tw.reverse {
		data = bytes.ReplaceAll(data, []byte(r.fake), []byte(r.orig))
	}
	return data
}
