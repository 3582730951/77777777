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

func (g *Gateway) optimizeResponsesToolOutputs(body []byte, provider string) []byte {
	if g == nil || g.cfg == nil {
		return body
	}
	if provider != "chatgpt" {
		return body
	}
	return optimizeResponsesToolOutputs(body, g.cfg.TokenOptimizer)
}

func optimizeResponsesToolOutputsForProvider(body []byte, cfg config.TokenOptimizer, provider string) []byte {
	if provider != "chatgpt" {
		return body
	}
	return optimizeResponsesToolOutputs(body, cfg)
}

func optimizeResponsesToolOutputs(body []byte, cfg config.TokenOptimizer) []byte {
	cfg = config.NormalizeTokenOptimizer(cfg)
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
		// Hard boundary: only tool results are eligible. System prompts,
		// instructions, user messages, model params, and group config stay exact.
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

func optimizeToolOutputText(text string, cfg config.TokenOptimizer) (string, bool) {
	if len(text) < cfg.MinToolOutputBytes {
		return text, false
	}
	switch cfg.Mode {
	case "cleanup":
		return cleanupToolOutputText(text)
	case "guarded":
		cleaned, cleanedOK := cleanupToolOutputText(text)
		candidate := text
		if cleanedOK {
			candidate = cleaned
		}
		if !guardedToolOutputCandidate(candidate) {
			return candidate, cleanedOK
		}
		optimized, ok := truncateToolOutputText(candidate, cfg, "guarded")
		if !ok {
			return candidate, cleanedOK
		}
		return optimized, true
	case "safe", "aggressive":
		return truncateToolOutputText(text, cfg, cfg.Mode)
	default:
		return text, false
	}
}

func cleanupToolOutputText(text string) (string, bool) {
	cleaned := stripTerminalNoise(text)
	cleaned = compactBlankLineRuns(cleaned, 2)
	if cleaned == text || len(cleaned) >= len(text) {
		return text, false
	}
	return cleaned, true
}

func truncateToolOutputText(text string, cfg config.TokenOptimizer, modeLabel string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if (cfg.Mode == "safe" || cfg.Mode == "guarded") && json.Valid([]byte(trimmed)) {
		return text, false
	}

	cleaned := stripTerminalNoise(text)
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
		header := fmt.Sprintf("[gateway token optimizer: %s mode preserved head, tail, and error context; total omitted %d lines / %d bytes]\n", modeLabel, omittedLines, omittedBytes)
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
		"npm err!", "assertionerror", "typeerror", "referenceerror", "syntaxerror",
		"compilation failed", "failed to compile", "panic:",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func guardedToolOutputCandidate(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return false
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "\ndiff --git ") || strings.Contains(lower, "\n@@ ") {
		return false
	}
	patterns := []string{
		"=== run", "--- fail:", "--- pass:", "\nfail\t", "\nok\t",
		"pytest", "short test summary", "\nfailed ", "traceback (most recent call last)",
		"error ts", "typeerror:", "assertionerror", "npm err!", "eslint", "ruff",
		"mypy", "panic:", "fatal error", "build failed", "compilation failed",
		"failed to compile", "test failed",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func stripTerminalNoise(s string) string {
	return collapseCarriageReturnFrames(stripANSI(s))
}

func collapseCarriageReturnFrames(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.SplitAfter(normalized, "\n")
	var out strings.Builder
	out.Grow(len(normalized))
	for _, part := range parts {
		hasNewline := strings.HasSuffix(part, "\n")
		line := strings.TrimSuffix(part, "\n")
		if strings.Contains(line, "\r") {
			frames := strings.Split(line, "\r")
			line = frames[len(frames)-1]
		}
		out.WriteString(line)
		if hasNewline {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func compactBlankLineRuns(s string, maxRun int) string {
	if maxRun < 1 || !strings.Contains(s, "\n\n\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0
	changed := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blankRun++
			if blankRun > maxRun {
				changed = true
				continue
			}
		} else {
			blankRun = 0
		}
		out = append(out, line)
	}
	if !changed {
		return s
	}
	return strings.Join(out, "\n")
}

func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				i += 2
				for i < len(s) {
					b := s[i]
					if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
						break
					}
					i++
				}
				continue
			case ']':
				i += 2
				for i < len(s) {
					if s[i] == 0x07 {
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i++
						break
					}
					i++
				}
				continue
			default:
				if s[i+1] >= 0x20 && s[i+1] <= 0x2f {
					i += 2
					for i < len(s) {
						b := s[i]
						if b >= 0x30 && b <= 0x7e {
							break
						}
						i++
					}
					continue
				}
			}
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
