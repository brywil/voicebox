package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// A minimal MCP client: initialize, tools/list, tools/call over Streamable HTTP.
//
// It lives in the proxy rather than the browser for two reasons that are not
// negotiable: mymcp is loopback-bound, so a page could not reach it at all, and
// the bearer token must not exist in a document that anything on the tailnet can
// read. The browser sees tool ACTIVITY; it never sees the tool server.

type MCPConfig struct {
	URL       string `json:"url"`        // e.g. http://127.0.0.1:9445/mcp
	TokenFile string `json:"token_file"` // file containing the bearer token, mode 600
	// MaxRounds bounds the agentic loop. A model that keeps calling tools
	// forever is a real failure mode, and on a voice interface it is an
	// expensive one -- every round is latency the user hears as silence.
	MaxRounds int `json:"max_rounds"`
}

type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type MCPClient struct {
	url   string
	token string
	http  *http.Client

	mu     sync.Mutex
	tools  []MCPTool
	loaded bool
}

func NewMCPClient(cfg MCPConfig) (*MCPClient, error) {
	if cfg.URL == "" {
		return nil, nil // not configured; tool calling stays off
	}
	tok := ""
	if cfg.TokenFile != "" {
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("mcp token_file: %w", err)
		}
		tok = strings.TrimSpace(string(b))
	}
	return &MCPClient{
		url:   cfg.URL,
		token: tok,
		// Generous: a web search goes out to the network and back. Too short a
		// timeout turns a slow search into a mysterious tool failure.
		http: &http.Client{Timeout: 90 * time.Second},
	}, nil
}

func (c *MCPClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", method, resp.StatusCode, trim(string(raw), 200))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("mcp %s: bad response: %w", method, err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s (code %d)", method, env.Error.Message, env.Error.Code)
	}
	return env.Result, nil
}

// Tools lists the catalog, cached after the first call. The catalog is fixed at
// the server's start -- it is a whitelist in a unit file -- so re-asking per
// turn would be pure latency.
func (c *MCPClient) Tools(ctx context.Context) ([]MCPTool, error) {
	c.mu.Lock()
	if c.loaded {
		defer c.mu.Unlock()
		return c.tools, nil
	}
	c.mu.Unlock()

	if _, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "voicebox", "version": "1"},
	}); err != nil {
		return nil, err
	}
	res, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tools, c.loaded = out.Tools, true
	c.mu.Unlock()
	return out.Tools, nil
}

// Call runs one tool and returns its text result. A tool that fails returns its
// error AS TEXT with ok=false rather than an error: the model is the one that
// has to recover, and it can only do that if it is told what went wrong in the
// same channel it asked the question.
func (c *MCPClient) Call(ctx context.Context, name string, args map[string]any) (string, bool) {
	res, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "tool call failed: " + err.Error(), false
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "tool returned an unreadable result: " + err.Error(), false
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Text != "" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), !out.IsError
}

// OpenAITools renders the catalog in the shape chat completions expects.
func OpenAITools(ts []MCPTool) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": schema,
			},
		})
	}
	return out
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
