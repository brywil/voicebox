package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The preflight guard (guard_server.py) scores each user message for "Is this statement nonsense?" and, at or
// above the threshold, a short note is appended to that message before the model sees it. Measured, gated at 0.5:
// BullshitBench 0.86 -> ~0.90 on Ornith-1.5 and 0.22 -> ~0.43 on Kolibri-1, for ~0.01-0.03 on sound questions,
// ~60 ms per message. Square brackets, not an XML tag: Kolibri talked about a "<preflight>" tag in 45% of its
// replies, the bracketed note in 4%; Ornith did neither much.
//
// THE NOTE MUST BE RE-APPLIED ON EVERY LATER TURN. The page sends history without it, so adding it only to the
// newest message would make the next request differ from the last one at that message -- and on a hybrid model
// (Ornith, Kolibri) any edit to the context forces a full re-prefill of the whole conversation. Decisions are
// therefore remembered by message text and replayed, so the prefix the model sees stays byte-identical.
//
// Decisions are kept in an append-only file beside the sessions and loaded at startup, never evicted: a message is
// checked exactly once, ever. Re-checking after a restart could flip a decision sitting near the threshold (batched
// numerics differ slightly with batch shape), and a flipped decision is a changed prefix -- the full re-prefill this
// design exists to avoid.
//
// Advisory and fail-open: if the guard is slow or down, the request goes ahead unchanged.

const (
	defaultGuardNote      = "[Note: this may rest on a nonsensical premise.]"
	defaultGuardThreshold = 0.5
	defaultGuardTimeout   = 1500 * time.Millisecond
)

type GuardConfig struct {
	URL       string  `json:"url"`        // e.g. http://127.0.0.1:8124; empty disables the guard
	Threshold float64 `json:"threshold"`  // default 0.5
	Note      string  `json:"note"`       // default defaultGuardNote
	TimeoutMS int     `json:"timeout_ms"` // default 1500
}

type Guard struct {
	cfg    GuardConfig
	client *http.Client
	mu     sync.Mutex
	seen   map[[32]byte]float64 // message text -> P(nonsense)
	state  string               // append-only decision log; "" keeps decisions in memory only
}

// NewGuard returns nil when no guard is configured. state is the decision log path ("" = memory only).
func NewGuard(cfg GuardConfig, state string) *Guard {
	if cfg.URL == "" {
		return nil
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultGuardThreshold
	}
	if cfg.Note == "" {
		cfg.Note = defaultGuardNote
	}
	t := defaultGuardTimeout
	if cfg.TimeoutMS > 0 {
		t = time.Duration(cfg.TimeoutMS) * time.Millisecond
	}
	g := &Guard{cfg: cfg, client: &http.Client{Timeout: t}, seen: map[[32]byte]float64{}, state: state}
	g.load()
	return g
}

type guardRecord struct {
	K string  `json:"k"` // hex sha256 of the message text as sent
	P float64 `json:"p"`
}

func (g *Guard) load() {
	if g.state == "" {
		return
	}
	data, err := os.ReadFile(g.state)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		var r guardRecord
		var k [32]byte
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if b, err := hex.DecodeString(r.K); err == nil && len(b) == len(k) {
			copy(k[:], b)
			g.seen[k] = r.P
		}
	}
	log.Printf("[guard] %d earlier decisions loaded from %s", len(g.seen), g.state)
}

// record appends new decisions. A failed write is logged, not fatal: the decision still holds in memory.
func (g *Guard) record(keys [][32]byte, ps []float64) {
	if g.state == "" {
		return
	}
	var b strings.Builder
	for i, k := range keys {
		line, _ := json.Marshal(guardRecord{hex.EncodeToString(k[:]), ps[i]})
		b.Write(line)
		b.WriteByte('\n')
	}
	_ = os.MkdirAll(filepath.Dir(g.state), 0o700)
	f, err := os.OpenFile(g.state, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.WriteString(b.String())
		f.Close()
	}
	if err != nil {
		log.Printf("[guard] could not record decisions: %v", err)
	}
}

// guardText is what the guard reads: the message without voicebox's own [voice] marker.
func guardText(s string) string { return strings.TrimPrefix(s, "[voice] ") }

