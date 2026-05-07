// sentinel.go — ChatGPT's anti-bot pre-flight. Before /backend-api/conversation
// will accept a request, we POST /backend-api/sentinel/chat-requirements with
// the bearer access token; the response either:
//
//   1. returns a `token` to put in OpenAI-Sentinel-Chat-Requirements-Token
//      and a proof-of-work spec we have to solve, OR
//   2. returns a Turnstile / arkose challenge that requires a human (in which
//      case we surface ErrCFChallenge so the failover loop can route around).
//
// The PoW format used by ChatGPT today is described in chat-gpt reverse-eng
// repos: BASE64(sha3-512(seed + "|" + payload)). The "difficulty" string is
// a hex prefix that the SHA3 output must be < (compared lexicographically).
// We brute-force a numeric nonce until satisfied. For Plus accounts the seed
// is short and difficulty is usually 5-6 hex chars, taking <100ms.
package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

type chatRequirements struct {
	Token       string                 `json:"token"`
	ProofOfWork *proofSpec             `json:"proofofwork"`
	Turnstile   *turnstileSpec         `json:"turnstile"`
	Arkose      *arkoseSpec            `json:"arkose"`
	Persona     string                 `json:"persona"`
	Force       map[string]interface{} `json:"force_login,omitempty"`
}

type proofSpec struct {
	Required   bool   `json:"required"`
	Seed       string `json:"seed"`
	Difficulty string `json:"difficulty"`
}

type turnstileSpec struct {
	Required bool   `json:"required"`
	DX       string `json:"dx"`
}

type arkoseSpec struct {
	Required bool   `json:"required"`
	DX       string `json:"dx"`
}

// fetchChatRequirements asks ChatGPT for the per-request anti-bot token. The
// returned token + (optionally solved) PoW are pumped into the conversation
// request as headers.
func (p *Provider) fetchChatRequirements(ctx context.Context, accessToken, ua, cookieHeader string) (reqToken, powToken string, err error) {
	body := map[string]any{"p": generatePowProof("0", "0", "0")} // placeholder body, server only needs the bearer
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/sentinel/chat-requirements",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("OAI-Language", "en-US")
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("chat-requirements: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("chat-requirements %d: %s", resp.StatusCode, snippet(respBody))
	}
	var cr chatRequirements
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", "", fmt.Errorf("decode chat-requirements: %w", err)
	}
	if cr.Turnstile != nil && cr.Turnstile.Required {
		return "", "", errors.New("turnstile required (captcha solver not configured)")
	}
	if cr.Arkose != nil && cr.Arkose.Required {
		return "", "", errors.New("arkose challenge required (solver not configured)")
	}
	if cr.ProofOfWork != nil && cr.ProofOfWork.Required {
		powToken = solvePoW(cr.ProofOfWork.Seed, cr.ProofOfWork.Difficulty, ua)
		if powToken == "" {
			return "", "", errors.New("pow solve failed")
		}
	}
	return cr.Token, powToken, nil
}

// solvePoW finds a nonce so that sha3-512(payload + "|" + seed) hex starts
// with leading characters less than `difficulty`. The payload format is
// what ChatGPT expects in the JSON-encoded prefix; we try nonces 0..N.
//
// Reference: chatgpt.com static js / community repos.
func solvePoW(seed, difficulty, ua string) string {
	now := time.Now().UTC().Format(time.RFC1123)
	prefix := []any{
		1024,
		now,
		nil,
		nil,
		"en-US",
		"en-US,en",
		ua,
		"",
		"de-DE",
		0,
	}
	for nonce := 0; nonce < 500_000; nonce++ {
		prefix[3] = nonce
		jb, _ := json.Marshal(prefix)
		payload := base64.StdEncoding.EncodeToString(jb)
		full := payload + "." + seed
		h := sha3.Sum512([]byte(full))
		hex := encodeHex(h[:])
		if hex < difficulty {
			return "gAAAAAB" + payload
		}
	}
	return ""
}

func generatePowProof(_, _, _ string) string {
	// tiny placeholder for the chat-requirements POST body
	return "gAAAAAA"
}

func encodeHex(b []byte) string {
	const tab = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = tab[v>>4]
		out[i*2+1] = tab[v&0xf]
	}
	return string(out)
}

// makeCookieHeader formats the stored cookies into a single Cookie: header.
// secret can be raw next-auth cookie or "name1=val1; name2=val2" multiline.
func makeCookieHeader(rawCookieField, sessionToken string) string {
	parts := []string{}
	st := strings.TrimSpace(sessionToken)
	if st != "" && !strings.HasPrefix(st, "{") {
		parts = append(parts, "__Secure-next-auth.session-token="+st)
	}
	for _, line := range strings.Split(rawCookieField, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// netscape "name=value" or "domain\tFLAG\tpath\tFLAG\texpiry\tname\tvalue"
		if strings.Contains(line, "\t") {
			fs := strings.Split(line, "\t")
			if len(fs) >= 7 {
				parts = append(parts, fs[5]+"="+fs[6])
				continue
			}
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}
