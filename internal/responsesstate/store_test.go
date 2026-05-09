package responsesstate

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLookupAndRecord(t *testing.T) {
	s := New(10, time.Minute)
	s.Record("resp-1", "acc-1", []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user"}`),
	})
	got, ok := s.Lookup("resp-1")
	if !ok {
		t.Fatal("expected lookup hit")
	}
	if got.AccountID != "acc-1" {
		t.Fatalf("unexpected account: %s", got.AccountID)
	}
	if len(got.Transcript) != 1 {
		t.Fatalf("unexpected transcript length: %d", len(got.Transcript))
	}
}

func TestLookupExpires(t *testing.T) {
	s := New(10, time.Millisecond)
	s.Record("resp-1", "acc-1", []json.RawMessage{json.RawMessage(`{"type":"message"}`)})
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.Lookup("resp-1"); ok {
		t.Fatal("expected expired session")
	}
}

func TestRecordClonesTranscript(t *testing.T) {
	s := New(10, time.Minute)
	src := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user"}`)}
	s.Record("resp-1", "acc-1", src)
	src[0][0] = '['
	got, ok := s.Lookup("resp-1")
	if !ok {
		t.Fatal("expected lookup hit")
	}
	if string(got.Transcript[0]) != `{"type":"message","role":"user"}` {
		t.Fatalf("transcript mutated: %s", got.Transcript[0])
	}
}

func TestByteLimitEvictsOldSessionsWithoutTruncatingCurrent(t *testing.T) {
	s := NewWithByteLimit(10, time.Minute, 40)
	long := json.RawMessage(`{"type":"message","content":[{"type":"output_text","text":"long-code-and-reasoning-chain"}]}`)
	s.Record("old-1", "acc-1", []json.RawMessage{json.RawMessage(`{"text":"old-a"}`)})
	s.Record("old-2", "acc-1", []json.RawMessage{json.RawMessage(`{"text":"old-b"}`)})
	s.Record("current", "acc-1", []json.RawMessage{long})

	got, ok := s.Lookup("current")
	if !ok {
		t.Fatal("current long session should be retained")
	}
	if len(got.Transcript) != 1 || string(got.Transcript[0]) != string(long) {
		t.Fatalf("current transcript was truncated or mutated: %v", got.Transcript)
	}
}

func TestByteLimitDropsOldTranscriptButKeepsAccountMapping(t *testing.T) {
	s := NewWithByteLimit(10, time.Minute, 64)
	oldLong := json.RawMessage(`{"type":"message","content":[{"type":"output_text","text":"old-long-code-and-reasoning-chain"}]}`)
	newLong := json.RawMessage(`{"type":"message","content":[{"type":"output_text","text":"new-long-code-and-reasoning-chain"}]}`)
	s.Record("old", "acc-old", []json.RawMessage{oldLong})
	s.Record("new", "acc-new", []json.RawMessage{newLong})

	gotOld, ok := s.Lookup("old")
	if !ok {
		t.Fatal("old response id should remain as lightweight account affinity")
	}
	if gotOld.AccountID != "acc-old" {
		t.Fatalf("old account mapping changed: %s", gotOld.AccountID)
	}
	if len(gotOld.Transcript) != 0 {
		t.Fatalf("old transcript should be released under byte pressure, got %d items", len(gotOld.Transcript))
	}

	gotNew, ok := s.Lookup("new")
	if !ok {
		t.Fatal("new response id should remain")
	}
	if len(gotNew.Transcript) != 1 || string(gotNew.Transcript[0]) != string(newLong) {
		t.Fatalf("new transcript should not be truncated: %v", gotNew.Transcript)
	}
}

func TestRecordAccountStoresAffinityWithoutTranscript(t *testing.T) {
	s := NewWithByteLimit(10, time.Minute, 64)
	s.RecordAccount("resp", "acc")

	got, ok := s.Lookup("resp")
	if !ok {
		t.Fatal("expected lightweight account mapping")
	}
	if got.AccountID != "acc" {
		t.Fatalf("unexpected account: %s", got.AccountID)
	}
	if len(got.Transcript) != 0 || got.Bytes != 0 {
		t.Fatalf("record account should not retain transcript bytes: %+v", got)
	}
}

