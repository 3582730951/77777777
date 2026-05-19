package chatgpt

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/scheduler"
)

func TestSwitchMessageErrorClassifiesModelCapacity(t *testing.T) {
	err := switchMessageError("⚠ Selected model is at capacity. Please try a different model.")
	if err == nil {
		t.Fatal("expected capacity message to be intercepted")
	}
	if !strings.Contains(err.Error(), "upstream capacity:") {
		t.Fatalf("expected capacity prefix, got %q", err.Error())
	}
	if got := scheduler.ClassifyError(0, "", err); got != domain.ErrRateLimited {
		t.Fatalf("expected rate limited class, got %s", got)
	}
}

func TestSwitchMessageErrorClassifiesUsageLimit(t *testing.T) {
	err := switchMessageError("You've hit your usage limit. Please try again later.")
	if err == nil {
		t.Fatal("expected usage limit message to be intercepted")
	}
	if !strings.Contains(err.Error(), "upstream quota:") {
		t.Fatalf("expected quota prefix, got %q", err.Error())
	}
	if got := scheduler.ClassifyError(0, "", err); got != domain.ErrQuotaExhausted {
		t.Fatalf("expected quota exhausted class, got %s", got)
	}
}

func TestStreamResponsesSSEInterceptsModelCapacityText(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"⚠ Selected model is at capacity. Please try a different model.\"}\n\n"
	out := make(chan ir.Event, 4)

	streamResponsesSSE(context.Background(), io.NopCloser(strings.NewReader(body)), out)

	ev, ok := <-out
	if !ok {
		t.Fatal("expected intercepted error event")
	}
	if ev.Kind != ir.EvError {
		t.Fatalf("expected EvError, got %v", ev.Kind)
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "upstream capacity:") {
		t.Fatalf("expected capacity error, got %v", ev.Err)
	}
}
