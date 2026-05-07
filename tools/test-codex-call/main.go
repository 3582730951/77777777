// quick-and-dirty CLI: dump account → call /backend-api/codex/responses with
// its accessToken → print the SSE stream. Used to verify Codex endpoint works
// before writing the full provider integration.
//
// Usage:
//   POOL_MASTER_KEY=test-key go run ./tools/test-codex-call <account-id> "<your prompt>"
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/store"
)

type sessionRaw struct {
	AccessToken string `json:"accessToken"`
	Account     struct {
		ID       string `json:"id"`
		PlanType string `json:"planType"`
	} `json:"account"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: test-codex-call <account-id> <prompt>")
		os.Exit(2)
	}
	accountID := os.Args[1]
	prompt := os.Args[2]

	st, err := store.Open("data/pool.db", os.Getenv("POOL_MASTER_KEY"))
	if err != nil {
		bail("open store: %v", err)
	}
	defer st.Close()

	sec, err := st.GetAccountSecret(context.Background(), accountID)
	if err != nil {
		bail("get secret: %v", err)
	}
	var sess sessionRaw
	s := strings.TrimSpace(sec.SessionToken)
	if !strings.HasPrefix(s, "{") {
		bail("this account is not in 'JSON session' format; resolution via /api/auth/session not supported by this tool")
	}
	if err := json.Unmarshal([]byte(s), &sess); err != nil {
		bail("parse session: %v", err)
	}
	if sess.AccessToken == "" {
		bail("no accessToken in stored session")
	}
	fmt.Println("== using account ==")
	fmt.Println("id        :", accountID)
	fmt.Println("plan      :", sess.Account.PlanType)
	fmt.Println("account_id:", sess.Account.ID)
	fmt.Println("token     :", sess.AccessToken[:30]+"...")
	fmt.Println()

	model := os.Getenv("MODEL")
	if model == "" {
		model = "gpt-5-codex"
	}
	body := map[string]interface{}{
		"model": model,
		"input": []map[string]interface{}{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": prompt},
				},
			},
		},
		"instructions": "You are Codex, an AI coding assistant.",
		"reasoning":    map[string]string{"effort": "medium"},
		"store":        false,
		"stream":       true,
		"include":      []string{"reasoning.encrypted_content"},
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		"https://chatgpt.com/backend-api/codex/responses",
		bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("session_id", randID())
	req.Header.Set("ChatGPT-Account-Id", sess.Account.ID)
	req.Header.Set("User-Agent", "codex_cli_rs/0.1.0 (Linux 6.6; x86_64) Codex/1.0")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		bail("http: %v", err)
	}
	defer resp.Body.Close()

	fmt.Println("== HTTP", resp.StatusCode, "==")
	for k, v := range resp.Header {
		fmt.Printf("%s: %s\n", k, v[0])
	}
	fmt.Println()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		fmt.Println(string(b))
		os.Exit(1)
	}

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if err != nil {
			break
		}
	}
}

func bail(f string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "fatal: "+f+"\n", args...)
	os.Exit(1)
}

func randID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
