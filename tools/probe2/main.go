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
	st, _ := store.Open("data/pool.db", os.Getenv("POOL_MASTER_KEY"))
	defer st.Close()
	sec, _ := st.GetAccountSecret(context.Background(), "acc-b399ee026fbd")
	var s struct{ AccessToken string `json:"accessToken"`; Account struct{ ID string `json:"id"` } `json:"account"` }
	json.Unmarshal([]byte(strings.TrimSpace(sec.SessionToken)), &s)

	for _, ep := range []string{
		"https://chatgpt.com/backend-api/codex/models?client_version=0.1.0",
		"https://chatgpt.com/backend-api/codex/models?client_version=0.45.0",
		"https://chatgpt.com/backend-api/codex/responses_metadata",
		"https://chatgpt.com/backend-api/codex/state",
		"https://chatgpt.com/backend-api/codex/auth_request",
	} {
		fmt.Println("== GET", ep)
		req, _ := http.NewRequest("GET", ep, nil)
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
		req.Header.Set("ChatGPT-Account-Id", s.Account.ID)
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("User-Agent", "codex_cli_rs/0.45.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil { fmt.Println(err); continue }
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bs := string(body)
		if len(bs) > 1500 { bs = bs[:1500] + "..." }
		fmt.Println(resp.StatusCode, bs); fmt.Println()
	}
}
