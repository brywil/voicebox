package main

import (
	"strings"
	"testing"
	"time"
)

var promptNow = time.Date(2026, 10, 16, 14, 5, 0, 0, time.Local)

func TestSystemPromptSpokenVsText(t *testing.T) {
	sp := systemPrompt(true, false, promptNow)
	if !strings.Contains(sp, "read aloud") || !strings.Contains(sp, "Never use markdown") {
		t.Fatalf("spoken prompt lacks the spoken rules:\n%s", sp)
	}
	tx := systemPrompt(false, false, promptNow)
	if strings.Contains(tx, "read aloud") || strings.Contains(tx, "markdown") {
		t.Fatalf("text prompt carries spoken rules:\n%s", tx)
	}
	for _, p := range []string{sp, tx} {
		if !strings.Contains(p, "Never make up facts") {
			t.Errorf("prompt lost the honesty rule:\n%s", p)
		}
		if !strings.Contains(p, "Friday, October 16, 2026, 2:05 PM") {
			t.Errorf("prompt lacks the local time:\n%s", p)
		}
		if strings.Contains(p, "[voice]") {
			t.Errorf("voice rule present without voice input:\n%s", p)
		}
	}
	if !strings.Contains(sp, "may be dictating") {
		t.Errorf("spoken prompt lacks the dictation rule (Gboard input is untagged):\n%s", sp)
	}
	if strings.Contains(tx, "dictating") {
		t.Errorf("text prompt carries the dictation rule:\n%s", tx)
	}
	if both := systemPrompt(true, true, promptNow); strings.Contains(both, "may be dictating") {
		t.Errorf("tagged voice input should get the measured [voice] rule only, not both:\n%s", both)
	}
}

func TestApplySystemPromptFoldsAndOrders(t *testing.T) {
	raw := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Notes from earlier in this conversation:\nwe chose B"},
		map[string]any{"role": "user", "content": "[voice] what was the plan"},
		map[string]any{"role": "assistant", "content": "B"},
		map[string]any{"role": "system", "content": "  "},
		map[string]any{"role": "user", "content": "and then?"},
	}}
	applySystemPrompt(raw, true, promptNow)
	msgs := raw["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 1 system + 3 turns, got %d messages", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("first message is %v, not system", sys["role"])
	}
	for i, m := range msgs[1:] {
		if m.(map[string]any)["role"] == "system" {
			t.Errorf("system message left at position %d; some chat templates reject that", i+1)
		}
	}
	s := sys["content"].(string)
	if !strings.Contains(s, "we chose B") {
		t.Errorf("client's summary note was dropped:\n%s", s)
	}
	if !strings.Contains(s, "[voice] was dictated") {
		t.Errorf("a [voice] message did not add the voice rule:\n%s", s)
	}
	if strings.Contains(s, "[typed]") {
		t.Errorf("prompt mentions [typed]; measured to make granite echo it:\n%s", s)
	}
	if msgs[1].(map[string]any)["content"] != "[voice] what was the plan" {
		t.Errorf("user turn was altered: %v", msgs[1])
	}
}

func TestApplySystemPromptNoMessages(t *testing.T) {
	raw := map[string]any{}
	applySystemPrompt(raw, false, promptNow)
	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("want exactly the system prompt, got %v", msgs)
	}
}
