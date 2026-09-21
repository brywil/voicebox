package main

// Conversation compaction.
//
// WHY THIS EXISTS NOW AND NOT BEFORE. Until sessions were added, a page refresh threw the
// conversation away -- which was annoying, and was also the only thing bounding context growth.
// The bug was load-bearing. With auto-resume a conversation can now run for weeks, and every
// turn sends the whole thing to the model, so without compaction it grows until the backend
// truncates it silently or rejects the request.
//
// WHY THE SERVER COMPACTS AND NOT EACH DEVICE. If the phone compacted at turn 40 and the laptop
// at turn 45, each would produce a DIFFERENT summary, send the model a different context, and
// the same conversation would behave differently depending on which device you picked up. Doing
// it once on the server makes the compacted view shared by construction, and the existing SSE
// fan-out tells every device about it immediately.
//
// NOTHING IS EVER DELETED. Compaction changes what gets SENT, never what is stored: the full
// transcript stays on disk, and `compacted_through` simply marks how much of it a client should
// replace with the summary. So a bad summary is recoverable -- recompact from the original turns
// -- and the record survives for reading later, which is most of why it was worth persisting.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// SummaryPrompt asks for the things a conversation actually needs carried forward.
//
// Deliberately NOT a narrative recap. What matters on resume is what is TRUE and what is
// OUTSTANDING -- a model handed "the user asked about X, then we discussed Y" has to re-derive
// the state from a story, whereas a list of established facts is usable directly.
const SummaryPrompt = `Summarise the conversation so far so it can be continued without the original transcript.

Write it as compact notes under these headings, omitting any that are empty:
FACTS - things established as true, including specifics like names, numbers, paths and versions
DECISIONS - what was chosen, and the reason if one was given
PREFERENCES - how the user wants things done, including anything they corrected
OPEN - questions asked but not answered, and work started but not finished

Rules:
- Preserve exact identifiers verbatim. A summary that rounds a number or paraphrases a filename is worse than no summary.
- Do not include pleasantries, acknowledgements, or a description of the conversation's shape.
- Do not speculate or add anything not present in the transcript.
- Third person, no preamble, no closing remark.`

// estTokens approximates a token count from bytes.
//
// There is no tokeniser here and adding one for three backends with different vocabularies
// would be worse than an estimate. 3.5 bytes/token is deliberately pessimistic for English
// (~4 is typical) because the cost of the two errors is not symmetric: compacting slightly
// early wastes a cheap model call, while compacting too late means a request the backend
// rejects or silently truncates, and silent truncation is the failure nobody notices.
func estTokens(s string) int { return len(s)*2/7 + 1 }

func (s *Session) estTokensFrom(seq int64) int {
	n := estTokens(s.Summary)
	for _, m := range s.Messages {
		if m.Seq > seq {
			n += estTokens(m.Content) + 4 // per-message role/framing overhead
		}
	}
	return n
}

// SetSummary stores a summary and the point it covers, then tells every connected device.
func (st *Store) SetSummary(id, summary string, through int64) error {
	m := st.lockFor(id)
	m.Lock()
	sess, err := st.Get(id)
	if err != nil {
		m.Unlock()
		return err
	}
	sess.Summary = summary
	sess.CompactedThrough = through
	sess.Updated = time.Now().UnixMilli()
	err = st.write(sess)
	m.Unlock()
	if err != nil {
		return err
	}
	// Kind "compact" rather than "commit": no new messages exist, but every device must
	// re-read `compacted_through` or it will keep sending turns the summary already covers.
	st.broadcast(Event{Session: id, Kind: "compact", Msgs: nil})
	return nil
}

