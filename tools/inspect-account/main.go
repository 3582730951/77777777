package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/llm-pool/gateway/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect-account <account-id>")
		os.Exit(2)
	}
	id := os.Args[1]
	st, err := store.Open("data/pool.db", os.Getenv("POOL_MASTER_KEY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer st.Close()
	sec, err := st.GetAccountSecret(context.Background(), id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "get:", err)
		os.Exit(1)
	}
	fmt.Println("Account:", id)
	fmt.Println("SessionToken bytes:", len(sec.SessionToken))
	fmt.Println("RefreshToken bytes:", len(sec.RefreshToken))
	fmt.Println("Cookies bytes:", len(sec.Cookies))
	fmt.Println("--- session_token first 1500 chars ---")
	st_ := sec.SessionToken
	if len(st_) > 1500 {
		st_ = st_[:1500]
	}
	fmt.Println(st_)
	fmt.Println("--- session_token last 500 chars ---")
	full := sec.SessionToken
	if len(full) > 500 {
		fmt.Println(full[len(full)-500:])
	}
	fmt.Println()
	fmt.Println("--- cookies first 1000 ---")
	c := string(sec.Cookies)
	if len(c) > 1000 {
		c = c[:1000]
	}
	fmt.Println(c)

	fmt.Println()
	fmt.Println("=== Detection ===")
	if strings.HasPrefix(strings.TrimSpace(sec.SessionToken), "[") || strings.HasPrefix(strings.TrimSpace(sec.SessionToken), "{") {
		fmt.Println("session_token looks like JSON (probably full Cookie-Editor export)")
	}
	if strings.Contains(sec.SessionToken, "next-auth.session-token") {
		fmt.Println("contains __Secure-next-auth.session-token cookie ✓")
	}
	if strings.Contains(sec.SessionToken, "cf_clearance") {
		fmt.Println("contains cf_clearance ✓")
	}
}