func TestRecordThreadStoresAccountAffinity(t *testing.T) {
	s := New(10, time.Minute)
	s.RecordThread("tenant/group/provider/thread", "acc-1")

	got, ok := s.LookupThread("tenant/group/provider/thread")
	if !ok {
		t.Fatal("expected thread affinity")
	}
	if got.AccountID != "acc-1" {
		t.Fatalf("unexpected account: %s", got.AccountID)
	}

	s.RecordThread("tenant/group/provider/thread", "acc-2")
	got, ok = s.LookupThread("tenant/group/provider/thread")
	if !ok {
		t.Fatal("expected updated thread affinity")
	}
	if got.AccountID != "acc-2" {
		t.Fatalf("thread affinity was not updated: %s", got.AccountID)
	}
}

func TestRecordWithThreadStoresReplayTranscript(t *testing.T) {
	s := New(10, time.Minute)
	transcript := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	}
	s.RecordWithThread("resp-1", "acc-1", "thread", transcript)

	byID, ok := s.Lookup("resp-1")
	if !ok {
		t.Fatal("expected response id lookup")
	}
	if byID.ThreadKey != "thread" || byID.AccountID != "acc-1" {
		t.Fatalf("unexpected response session: %+v", byID)
	}
	if len(byID.Transcript) != len(transcript) {
		t.Fatalf("response id lookup should recover thread transcript, got %d items", len(byID.Transcript))
	}

	byThread, ok := s.LookupThreadTranscript("thread")
	if !ok {
		t.Fatal("expected thread transcript")
	}
	if byThread.ResponseID != "resp-1" || byThread.AccountID != "acc-1" {
		t.Fatalf("unexpected thread session: %+v", byThread)
	}
	if string(byThread.Transcript[1]) != string(transcript[1]) {
		t.Fatalf("thread transcript changed: %s", byThread.Transcript[1])
	}
}

func TestRecordThreadClearsStaleReplayTranscript(t *testing.T) {
	s := New(10, time.Minute)
	s.RecordWithThread("resp-1", "acc-1", "thread", []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user"}`),
	})
	s.RecordThread("thread", "acc-1")

	if _, ok := s.LookupThreadTranscript("thread"); ok {
		t.Fatal("account-only thread update should clear stale replay transcript")
	}
	got, ok := s.LookupThread("thread")
	if !ok {
		t.Fatal("expected lightweight thread affinity")
	}
	if got.AccountID != "acc-1" || got.ResponseID != "" {
		t.Fatalf("unexpected lightweight thread affinity: %+v", got)
	}
}

func TestLookupThreadExpires(t *testing.T) {
	s := New(10, time.Millisecond)
	s.RecordThread("thread", "acc")
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.LookupThread("thread"); ok {
		t.Fatal("expected expired thread affinity")
	}
}

func TestThreadAffinityEvictionIsSeparateFromTranscriptBytes(t *testing.T) {
	s := NewWithByteLimit(1, time.Minute, 10)
	s.Record("resp", "acc-1", []json.RawMessage{json.RawMessage(`{"text":"long-transcript"}`)})
	s.RecordThread("old-thread", "acc-1")
	s.RecordThread("new-thread", "acc-2")

	if _, ok := s.LookupThread("old-thread"); ok {
		t.Fatal("old thread affinity should be evicted by thread limit")
	}
	got, ok := s.LookupThread("new-thread")
	if !ok {
		t.Fatal("new thread affinity should remain")
	}
	if got.AccountID != "acc-2" {
		t.Fatalf("unexpected thread account: %s", got.AccountID)
	}

	resp, ok := s.Lookup("resp")
	if !ok || resp.AccountID != "acc-1" {
		t.Fatalf("response id affinity should not be evicted by thread records: ok=%v resp=%+v", ok, resp)
	}
}

func TestSystemPromptRequirementClearedByPromptAwareThreadRecord(t *testing.T) {
	s := New(10, time.Minute)
	s.RecordWithThreadPrompt("resp-1", "acc-1", "thread", []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user"}`),
	}, true)

	thread, ok := s.LookupThread("thread")
	if !ok {
		t.Fatal("expected thread")
	}
	if !thread.SystemPromptInjected {
		t.Fatal("thread should remember that group system prompt was injected")
	}

	s.RequireSystemPromptForThread("thread")
	thread, ok = s.LookupThread("thread")
	if !ok || !thread.RequireSystemPrompt {
		t.Fatalf("thread should require prompt after compact: ok=%v thread=%+v", ok, thread)
	}

	s.RecordThreadPrompt("thread", "acc-1", true)
	thread, ok = s.LookupThread("thread")
	if !ok {
		t.Fatal("expected thread after prompt-aware record")
	}
	if thread.RequireSystemPrompt {
		t.Fatal("successful prompt-aware record should clear one-shot requirement")
	}
	if !thread.SystemPromptInjected {
		t.Fatal("prompt-aware record should preserve injected state")
	}
}
