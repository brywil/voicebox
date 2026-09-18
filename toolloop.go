package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// The agentic loop.
//
// A transparent proxy cannot do this: tool calling is a CONVERSATION with the
// model -- ask, receive a call, run it, tell it the answer, ask again -- and
// each round is a fresh upstream request carrying the accumulated messages. So
// /v1/chat/completions is handled rather than forwarded whenever tools are in
// play.
//
// The browser still sees one continuous SSE stream. Content and reasoning
// deltas pass through untouched; tool activity is added as extra frames the page
// recognises and the model never sees. That matters more on a voice interface
// than a visual one: a silent thirty-second gap while something searches the web
// is indistinguishable from a hang when you cannot see a spinner.

const defaultMaxRounds = 4

type chatRequest struct {
	Model    string           `json:"model"`
	Stream   bool             `json:"stream"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools,omitempty"`
	raw      map[string]any   // everything else, preserved verbatim
}

// toolCall is accumulated across streamed deltas. Arguments arrive as partial
// JSON fragments that only parse once concatenated.
type toolCall struct {
	ID   string
	Name string
	Args strings.Builder
}

func (h *voicebox) handleChat(w http.ResponseWriter, r *http.Request) {
	backendID := r.Header.Get("X-Voicebox-Backend")
	if backendID == "" {
		backendID = h.cfg.Default
	}
	b := h.cfg.find(backendID)
	if b == nil {
		http.Error(w, "unknown backend "+backendID, http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Tools only when the client asked AND a tool server is configured. Without
	// both, this is a plain proxy and behaves exactly as before.
	wantTools, _ := raw["voicebox_tools"].(bool)
	delete(raw, "voicebox_tools") // ours, not the model's
	if !wantTools || h.mcp == nil {
		h.proxyChat(w, r, b, body)
		return
	}

	tools, err := h.mcp.Tools(r.Context())
	if err != nil {
		log.Printf("[tools] catalog unavailable (%v); continuing without tools", err)
		h.proxyChat(w, r, b, body)
		return
	}
	raw["tools"] = OpenAITools(tools)

	msgs, _ := raw["messages"].([]any)
	messages := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			messages = append(messages, mm)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	maxRounds := h.cfg.MCP.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}

	for round := 1; ; round++ {
		raw["messages"] = messages
		raw["stream"] = true

		calls, finish, err := h.streamRound(r.Context(), b, raw, w, flusher)
		if err != nil {
			emitEvent(w, flusher, map[string]any{"error": err.Error()})
			break
		}
		if len(calls) == 0 || finish != "tool_calls" {
			break
		}
		if round >= maxRounds {
			// Say so rather than stopping silently: an answer that quietly used
			// fewer tools than it wanted is worse than one that admits it.
			emitEvent(w, flusher, map[string]any{"notice": fmt.Sprintf(
				"stopped after %d rounds of tool calls", maxRounds)})
			break
		}

		// Record what the model asked for, exactly as the API expects it back.
		tcs := make([]map[string]any, 0, len(calls))
		for _, c := range calls {
			tcs = append(tcs, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Args.String()},
			})
		}
		messages = append(messages, map[string]any{
			"role": "assistant", "content": nil, "tool_calls": tcs,
		})

		for _, c := range calls {
			args := map[string]any{}
			if s := strings.TrimSpace(c.Args.String()); s != "" && s != "null" {
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					log.Printf("[tools] %s: unparseable arguments %q", c.Name, trim(s, 120))
				}
			}
			emitEvent(w, flusher, map[string]any{
				"tool": map[string]any{"phase": "start", "name": c.Name, "args": args},
			})
			out, ok := h.mcp.Call(r.Context(), c.Name, args)
			log.Printf("[tools] %s ok=%v (%d bytes)", c.Name, ok, len(out))
			emitEvent(w, flusher, map[string]any{
				"tool": map[string]any{"phase": "done", "name": c.Name, "ok": ok,
					"summary": firstLineOf(out, 160)},
			})
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": c.ID, "name": c.Name, "content": out,
			})
		}
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// streamRound sends one upstream request, forwards every content/reasoning
// delta to the browser verbatim, and returns any tool calls it accumulated.
func (h *voicebox) streamRound(ctx context.Context, b *Backend, payload map[string]any,
	w http.ResponseWriter, flusher http.Flusher) ([]*toolCall, string, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.URL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKeyEnv != "" {
		if k := envOf(b.APIKeyEnv); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := h.upstream.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("backend %s: HTTP %d: %s", b.ID, resp.StatusCode, trim(string(msg), 300))
	}

	byIndex := map[int]*toolCall{}
	var order []int
	finish := ""

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			continue // the loop decides when the browser's stream ends
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // a frame we do not understand is not a reason to stop
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			finish = *ch.FinishReason
		}
		// Tool-call deltas are ACCUMULATED, not forwarded: a half-built function
		// name rendered into the transcript would be noise, and spoken aloud it
		// would be gibberish.
		for _, tc := range ch.Delta.ToolCalls {
			c := byIndex[tc.Index]
			if c == nil {
				c = &toolCall{}
				byIndex[tc.Index] = c
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				c.ID = tc.ID
			}
			if tc.Function.Name != "" {
				c.Name = tc.Function.Name
			}
			c.Args.WriteString(tc.Function.Arguments)
		}
		// Everything the user should see or hear goes straight through, so the
		// page's existing parser needs no special case.
		if ch.Delta.Content != nil || ch.Delta.Reasoning != nil {
			_, _ = io.WriteString(w, "data: "+payload+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, finish, fmt.Errorf("reading upstream stream: %w", err)
	}

	calls := make([]*toolCall, 0, len(order))
	for _, i := range order {
		if c := byIndex[i]; c != nil && c.Name != "" {
			if c.ID == "" {
				c.ID = fmt.Sprintf("call_%d", i) // some servers omit it
			}
			calls = append(calls, c)
		}
	}
	return calls, finish, nil
}

// emitEvent sends a voicebox-only SSE frame. It is shaped like a chat chunk so
// a client that does not know about it simply sees a delta with no content.
func emitEvent(w http.ResponseWriter, flusher http.Flusher, v map[string]any) {
	v["object"] = "voicebox.event"
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// proxyChat is the untouched path: no tools, no parsing, just the reverse proxy.
func (h *voicebox) proxyChat(w http.ResponseWriter, r *http.Request, b *Backend, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	b.proxy.ServeHTTP(w, r)
}

func firstLineOf(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trim(strings.TrimSpace(s), n)
}