// Compact summarises everything up to and including `through` and stores the result.
//
// keepVerbatim recent turns are EXCLUDED from the summary even if they fall before `through`.
// Summarising what was just said is where these systems feel brain-damaged: the model loses the
// exact wording of the thing it is currently working on. Recent turns stay verbatim; only the
// older tail is compressed.
func (h *voicebox) Compact(ctx context.Context, id string, backendID string, model string, keepVerbatim int) error {
	sess, err := h.store.Get(id)
	if err != nil {
		return err
	}
	b := h.cfg.find(orDefault(backendID, h.cfg.Default))
	if b == nil {
		return fmt.Errorf("unknown backend %q", backendID)
	}

	// Choose the cut: everything except the last keepVerbatim messages, and never anything
	// already covered by an existing summary.
	cut := len(sess.Messages) - keepVerbatim
	if cut <= 0 {
		return nil // nothing old enough to be worth compressing
	}
	var toSummarise []Message
	for _, m := range sess.Messages[:cut] {
		if m.Seq > sess.CompactedThrough {
			toSummarise = append(toSummarise, m)
		}
	}
	if len(toSummarise) == 0 {
		return nil
	}
	through := toSummarise[len(toSummarise)-1].Seq

	var sb strings.Builder
	if sess.Summary != "" {
		// Fold the previous summary in rather than discarding it, or each compaction would
		// forget everything the one before it established.
		sb.WriteString("Notes from earlier in this conversation:\n")
		sb.WriteString(sess.Summary)
		sb.WriteString("\n\nSubsequent transcript:\n")
	}
	for _, m := range toSummarise {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}

	payload := map[string]any{
		"model":  model,
		"stream": false,
		"messages": []map[string]any{
			{"role": "system", "content": SummaryPrompt},
			{"role": "user", "content": sb.String()},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.URL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKeyEnv != "" {
		if k := envOf(b.APIKeyEnv); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := h.upstream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("summariser: HTTP %d: %s", resp.StatusCode, trim(string(raw), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return fmt.Errorf("summariser: unparseable response")
	}
	summary := strings.TrimSpace(out.Choices[0].Message.Content)
	if summary == "" {
		// An empty summary would silently drop every turn it claimed to cover. Failing here
		// leaves the conversation uncompacted, which is recoverable; storing it is not.
		return fmt.Errorf("summariser returned nothing; leaving conversation uncompacted")
	}

	before := sess.estTokensFrom(sess.CompactedThrough)
	if err := h.store.SetSummary(id, summary, through); err != nil {
		return err
	}
	after, _ := h.store.Get(id)
	log.Printf("[compact] %s: %d messages -> summary, ~%d tokens -> ~%d (through seq %d)",
		id, len(toSummarise), before, after.estTokensFrom(after.CompactedThrough), through)
	return nil
}

// maybeCompact runs compaction in the background when a session has grown past its budget.
//
// Asynchronous on purpose: this is triggered just after a reply was delivered, and making the
// user wait for a summariser call to finish a turn they already received would be a visible
// stall for no benefit. If it fails, the conversation is merely uncompacted and the next turn
// tries again.
func (h *voicebox) maybeCompact(id, backendID, model string) {
	if h.store == nil {
		return
	}
	sess, err := h.store.Get(id)
	if err != nil {
		return
	}
	budget := h.compactBudget(backendID)
	if budget <= 0 {
		return // no context size known for this backend; never guess one
	}
	if sess.estTokensFrom(sess.CompactedThrough) < budget {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := h.Compact(ctx, id, backendID, model, defaultKeepVerbatim); err != nil {
			log.Printf("[compact] %s: %v", id, err)
		}
	}()
}

// defaultKeepVerbatim is how many recent messages stay uncompressed. Six is roughly three
// exchanges -- enough that the model still has the exact wording of whatever is currently being
// worked on, which is the context most expensive to lose.
const defaultKeepVerbatim = 6

// compactBudget is the token count above which a session is compacted: a fraction of the
// backend's context, leaving room for the reply itself and for the next few turns before this
// fires again. Zero means the backend's context is unconfigured and compaction stays off --
// guessing a context size would mean either compacting conversations that never needed it or
// failing to compact ones that did.
func (h *voicebox) compactBudget(backendID string) int {
	b := h.cfg.find(orDefault(backendID, h.cfg.Default))
	if b == nil || b.Context <= 0 {
		return 0
	}
	frac := b.CompactAt
	if frac <= 0 || frac >= 1 {
		frac = 0.6
	}
	return int(float64(b.Context) * frac)
}
