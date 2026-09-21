package main

// Live context-window detection.
//
// Compaction has to know how much context it is planning against, and that number MUST NOT be
// maintained by hand: a config value silently drifts from whatever the server was last
// restarted with, and the whole failure mode here is silent. So it is probed, per backend and
// per model, from whatever the backend is willing to say.
//
// THE ONE RULE THAT MATTERS MORE THAN THE LADDER: never read the model's ARCHITECTURAL
// maximum. Every backend family publishes one, it looks authoritative, and it is wrong in the
// dangerous direction. MEASURED on this box:
//
//     llama.cpp  /v1/models   meta.n_ctx       = 4096     <- what is actually served
//                             meta.n_ctx_train = 131072   <- 32x too large
//     ollama     /api/show    gemma4.context_length = 262144  <- the model's max; ollama serves
//                                                                far less unless told otherwise
//
// Using the maximum means the budget is never reached, compaction never fires, and the backend
// quietly drops the front of the conversation -- exactly the failure compaction exists to
// prevent, made worse by appearing to be configured correctly.
//
// THE LADDER, most authoritative first:
//
//   1. /v1/models -> data[].meta.n_ctx        llama.cpp, and the endpoint every OpenAI-
//                                             compatible backend already exposes. Per-model,
//                                             and already divided by --parallel.
//   2. /props -> default_generation_settings.n_ctx
//                                             llama.cpp's own endpoint. Same value; kept
//                                             because older builds lack the meta block in
//                                             /v1/models. NOTE it lives at the ROOT -- a
//                                             backend URL points at the OpenAI surface, so
//                                             base+"/props" becomes ".../v1/props" and 404s.
//   3. /api/ps -> models[].context_length     ollama, and only while the model is LOADED --
//                                             which is the point: it is the context ollama is
//                                             actually serving, not the one the model could
//                                             support. (`ollama ps` shows this as its CONTEXT
//                                             column.)
//
// Anything else returns 0, meaning unknown, which leaves compaction OFF for that backend. That
// is deliberate: not compacting is a known, bounded problem, while compacting against a guessed
// window is an unbounded one.

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ctxCache holds probed windows, keyed by backend AND model because the answer differs per
// model. A TTL rather than a one-shot probe at startup: llama-server is restarted with
// different models and different -c values constantly, and ollama loads and unloads, so a value
// cached for the process lifetime would eventually describe something that is no longer there.
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

func rootOf(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	return strings.TrimRight(u, "/")
}

// shortClient is used for every probe. These run on the path of deciding whether to compact,
// and a backend slow to answer a metadata call must not delay that -- "unknown" is a safe
// answer and the next turn tries again.
var shortClient = &http.Client{Timeout: 3 * time.Second}

// rootBackend returns a copy of b whose URL is the ROOT, for endpoints that do not live under
// the OpenAI surface (/props, /api/ps). Copied rather than mutated so the real backend, which
// the proxy uses, is untouched -- and it keeps APIKeyEnv, so auth still applies.
func rootBackend(b *Backend) *Backend {
	c := *b
	c.URL = rootOf(b.URL)
	return &c
}

func num(m map[string]any, keys ...string) int {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur, ok = mm[k]
		if !ok {
			return 0
		}
	}
	if f, ok := cur.(float64); ok && f > 0 {
		return int(f)
	}
	return 0
}

// ctxFromModels reads the live window out of the OpenAI-compatible model list.
//
// Preferred because it is the one endpoint every backend in this config already serves, and
// because it is per-model: a backend hosting several models does not have one context size.
// meta.n_ctx ONLY -- meta.n_ctx_train sits right beside it and is the model's training length.
func ctxFromModels(b *Backend, model string) int {
	d, err := getJSON(shortClient, b, "/v1/models", nil)
	if err != nil {
		return 0
	}
	arr, _ := d["data"].([]any)
	var single int
	for _, e := range arr {
		m, _ := e.(map[string]any)
		n := num(m, "meta", "n_ctx") // NOT n_ctx_train, which sits beside it
		if n <= 0 {
			continue
		}
		if id, _ := m["id"].(string); model != "" && id == model {
			return n
		}
		if single == 0 {
			single = n
		}
	}
	if len(arr) == 1 {
		return single // single-model server: unambiguous
	}
	return 0
}

// ctxFromProps is llama.cpp's own endpoint, at the ROOT rather than under /v1.
func ctxFromProps(b *Backend) int {
	d, err := getJSON(shortClient, rootBackend(b), "/props", nil)
	if err != nil {
		return 0
	}
	return num(d, "default_generation_settings", "n_ctx")
}

// ctxFromOllamaPS reads the context of a model ollama currently has LOADED.
//
// /api/show is deliberately not used: it reports model_info.<arch>.context_length, the model's
// architectural maximum, which ollama does not serve unless explicitly configured to. /api/ps
// reports what is actually loaded -- the same value `ollama ps` prints in its CONTEXT column --
// and returns nothing when no model is loaded, which correctly yields "unknown" rather than an
// optimistic guess.
func ctxFromOllamaPS(b *Backend, model string) int {
	d, err := getJSON(shortClient, rootBackend(b), "/api/ps", nil)
	if err != nil {
		return 0
	}
	arr, _ := d["models"].([]any)
	for _, e := range arr {
		m, _ := e.(map[string]any)
		n := num(m, "context_length")
		if n <= 0 {
			continue
		}
		name, _ := m["name"].(string)
		mdl, _ := m["model"].(string)
		if model == "" || name == model || mdl == model {
			return n
		}
	}
	return 0
}

// probeContextFor walks the ladder and returns 0 when nothing will say.
func probeContextFor(b *Backend, model string) (int, string) {
	if n := ctxFromModels(b, model); n > 0 {
		return n, "/v1/models meta.n_ctx"
	}
	if n := ctxFromProps(b); n > 0 {
		return n, "/props n_ctx"
	}
	if n := ctxFromOllamaPS(b, model); n > 0 {
		return n, "/api/ps context_length"
	}
	return 0, ""
}

// contextFor returns the window to plan against for a backend/model pair.
//
// Config still wins when set, but it is no longer the expected path -- it exists for a backend
// that reports nothing and that you would rather compact than leave unbounded. Leaving it unset
// is the norm, because a probed value cannot drift out of step with the server the way a
// written-down one does.
func (h *voicebox) contextFor(b *Backend, model string) int {
	if b == nil {
		return 0
	}
	if b.Context > 0 {
		return b.Context
	}
	key := b.ID + "\x00" + model
	probedCtx.mu.Lock()
	e, ok := probedCtx.vals[key]
	probedCtx.mu.Unlock()
	if ok && time.Since(e.at) < ctxTTL {
		return e.n
	}
	n, via := probeContextFor(b, model)
	probedCtx.mu.Lock()
	probedCtx.vals[key] = ctxEntry{n: n, at: time.Now()}
	probedCtx.mu.Unlock()
	if n > 0 {
		log.Printf("[compact] backend %s model %q: %d-token context (via %s)", b.ID, model, n, via)
	}
	return n
}
