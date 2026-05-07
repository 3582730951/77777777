package stealth

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const scrubFallbackMaxBytes = 8 << 20

var scrubPaths = []string{
	"metadata.user_id",
	"metadata.session_id",
	"metadata.client_id",
	"metadata.device_id",
	"x-request-id",
}

// ScrubRequestBody removes client-identifying fields from the request body
// that could reveal multiple users sharing the same account.
func ScrubRequestBody(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"metadata"`)) && !bytes.Contains(body, []byte(`"x-request-id"`)) {
		return body
	}
	for _, p := range scrubPaths {
		res := gjson.GetBytes(body, p)
		if !res.Exists() {
			continue
		}
		if next, ok := deleteJSONFieldInPlace(body, res.Index, len(res.Raw)); ok {
			body = next
			continue
		}
		// sjson can allocate a full replacement buffer. Keep it as a small-body
		// fallback, but do not risk doubling memory for long-context requests.
		if len(body) <= scrubFallbackMaxBytes {
			body, _ = sjson.DeleteBytes(body, p)
		}
	}
	return body
}

func deleteJSONFieldInPlace(body []byte, valueStart, rawLen int) ([]byte, bool) {
	if valueStart <= 0 || rawLen <= 0 || valueStart >= len(body) || valueStart+rawLen > len(body) {
		return body, false
	}

	colon := skipJSONWhitespaceBackward(body, valueStart-1)
	if colon < 0 || body[colon] != ':' {
		return body, false
	}
	keyEnd := skipJSONWhitespaceBackward(body, colon-1)
	if keyEnd < 0 || body[keyEnd] != '"' {
		return body, false
	}
	keyStart := findJSONStringStart(body, keyEnd)
	if keyStart < 0 {
		return body, false
	}

	start := keyStart
	end := valueStart + rawLen
	next := skipJSONWhitespaceForward(body, end)
	if next < len(body) && body[next] == ',' {
		end = next + 1
	} else if prev := skipJSONWhitespaceBackward(body, start-1); prev >= 0 && body[prev] == ',' {
		start = prev
	}

	return append(body[:start], body[end:]...), true
}

func skipJSONWhitespaceBackward(body []byte, i int) int {
	for i >= 0 {
		switch body[i] {
		case ' ', '\n', '\r', '\t':
			i--
		default:
			return i
		}
	}
	return -1
}

func skipJSONWhitespaceForward(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func findJSONStringStart(body []byte, keyEnd int) int {
	for i := keyEnd - 1; i >= 0; i-- {
		if body[i] == '"' && !jsonQuoteEscaped(body, i) {
			return i
		}
	}
	return -1
}

func jsonQuoteEscaped(body []byte, quote int) bool {
	backslashes := 0
	for i := quote - 1; i >= 0 && body[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}
