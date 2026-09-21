package main

// Live context-window detection for llama.cpp backends.
//
// Compaction needs to know how much context it is working against, and an earlier version of
// this file did not exist because I assumed the window had to be configured by hand. It does
// not: llama.cpp reports it, and openclaw-go (internal/llama/client.go, ModelSelfInfo) already
// detects it this way. This mirrors that rather than inventing a second method.
//
// TWO THINGS THAT ARE EASY TO GET WRONG, both learned from that code:
//
//   /props IS AT THE ROOT, NOT UNDER /v1. A backend URL points at the OpenAI-compatible surface,
//   so the naive base+"/props" becomes ".../v1/props" and 404s. The /v1 suffix has to be
//   stripped first.
//
//   THE FIELD IS default_generation_settings.n_ctx, AND IT IS ALREADY DIVIDED BY --parallel.
//   There is no top-level n_ctx to reach for. MEASURED on this box rather than assumed, because
//   getting it wrong is silent in both directions:
//
//       llama-server --ctx-size 8192 --parallel 4
//         default_generation_settings.n_ctx = 2048     <- 8192/4, per SLOT
//         total_slots                       = 4
//         server log: n_slots = 4, n_ctx_slot = 2048
//
//   So this value is exactly what to plan against: a conversation occupies one slot and 2048 is
//   genuinely all it gets. DO NOT divide it by total_slots -- that division is already done, and
//   doing it again would give 512 here and compact four times too aggressively. That is the
//   error worth guarding, because over-compaction just looks like a model with a poor memory.
//
// Backends that are not llama.cpp (ollama, cloud APIs) simply 404 or time out, and the probe
// returns 0 -- meaning "unknown", which leaves compaction off for that backend rather than
// guessing a window.

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ctxCache holds probed context sizes. A TTL rather than a one-shot probe at startup because
// llama-server gets restarted with different models and different -c values all the time; a
// value cached forever would eventually describe a model that is no longer loaded.
type ctxCache struct {
	mu   sync.Mutex
	vals map[string]ctxEntry
}

type ctxEntry struct {
	n  int
	at time.Time
}

const ctxTTL = 5 * time.Minute

var probedCtx = &ctxCache{vals: map[string]ctxEntry{}}

// probeContext asks a llama.cpp server for its live per-slot context size. Returns 0 for
// anything that is not a llama.cpp server, or that cannot be reached.
func probeContext(baseURL string) int {
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	root = strings.TrimRight(root, "/")

	// Short timeout: this runs on the path of deciding whether to compact, and a backend that
	// is slow to answer a metadata call should not delay that. Unknown is a safe answer.
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(root + "/props")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()
	var p struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return 0
	}
	return p.DefaultGenerationSettings.NCtx
}

// contextFor returns the window to plan against: the configured value if there is one,
// otherwise a probed and cached one.
//
// Config WINS over the probe, deliberately. A probe reports what the server will accept, which
// is not always what you want to fill -- a shared llama-server, or one you would rather not
// drive to its limit, is a reason to set a smaller number by hand and have it respected.
func (h *voicebox) contextFor(b *Backend) int {
	if b == nil {
		return 0
	}
	if b.Context > 0 {
		return b.Context
	}
	probedCtx.mu.Lock()
	e, ok := probedCtx.vals[b.ID]
	probedCtx.mu.Unlock()
	if ok && time.Since(e.at) < ctxTTL {
		return e.n
	}
	n := probeContext(b.URL)
	probedCtx.mu.Lock()
	probedCtx.vals[b.ID] = ctxEntry{n: n, at: time.Now()}
	probedCtx.mu.Unlock()
	if n > 0 {
		log.Printf("[compact] backend %s reports %d-token context", b.ID, n)
	}
	return n
}
