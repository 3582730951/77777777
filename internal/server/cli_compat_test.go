package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/tidwall/gjson"
)

func TestCodexModelsAdvertisePriorityServiceTier(t *testing.T) {
	gw := &Gateway{}
	group := &domain.Group{
		ID:         "g",
		TenantID:   "default",
		Provider:   "chatgpt",
		Models:     []string{"gpt-5.5"},
		AccountIDs: []string{"acc-1", "acc-2"},
	}
	req := httptest.NewRequest(http.MethodGet, "/backend-api/codex/models?client_version=0.99.0", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group}))
	rec := httptest.NewRecorder()

	gw.handleCodexModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := gjson.Get(body, "models.0.slug").String(); got != "gpt-5.5" {
		t.Fatalf("model slug = %q body=%s", got, body)
	}
	if got := gjson.Get(body, "models.0.service_tiers.0.id").String(); got != "priority" {
		t.Fatalf("service tier id = %q, want priority body=%s", got, body)
	}
	if got := gjson.Get(body, "models.0.service_tiers.0.name").String(); got != "fast" {
		t.Fatalf("service tier name = %q, want fast body=%s", got, body)
	}
	if !gjson.Get(body, "models.0.supported_in_api").Bool() {
		t.Fatalf("model should be supported_in_api body=%s", body)
	}
	if got := gjson.Get(body, "models.0.truncation_policy.mode").String(); got == "" {
		t.Fatalf("missing current Codex ModelInfo fields body=%s", body)
	}
}

func TestListModelsCodexUAUsesCodexModelsResponseWithPriorityServiceTier(t *testing.T) {
	gw := &Gateway{}
	group := &domain.Group{
		ID:       "g",
		TenantID: "default",
		Provider: "chatgpt",
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("User-Agent", "codex_cli_rs/0.99.0")
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group}))
	rec := httptest.NewRecorder()

	gw.handleListModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if gjson.Get(body, "data").Exists() {
		t.Fatalf("codex UA should receive Codex ModelsResponse, not OpenAI list body=%s", body)
	}
	if got := gjson.Get(body, "models.0.service_tiers.0.id").String(); got != "priority" {
		t.Fatalf("service tier id = %q, want priority body=%s", got, body)
	}
}
