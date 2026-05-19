package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func TestBuildUpstreamSessionKeyIsStableForSameClientAndFirstTurn(t *testing.T) {
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	httpReq.Header.Set("X-Forwarded-For", "198.51.100.10, 10.0.0.1")
	res := auth.Resolved{
		Tenant: &domain.Tenant{ID: "tenant-1"},
		Group:  &domain.Group{ID: "claude", TenantID: "tenant-1", Provider: "claude"},
		APIKey: "sk-pool-test",
	}
	irReq := &ir.Request{
		OriginalProto: "anthropic",
		OriginalModel: "claude-sonnet-4-6",
		System:        "system",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "first user request"}},
		}},
	}

	first := buildUpstreamSessionKey(httpReq, res, irReq, "")
	second := buildUpstreamSessionKey(httpReq, res, irReq, "")

	if first == "" {
		t.Fatal("session key should not be empty")
	}
	if first != second {
		t.Fatalf("session key changed for same request context: %q vs %q", first, second)
	}
}

func TestBuildUpstreamSessionKeySeparatesClientIP(t *testing.T) {
	res := auth.Resolved{
		Tenant: &domain.Tenant{ID: "tenant-1"},
		Group:  &domain.Group{ID: "claude", TenantID: "tenant-1", Provider: "claude"},
		APIKey: "sk-pool-test",
	}
	irReq := &ir.Request{
		OriginalProto: "anthropic",
		OriginalModel: "claude-sonnet-4-6",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "same prompt"}},
		}},
	}
	reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqA.Header.Set("X-Real-IP", "198.51.100.10")
	reqB := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqB.Header.Set("X-Real-IP", "198.51.100.11")

	if gotA, gotB := buildUpstreamSessionKey(reqA, res, irReq, ""), buildUpstreamSessionKey(reqB, res, irReq, ""); gotA == gotB {
		t.Fatalf("different client IPs should produce isolated session keys: %q", gotA)
	}
}
