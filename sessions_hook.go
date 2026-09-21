package main

// Persistence hook for /v1/chat/completions.
//
// WHY THE SERVER PERSISTS RATHER THAN THE CLIENT. The client already posts its whole history
// with every request and the reply already streams back through here, so the server sees both
// halves of every turn. Having the browser save afterwards would duplicate that, and would lose
// the turn in exactly the case that motivated this work: the page going away unexpectedly.
//
// The user turn is written ON RECEIPT and the assistant turn ON COMPLETION. That ordering is the
// point -- refresh, crash or close mid-generation and the question survives even though the
// answer does not. The other order saves nothing until the end and loses both.
//
// ONE HOOK COVERS BOTH PATHS. handleChat either proxies straight through or drives the agentic
// tool loop, and both emit OpenAI-format SSE. Sniffing the response stream catches the assistant
// text from either without the two paths each needing their own save call -- which is the kind
// of duplication that later drifts apart and leaves one path silently not persisting.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// sessionTee wraps the ResponseWriter for a chat request, passing bytes through untouched while
// reassembling the assistant's text from the SSE frames going past.
type sessionTee struct {
	http.ResponseWriter
	flusher http.Flusher
	buf     bytes.Buffer // partial SSE frame across Write boundaries
	text    strings.Builder
}

func (t *sessionTee) Flush() {
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func (t *sessionTee) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p) // client first: persistence must never delay the stream
	t.buf.Write(p)
	t.scan()
	return n, err
}

// scan pulls complete "data: {...}" frames out of the buffer and accumulates delta content.
// A frame can be split across Writes, so anything after the last blank line stays buffered.
func (t *sessionTee) scan() {
	for {
		b := t.buf.Bytes()
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := string(b[:i])
		t.buf.Next(i + 2)
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(payload), &ev) != nil || len(ev.Choices) == 0 {
				continue
			}
			// Only `content` is accumulated. Reasoning/thinking deltas arrive in a separate
			// field and are deliberately excluded: they are not spoken, not shown by default,
			// and storing them would put the model's scratch work into the transcript that
			// gets replayed to it on the next turn.
			if c := ev.Choices[0].Delta.Content; c != "" {
				t.text.WriteString(c)
			} else if c := ev.Choices[0].Message.Content; c != "" {
				t.text.WriteString(c)
			}
		}
	}
}

// withSession persists a chat turn if the client named a session. It returns the writer that
// handleChat should use.
//
// A missing or unknown session header means this is simply not a session-backed request, and it
// proceeds exactly as before. Sessions are additive: nothing about the existing chat path
// changes when the feature is unused or disabled.
func (h *voicebox) withSession(w http.ResponseWriter, r *http.Request, body []byte) (http.ResponseWriter, func()) {
	if h.store == nil {
		return w, func() {}
	}
	sid := r.Header.Get("X-Voicebox-Session")
	if sid == "" || !safeID(sid) {
		return w, func() {}
	}
	if _, err := h.store.Get(sid); err != nil {
		log.Printf("[sessions] chat referenced unknown session %q; not persisting", sid)
		return w, func() {}
	}
	origin := r.Header.Get("X-Voicebox-Client")
	// Recorded so the idle sweeper, which runs outside any request, summarises with the same
	// model this conversation is using rather than the server default.
	h.store.SetRoute(sid, r.Header.Get("X-Voicebox-Backend"), modelOf(body))

	// Persist only the LAST user message. The request carries the whole history, but the
	// earlier turns are already stored -- appending them again would duplicate the entire
	// conversation on every single request.
	if last, ok := lastUserMessage(body); ok {
		if _, err := h.store.AppendUserTurn(sid, origin, last); err != nil {
			log.Printf("[sessions] append user turn: %v", err)
		}
	}

	flusher, _ := w.(http.Flusher)
	tee := &sessionTee{ResponseWriter: w, flusher: flusher}
	return tee, func() {
		text := strings.TrimSpace(tee.text.String())
		if text == "" {
			// Nothing to store: the request failed, was cancelled, or the client went away
			// mid-generation. The user turn is already saved, which is the behaviour that
			// matters -- the question survives a refresh even when the answer does not.
			return
		}
		// Stamped with the model that produced it: the picker can change the model mid
		// conversation, and a later turn must be able to tell whose words these were.
		if _, err := h.store.Append(sid, origin,
			Message{Role: "assistant", Content: text, Model: modelOf(body)}); err != nil {
			log.Printf("[sessions] append assistant turn: %v", err)
			return
		}
		// Checked AFTER the reply has been delivered, and it runs in the background: making
		// the user wait on a summariser call to finish a turn they already received would be
		// a visible stall for no benefit.
		h.maybeCompact(sid, r.Header.Get("X-Voicebox-Backend"), modelOf(body))
	}
}

// modelOf reads the model name back out of the request so a background compaction summarises
// with the same model the conversation is using, rather than whatever the backend defaults to.
func modelOf(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Model
}

// lastUserMessage extracts the final user turn from an OpenAI-format request body.
func lastUserMessage(body []byte) (string, bool) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		// Content is a string in this client, but the OpenAI schema also allows an array of
		// parts. Handle the string case and skip the rest rather than storing raw JSON.
		var s string
		if json.Unmarshal(req.Messages[i].Content, &s) == nil {
			return s, s != ""
		}
		return "", false
	}
	return "", false
}
