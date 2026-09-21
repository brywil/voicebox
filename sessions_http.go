package main

// HTTP surface for server-side sessions: CRUD plus a live event stream per session.
//
// The stream is SSE rather than a WebSocket for three reasons specific to this codebase: the
// traffic is one-way (the client already POSTs its turns through /v1/chat/completions, so it
// needs no second uplink), SSE reconnects by itself and replays via Last-Event-ID, and the
// server already speaks SSE in toolloop.go -- a WebSocket would mean a dependency and a second
// framing to get wrong, in a module that currently has no dependencies at all.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// joinLimit is how many past messages a device gets when it opens a session without asking for
// a specific window. Enough to give a conversation context on a phone; not the whole transcript,
// which on a long session is megabytes a new client does not need to render.
const joinLimit = 60

func (h *voicebox) registerSessionRoutes(mux *http.ServeMux) {
	if h.store == nil {
		return // sessions disabled; every route below stays unregistered
	}

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		list, err := h.store.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	})

	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
		sess, err := h.store.Create(body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, sess)
	})

	// GET one session. `since` returns only what the caller has not seen, which is what a
	// reconnecting device asks for; `limit` caps how far back a fresh device reads.
	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		limit := joinLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n // 0 or negative means "everything", handled in Since
			}
		}
		sess, err := h.store.Get(id)
		if err != nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		msgs, head, err := h.store.Since(id, since, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"id": sess.ID, "title": sess.Title, "created": sess.Created,
			"updated": sess.Updated, "head": head, "total": len(sess.Messages),
			// summary/compacted_through are the contract for building context: a client
			// sends the summary in place of every message at or below compacted_through,
			// then the turns after it verbatim. Without both it cannot tell which turns the
			// summary already covers and would send them twice.
			"summary": sess.Summary, "compacted_through": sess.CompactedThrough,
			"messages": msgs,
			// truncated tells the client it is looking at a window rather than the whole
			// conversation, so it can offer "load earlier" instead of silently implying
			// the session began where the window does.
			"truncated": len(msgs) < len(sess.Messages),
		})
	})

	mux.HandleFunc("PATCH /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		if err := h.store.Rename(r.PathValue("id"), body.Title); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.Delete(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// Manual compaction. Auto-compaction covers normal use; this exists so the behaviour is
	// testable without waiting for a session to grow, and so a "compact now" control has
	// something to call.
	mux.HandleFunc("POST /api/sessions/{id}/compact", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Backend string `json:"backend"`
			Model   string `json:"model"`
			Keep    int    `json:"keep"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
		if body.Keep <= 0 {
			body.Keep = defaultKeepVerbatim
		}
		// Through compactGuarded, not Compact directly: inFlight exists so the sweeper and
		// the request path cannot run two summarisers over the same turns, and calling
		// Compact here would let a "compact now" press during an auto compaction do exactly
		// that. Stored state would stay consistent -- SetSummary writes summary and through
		// together -- but it wastes a backend slot and, on --parallel 1, stalls the user's
		// next turn for the measured 5 s.
		if err := h.compactGuarded(r.Context(), r.PathValue("id"), body.Backend, body.Model, body.Keep); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		sess, err := h.store.Get(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "summary": sess.Summary,
			"compacted_through": sess.CompactedThrough})
	})

	mux.HandleFunc("GET /api/sessions/{id}/events", h.handleSessionEvents)
}

// handleSessionEvents streams changes to one session.
//
// RECONNECT IS THE WHOLE POINT OF THE SEQ NUMBERS. A phone that sleeps, changes network, or is
// backgrounded will drop this connection constantly, and EventSource reconnects on its own with
// a Last-Event-ID header. Replaying everything after that id closes the gap without the client
// having to notice it existed. A device that has been away long enough to miss a lot still gets
// the whole gap -- correctness first; the join window only applies to a fresh open.
func (h *voicebox) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.store.Get(id); err != nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Without this, any buffering proxy in front of the tailnet will hold the stream until it
	// has enough bytes to bother forwarding, which looks exactly like the server being hung.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe BEFORE replaying the backlog. The other order has a hole: a turn committed
	// between the replay and the subscribe reaches neither, and the client silently misses a
	// message with no error anywhere.
	ch, release := h.store.Subscribe(id)
	defer release()

	var since int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}
	if backlog, _, err := h.store.Since(id, since, 0); err == nil && len(backlog) > 0 {
		sendEvent(w, flusher, Event{Session: id, Kind: "commit", Msgs: backlog})
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			sendEvent(w, flusher, ev)
		}
	}
}

// sendEvent writes one SSE frame. The id: line is what EventSource echoes back as
// Last-Event-ID after a reconnect, so it must be the highest seq in the frame -- anything less
// and the client re-receives messages it already has; anything more and it silently skips some.
func sendEvent(w http.ResponseWriter, flusher http.Flusher, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[sessions] marshal event: %v", err)
		return
	}
	var top int64
	for _, m := range ev.Msgs {
		if m.Seq > top {
			top = m.Seq
		}
	}
	var sb strings.Builder
	if top > 0 {
		fmt.Fprintf(&sb, "id: %d\n", top)
	}
	sb.WriteString("data: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	_, _ = io.WriteString(w, sb.String())
	flusher.Flush()
}