// userText returns a user message's text and a setter that writes new text back into the same message, for
// both content shapes the OpenAI schema allows (a string, or an array of parts).
func userText(m map[string]any) (string, func(string), bool) {
	if m["role"] != "user" {
		return "", nil, false
	}
	switch c := m["content"].(type) {
	case string:
		return c, func(s string) { m["content"] = s }, true
	case []any:
		for i := len(c) - 1; i >= 0; i-- {
			if p, ok := c[i].(map[string]any); ok && p["type"] == "text" {
				if s, ok := p["text"].(string); ok {
					return s, func(s string) { p["text"] = s }, true
				}
			}
		}
	}
	return "", nil, false
}

// Apply checks the newest user message if it has no decision yet, then appends the note to every user message
// whose recorded decision is at or above the threshold -- so a note given once is given again, identically, on
// every later turn. It returns the newest message's probability and whether it got a note. On a guard failure
// the messages are left unchanged.
func (g *Guard) Apply(ctx context.Context, messages []any) (p float64, fired bool, err error) {
	type ref struct {
		key  [32]byte
		text string
		set  func(string)
	}
	var refs []ref
	for _, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if s, set, ok := userText(mm); ok && s != "" {
			refs = append(refs, ref{sha256.Sum256([]byte(s)), s, set})
		}
	}
	if len(refs) == 0 {
		return 0, false, nil
	}

	// Only the NEWEST message is ever checked. An older message with no recorded decision (history from before
	// the guard was deployed, or a lost state file) is recorded as exempt WITHOUT a check: the model already saw it
	// without a note, and giving it one now would change the prefix and re-prefill the whole conversation.
	newest := refs[len(refs)-1]
	g.mu.Lock()
	var exempt [][32]byte
	for _, r := range refs[:len(refs)-1] {
		if _, ok := g.seen[r.key]; !ok {
			g.seen[r.key] = -1
			exempt = append(exempt, r.key)
		}
	}
	if len(exempt) > 0 {
		ps := make([]float64, len(exempt))
		for i := range ps {
			ps[i] = -1
		}
		g.record(exempt, ps)
	}
	_, known := g.seen[newest.key]
	g.mu.Unlock()

	if !known {
		var ps []float64
		if ps, err = g.check(ctx, []string{guardText(newest.text)}); err == nil {
			g.mu.Lock()
			if _, dup := g.seen[newest.key]; !dup { // a concurrent request may have decided it meanwhile: first wins
				g.seen[newest.key] = ps[0]
				g.record([][32]byte{newest.key}, ps)
			}
			g.mu.Unlock()
		}
		// On failure, fail open for THIS message only: nothing is recorded, so next turn it is "older and unseen"
		// -> exempt, matching what the model is about to see. Earlier notes below are still re-applied -- skipping
		// them on a guard hiccup would change the prefix, the very cost this design avoids.
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range refs {
		q, ok := g.seen[r.key]
		if ok && q >= g.cfg.Threshold {
			r.set(r.text + "\n\n" + g.cfg.Note)
		}
		if i == len(refs)-1 {
			p, fired = q, ok && q >= g.cfg.Threshold
		}
	}
	return p, fired, err
}

func (g *Guard) check(ctx context.Context, texts []string) ([]float64, error) {
	body, _ := json.Marshal(map[string]any{"texts": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.cfg.URL, "/")+"/check", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		P []float64 `json:"p"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("guard: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.P) != len(texts) {
		return nil, fmt.Errorf("guard: %d answers for %d texts", len(out.P), len(texts))
	}
	return out.P, nil
}

// guardRequest runs the guard over raw["messages"] in place. It returns the notice to show the user when the
// newest message fired, or "" -- including when the guard is off or failed (logged, never fatal).
func (h *voicebox) guardRequest(ctx context.Context, raw map[string]any) string {
	if h.guard == nil {
		return ""
	}
	msgs, _ := raw["messages"].([]any)
	t0 := time.Now()
	p, fired, err := h.guard.Apply(ctx, msgs)
	if err != nil {
		log.Printf("[guard] skipped (%v)", err)
		return ""
	}
	log.Printf("[guard] p=%.3f fired=%v (%d ms)", p, fired, time.Since(t0).Milliseconds())
	if !fired {
		return ""
	}
	return fmt.Sprintf("guard: this may rest on a nonsensical premise (p %.2f) — note added", p)
}
