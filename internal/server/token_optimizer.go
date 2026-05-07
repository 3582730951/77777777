package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/tidwall/gjson"
)

func (g *Gateway) optimizeResponsesToolOutputs(body []byte) []byte {
	if g == nil || g.cfg == nil {
		return body
	}
	return optimizeResponsesToolOutputs(body, g.cfg.TokenOptimizer)
}

func optimizeResponsesToolOutputs(body []byte, cfg config.TokenOptimizer) []byte {
	cfg = saneTokenOptimizerConfig(cfg)
	if cfg.Mode == "off" || !bytes.Contains(body, []byte(`"function_call_output"`)) {
		return body
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	var input []json.RawMessage
	if err := json.Unmarshal(req["input"], &input); err != nil || len(input) == 0 {
		return body
	}

	changed := false
	for i, raw := range input {
		if gjson.GetBytes(raw, "type").String() != "function_call_output" {
			continue
		}
		output := gjson.GetBytes(raw, "output")
		if !output.Exists() || output.Type != gjson.String {
			continue
		}
		optimized, ok := optimizeToolOutputText(output.String(), cfg)
		if !ok {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		encoded, err := json.Marshal(optimized)
		if err != nil {
			continue
		}
		item["output"] = encoded
		next, err := json.Marshal(item)
		if err != nil {
			continue
		}
		input[i] = json.RawMessage(next)
		changed = true
	}
	if !changed {
		return body
	}

	encodedInput, err := json.Marshal(input)
	if err != nil {
		return body
	}
	req["input"] = encodedInput
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func saneTokenOptimizerConfig(cfg config.TokenOptimizer) config.TokenOptimizer {
	switch cfg.Mode {
	case "", "safe":
		cfg.Mode = "safe"
	case "aggressive", "off":
	default:
		cfg.Mode = "safe"
	}
	if cfg.MinToolOutputBytes <= 0 {
		cfg.MinToolOutputBytes = 32 << 10
	}
	if cfg.MaxOptimizedToolOutputBytes <= 0 {
		cfg.MaxOptimizedToolOutputBytes = 64 << 10
	}
	if cfg.HeadLines <= 0 {
		cfg.HeadLines = 80
	}
	if cfg.TailLines <= 0 {
		cfg.TailLines = 80
	}
	if cfg.ErrorContextLines < 0 {
		cfg.ErrorContextLines = 0
	}
	if cfg.ErrorContextLines == 0 {
		cfg.ErrorContextLines = 6
	}
	return cfg
}

func optimizeToolOutputText(text string, cfg config.TokenOptimizer) (string, bool) {
	if len(text) < cfg.MinToolOutputBytes {
		return text, false
	}
	trimmed := strings.TrimSpace(text)
	if cfg.Mode == "safe" && json.Valid([]byte(trimmed)) {
		return text, false
	}

	cleaned := stripANSI(text)
	lines := strings.Split(cleaned, "\n")
	if len(lines) <= cfg.HeadLines+cfg.TailLines {
		compacted := compactRepeatedLines(lines)
		if len(compacted) >= len(text) {
			return text, false
		}
		return compacted, true
	}

	keep := make([]bool, len(lines))
	for i := 0; i < len(lines) && i < cfg.HeadLines; i++ {
		keep[i] = true
	}
	for i := maxServerInt(0, len(lines)-cfg.TailLines); i < len(lines); i++ {
		keep[i] = true
	}
	for i, line := range lines {
		if !importantToolOutputLine(line) {
			continue
		}
		start := maxServerInt(0, i-cfg.ErrorContextLines)
		end := minInt(len(lines)-1, i+cfg.ErrorContextLines)
		for j := start; j <= end; j++ {
			keep[j] = true
		}
	}

	var out []string
	omittedLines := 0
	omittedBytes := 0
	for i := 0; i < len(lines); {
		if keep[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		start := i
		bytesSkipped := 0
		for i < len(lines) && !keep[i] {
			bytesSkipped += len(lines[i]) + 1
			i++
		}
		n := i - start
		omittedLines += n
		omittedBytes += bytesSkipped
		out = append(out, fmt.Sprintf("[gateway token optimizer: omitted %d middle lines / %d bytes]", n, bytesSkipped))
	}

	optimized := compactRepeatedLines(out)
	if omittedLines > 0 {
		header := fmt.Sprintf("[gateway token optimizer: safe mode preserved head, tail, and error context; total omitted %d lines / %d bytes]\n", omittedLines, omittedBytes)
		optimized = header + optimized
	}
	if len(optimized) > cfg.MaxOptimizedToolOutputBytes {
		optimized = shrinkOptimizedOutput(optimized, cfg.MaxOptimizedToolOutputBytes)
	}
	if len(optimized) >= len(text) {
		return text, false
	}
	return optimized, true
}

func compactRepeatedLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		out = append(out, lines[i])
		if repeat := j - i; repeat > 3 {
			out = append(out, fmt.Sprintf("[gateway token optimizer: previous line repeated %d times]", repeat-1))
		} else {
			for k := i + 1; k < j; k++ {
				out = append(out, lines[k])
			}
		}
		i = j
	}
	return strings.Join(out, "\n")
}

func importantToolOutputLine(line string) bool {
	lower := strings.ToLower(line)
	patterns := []string{
		"error", "failed", "failure", "panic", "fatal", "exception", "traceback",
		"assertion", "undefined", "cannot", "no such file", "permission denied",
		"exit status", "--- fail", " fail:", "build failed", "test failed",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				b := s[i]
				if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
					break
				}
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func shrinkOptimizedOutput(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	marker := "\n[gateway token optimizer: output truncated to configured byte budget]\n"
	if maxBytes <= len(marker)+32 {
		return safeUTF8Prefix(s, maxBytes)
	}
	head := (maxBytes - len(marker)) / 2
	tail := maxBytes - len(marker) - head
	return safeUTF8Prefix(s, head) + marker + safeUTF8Suffix(s, tail)
}

func safeUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	for maxBytes > 0 && !utf8.ValidString(s[:maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func safeUTF8Suffix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.ValidString(s[start:]) {
		start++
	}
	return s[start:]
}

func maxServerInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
