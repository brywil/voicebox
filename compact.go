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
	"sync"
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

	// Announced only now, after the decision that there IS something to compact. Announcing
	// earlier would flash an indicator for the common case where Compact returns immediately
	// with nothing to do.
	h.store.Notify(id, "compacting")
	ok := false
	defer func() {
		if !ok {
			// Every failure path below must clear the indicator, or a client that showed
			// "compacting" is stuck displaying it. A deferred flag is the only way to catch
			// all of them without repeating the call at each return.
			h.store.Notify(id, "compact_failed")
		}
	}()

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
	ok = true
	after, _ := h.store.Get(id)
	log.Printf("[compact] %s: %d messages -> summary, ~%d tokens -> ~%d (through seq %d)",
		id, len(toSummarise), before, after.estTokensFrom(after.CompactedThrough), through)
	return nil
}

// --- WHEN TO COMPACT -------------------------------------------------------------------
//
// TWO THRESHOLDS, WIDE APART, because compaction is cheap to do early and expensive to do late.
//
//   SOFT (default 0.35)  above this, compact as soon as the conversation goes quiet. Most
//                        compactions should happen here, in a gap while the user is thinking,
//                        where nothing is waiting on the backend.
//   HARD (default 0.85)  above this, compact immediately regardless of activity. Overflowing
//                        the context is worse than a visible stall: the backend silently drops
//                        the front of the conversation and the model starts contradicting
//                        things it agreed to, with nothing in any log to explain it.
//
// The band is deliberately wide. A narrow band would put the soft threshold close to the hard
// one and leave few idle moments to catch, which defeats the point -- the whole reason to start
// early is to have many chances to compact for free before being forced to do it in the user's
// way. MEASURED on this box: on --parallel 1 a summariser call made a five-token user turn take
// 5.04 s instead of ~0.3 s, because the turn queued behind it. That stall is what the soft
// threshold exists to avoid.

const (
	defaultSoftFrac = 0.35
	defaultHardFrac = 0.85
	// defaultIdle is how quiet a conversation must be before an opportunistic compaction runs.
	// Long enough that a pause for thought is not mistaken for the end of a conversation;
	// short enough to catch the gap when someone puts the phone down.
	defaultIdle = 45 * time.Second
	sweepEvery  = 20 * time.Second
)

// inFlight stops the sweeper and the request path from compacting the same session at once,
// which would run two summarisers over the same turns and have the second overwrite the first.
var inFlight sync.Map // session id -> struct{}

func (h *voicebox) compactOnce(id, backendID, model, why string) {
	if _, busy := inFlight.LoadOrStore(id, struct{}{}); busy {
		return
	}
	go func() {
		defer inFlight.Delete(id)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := h.Compact(ctx, id, backendID, model, defaultKeepVerbatim); err != nil {
			log.Printf("[compact] %s (%s): %v", id, why, err)
		}
	}()
}

// maybeCompact is the HARD check, on the request path. It runs after the reply has been
// delivered, so it never delays the turn that triggered it -- but it does not wait for idle
// either, because at this point the next turn may not fit.
func (h *voicebox) maybeCompact(id, backendID, model string) {
	if h.store == nil {
		return
	}
	sess, err := h.store.Get(id)
	if err != nil {
		return
	}
	ctxTokens := h.compactContext(backendID)
	if ctxTokens <= 0 {
		return // no window known for this backend; never guess one
	}
	if sess.estTokensFrom(sess.CompactedThrough) >= int(float64(ctxTokens)*h.hardFrac(backendID)) {
		h.compactOnce(id, backendID, model, "hard limit")
	}
}

// sweepCompactable is the SOFT check: every session that is over the soft threshold AND has
// been quiet long enough gets compacted opportunistically.
func (h *voicebox) sweepCompactable() {
	metas, err := h.store.List()
	if err != nil {
		return
	}
	now := time.Now()
	for _, m := range metas {
		if now.Sub(time.UnixMilli(m.Updated)) < defaultIdle {
			continue
		}
		sess, err := h.store.Get(m.ID)
		if err != nil {
			continue
		}
		ctxTokens := h.compactContext(sess.Backend)
		if ctxTokens <= 0 {
			continue
		}
		if sess.estTokensFrom(sess.CompactedThrough) >= int(float64(ctxTokens)*h.softFrac(sess.Backend)) {
			h.compactOnce(m.ID, sess.Backend, sess.Model, "idle")
		}
	}
}

// StartCompactSweeper runs the opportunistic pass. Started once at boot; a no-op when sessions
// are disabled.
func (h *voicebox) StartCompactSweeper() {
	if h.store == nil {
		return
	}
	go func() {
		for range time.Tick(sweepEvery) {
			h.sweepCompactable()
		}
	}()
	log.Printf("[compact] idle sweeper active (soft %.0f%%, hard %.0f%%, idle %s)",
		defaultSoftFrac*100, defaultHardFrac*100, defaultIdle)
}

// compactContext is the window to plan against: configured if set, otherwise probed live from
// the backend (llama.cpp reports its per-slot context on /props). Zero means unknown, which
// leaves compaction off rather than guessing.
func (h *voicebox) compactContext(backendID string) int {
	return h.contextFor(h.cfg.find(orDefault(backendID, h.cfg.Default)))
}

func (h *voicebox) softFrac(backendID string) float64 {
	if b := h.cfg.find(orDefault(backendID, h.cfg.Default)); b != nil && b.CompactIdleAt > 0 && b.CompactIdleAt < 1 {
		return b.CompactIdleAt
	}
	return defaultSoftFrac
}

func (h *voicebox) hardFrac(backendID string) float64 {
	if b := h.cfg.find(orDefault(backendID, h.cfg.Default)); b != nil && b.CompactAt > 0 && b.CompactAt < 1 {
		return b.CompactAt
	}
	return defaultHardFrac
}

// defaultKeepVerbatim is how many recent messages stay uncompressed. Six is roughly three
// exchanges -- enough that the model still has the exact wording of whatever is currently being
// worked on, which is the context most expensive to lose.
const defaultKeepVerbatim = 6
