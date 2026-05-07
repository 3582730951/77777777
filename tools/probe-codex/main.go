// Probe what API surface the account actually has access to.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/llm-pool/gateway/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe-codex <account-id>")
		os.Exit(2)
	}
	st, _ := store.Open("data/pool.db", os.Getenv("POOL_MASTER_KEY"))
	defer st.Close()
	sec, err := st.GetAccountSecret(context.Background(), os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var s struct {
		AccessToken string `json:"accessToken"`
		Account     struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	json.Unmarshal([]byte(strings.TrimSpace(sec.SessionToken)), &s)
	fmt.Println("Account ID:", s.Account.ID)
	fmt.Println()

	endpoints := []string{
		"https://chatgpt.com/backend-api/codex/responses?_probe=1",
		"https://chatgpt.com/backend-api/codex/me",
		"https://chatgpt.com/backend-api/codex/models",
		"https://chatgpt.com/backend-api/codex/usage",
		"https://chatgpt.com/backend-api/me",
		"https://chatgpt.com/backend-api/models",
		"https://api.openai.com/v1/models",
		"https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27",
	}
	for _, ep := range endpoints {
		fmt.Println("== GET", ep, "==")
		req, _ := http.NewRequest("GET", ep, nil)
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("ChatGPT-Account-Id", s.Account.ID)
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("User-Agent", "codex_cli_rs/0.1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("ERR", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bs := string(body)
		if len(bs) > 600 {
			bs = bs[:600] + "...[truncated]"
		}
		fmt.Println(resp.StatusCode, bs)
		fmt.Println()
	}
}
