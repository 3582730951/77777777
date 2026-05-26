package admin

import (
	"strings"

	chatgptprovider "github.com/llm-pool/gateway/internal/provider/chatgpt"
	"github.com/llm-pool/gateway/internal/store"
)

func normalizeChatGPTAccountSecret(provider string, sec store.AccountSecret) store.AccountSecret {
	if !strings.EqualFold(strings.TrimSpace(provider), "chatgpt") {
		return sec
	}
	if !chatgptprovider.IsSessionOnlyAuthJSON(sec.SessionToken) {
		return sec
	}
	sec.RefreshToken = ""
	sec.Cookies = nil
	return sec
}
