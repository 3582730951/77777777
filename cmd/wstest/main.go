package main

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/provider/windsurf"
	"github.com/llm-pool/gateway/internal/store"
)

func main() {
	// Open store to save test account
	st, err := store.Open("/home/12/llm-pool/data/pool.db", "")
	if err != nil {
		panic(err)
	}
	defer st.Close()

	apiKey := "devin-session-token$eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzZXNzaW9uX2lkIjoid2luZHN1cmYtc2Vzc2lvbi00NTE4YmNkZmI4ZTE0NWU5YmE0ZDRjNmFkYWZhNDg5YSJ9.GfkcVRX6Yna8rhw4HBLHNXs0pQL3-oMgpstzBIvG_f4"

	// Upsert test windsurf account
	acc := &domain.Account{
		ID:       "acc-ws-test",
		TenantID: "default",
		Provider: "windsurf",
		PlanTier: "pro",
		State:    domain.StateActive,
	}
	sec := store.AccountSecret{SessionToken: apiKey}
	if err := st.UpsertAccount(context.Background(), acc, sec); err != nil {
		panic(err)
	}
	fmt.Println("Account saved.")

	// Create provider in real mode
	prov := windsurf.New("real")
	prov.SetStore(st)

	// Build a simple chat request
	req := &ir.Request{
		Model: "claude-3.5-sonnet",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "Hello! Please respond with just 'Hi there!' and nothing else."}}},
		},
		Stream: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println("Invoking windsurf provider...")
	ch, err := prov.Invoke(ctx, acc, req)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}

	fmt.Println("Streaming response:")
	for ev := range ch {
		switch ev.Kind {
		case ir.EvTextDelta:
			fmt.Print(ev.Text)
		case ir.EvError:
			fmt.Printf("\n[ERROR] %v\n", ev.Err)
		case ir.EvDone:
			fmt.Printf("\n[DONE] finish_reason=%s\n", ev.FinishReason)
		case ir.EvUsage:
			fmt.Printf("[USAGE] in=%d out=%d\n", ev.InputTokens, ev.OutputTokens)
		}
	}
}
