package openai

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// With GEMINI_MAX_RETRIES<=1 the tool bridge sends one generation and JSON mode
// sends no repair generation. Each test counts generate calls, not replies.

func TestSingleAttemptToolBridgeSendsOneGeneration(t *testing.T) {
	fake := &fakeGemini{replies: []string{"Sure, here is a one-sentence summary."}}
	service := newToolBridgeTestService(t, fake)
	service.singleAttempt = true

	_, err := service.CreateChatCompletion(context.Background(), doneToolRequest(t, forcedDone))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("Gemini was called %d times, want 1", len(fake.prompts))
	}
}

func TestSingleAttemptJSONModeSkipsRepairGeneration(t *testing.T) {
	fake := &fakeGemini{replies: []string{
		"Here is the person: {\"name\":\"Ada\"}",
		"```json\n" + personJSON + "\n```",
	}}
	service := newJSONModeTestService(t, fake, zap.NewNop())
	service.singleAttempt = true

	_, _ = service.CreateChatCompletion(context.Background(), jsonModeRequest(t, jsonSchemaPersonRequestJSON))
	if len(fake.prompts) != 1 {
		t.Fatalf("Gemini was called %d times, want 1 (no repair generation)", len(fake.prompts))
	}
}

func TestDefaultRetriesKeepToolBridgeAttempts(t *testing.T) {
	fake := &fakeGemini{replies: []string{"I cannot call tools, but the answer is 42."}}
	service := newToolBridgeTestService(t, fake)

	_, err := service.CreateChatCompletion(context.Background(), doneToolRequest(t, forcedDone))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
	if len(fake.prompts) != maxToolBridgeAttempts {
		t.Fatalf("Gemini was called %d times, want %d", len(fake.prompts), maxToolBridgeAttempts)
	}
}
