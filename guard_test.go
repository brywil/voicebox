package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGuard scores a text 0.9 if it contains "nonsense", else 0.1, corrects "sweeden" to "Sweden", and records every
// text it was asked about.
type fakeGuard struct {
	mu    sync.Mutex
	asked []string
	fail  bool
}

func (f *fakeGuard) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		var req struct{ Texts []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.asked = append(f.asked, req.Texts...)
		f.mu.Unlock()
		p := make([]float64, len(req.Texts))
		fixed := make([]string, len(req.Texts))
		fixes := make([][]map[string]string, len(req.Texts))
		for i, s := range req.Texts {
			p[i] = 0.1
			if strings.Contains(s, "nonsense") {
				p[i] = 0.9
			}
			fixed[i] = strings.ReplaceAll(s, "sweeden", "Sweden")
			fixes[i] = []map[string]string{}
			if fixed[i] != s {
				fixes[i] = append(fixes[i], map[string]string{"word": "sweeden", "suggest": "Sweden"})
			}
		}
		writeJSON(w, map[string]any{"p": p, "fixed": fixed, "fixes": fixes})
	}))
	t.Cleanup(s.Close)
	return s
}

func msgs(pairs ...string) []any { // role, content, role, content, ...
	var out []any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"role": pairs[i], "content": pairs[i+1]})
	}
	return out
}

func content(m any) string { return m.(map[string]any)["content"].(string) }

// THE CACHE RULE. The page sends history without the note; the note given on turn 1 must come back on turn 2
// byte-identical, or a hybrid model re-prefills the whole conversation. And the old message is not re-checked.
func TestGuardReplaysTheSameNoteOnLaterTurns(t *testing.T) {
	f := &fakeGuard{}
	g := NewGuard(GuardConfig{URL: f.server(t).URL}, "")

	t1 := msgs("user", "a nonsense question")
	if r, err := g.Apply(t.Context(), t1); err != nil || !r.Fired {
		t.Fatalf("turn 1: %+v err=%v, want a note", r, err)
	}
	noted := content(t1[0])
	if !strings.HasSuffix(noted, defaultGuardNote) {
		t.Fatalf("turn 1 note missing: %q", noted)
	}

	t2 := msgs("user", "a nonsense question", "assistant", "that makes no sense", "user", "a sound follow-up")
	r, err := g.Apply(t.Context(), t2)
	if err != nil || r.Fired {
		t.Fatalf("turn 2: %+v err=%v, want no note on the follow-up", r, err)
	}
	if content(t2[0]) != noted {
		t.Errorf("turn 2 changed turn 1's message:\n got %q\nwant %q", content(t2[0]), noted)
	}
	if content(t2[2]) != "a sound follow-up" {
		t.Errorf("sound follow-up was edited: %q", content(t2[2]))
	}
	if len(f.asked) != 2 || f.asked[1] != "a sound follow-up" {
		t.Errorf("guard asked %q, want each message checked once", f.asked)
	}
}

// History the guard never saw (pre-deployment, lost state) was answered without a note; adding one now would
// change the prefix. It must stay untouched and must not even be checked.
func TestGuardNeverNotesUnseenHistory(t *testing.T) {
	f := &fakeGuard{}
	g := NewGuard(GuardConfig{URL: f.server(t).URL}, "")
	m := msgs("user", "old nonsense", "assistant", "ok", "user", "new and fine")
	if _, err := g.Apply(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if content(m[0]) != "old nonsense" {
		t.Errorf("unseen history got a note: %q", content(m[0]))
	}
	if len(f.asked) != 1 || f.asked[0] != "new and fine" {
		t.Errorf("guard asked %q, want only the newest message", f.asked)
	}
}

func TestGuardReadsPastTheVoiceTag(t *testing.T) {
	f := &fakeGuard{}
	g := NewGuard(GuardConfig{URL: f.server(t).URL}, "")
	m := msgs("user", "[voice] whats the capital of sweeden")
	_, _ = g.Apply(t.Context(), m)
	if len(f.asked) != 1 || f.asked[0] != "whats the capital of sweeden" {
		t.Errorf("guard asked %q, want the text without [voice]", f.asked)
	}
}

// Fail open, and stay consistent: the failed message reached the model without a note, so on the next turn it
// is history the guard never decided -- exempt, still no note.
func TestGuardFailsOpenAndStaysConsistent(t *testing.T) {
	f := &fakeGuard{fail: true}
	g := NewGuard(GuardConfig{URL: f.server(t).URL}, "")
	m := msgs("user", "nonsense while the guard is down")
	if r, err := g.Apply(t.Context(), m); err == nil || r.Fired {
		t.Fatalf("want an error and no note, got %+v err=%v", r, err)
	}
	if content(m[0]) != "nonsense while the guard is down" {
		t.Fatalf("message edited despite failure: %q", content(m[0]))
	}
	f.fail = false
	m2 := msgs("user", "nonsense while the guard is down", "assistant", "x", "user", "fine")
	_, _ = g.Apply(t.Context(), m2)
	if content(m2[0]) != "nonsense while the guard is down" {
		t.Errorf("a message the model saw without a note got one later: %q", content(m2[0]))
	}
}

// A restart must not re-decide: the state file replays the same decision with no guard call at all.
func TestGuardDecisionsSurviveRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "guard-decisions.jsonl")
	f := &fakeGuard{}
	srv := f.server(t)
	m := msgs("user", "a nonsense question")
	if r, _ := NewGuard(GuardConfig{URL: srv.URL}, state).Apply(t.Context(), m); !r.Fired {
		t.Fatal("first run did not fire")
	}
	f.fail = true // a restarted guard that cannot be reached must still replay the note
	m2 := msgs("user", "a nonsense question", "assistant", "x", "user", "and then?")
	_, _ = NewGuard(GuardConfig{URL: srv.URL}, state).Apply(t.Context(), m2)
	if content(m2[0]) != content(m[0]) {
		t.Errorf("after restart:\n got %q\nwant %q", content(m2[0]), content(m[0]))
	}
}

func TestGuardHandlesArrayContent(t *testing.T) {
	f := &fakeGuard{}
	g := NewGuard(GuardConfig{URL: f.server(t).URL}, "")
	part := map[string]any{"type": "text", "text": "describe this nonsense"}
	m := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:,"}}, part}}}
	if r, err := g.Apply(t.Context(), m); err != nil || !r.Fired {
		t.Fatalf("%+v err=%v", r, err)
	}
	if !strings.HasSuffix(part["text"].(string), defaultGuardNote) {
		t.Errorf("text part not noted: %q", part["text"])
	}
}

// End to end through handleChat, tools off: the model receives the note and the page receives the notice.
func TestChatGuardNoteReachesModelAndNoticeReachesPage(t *testing.T) {
	f := &fakeGuard{}
	var upstreamBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"That premise does not hold."},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cfgPath, []byte(`{"backends":[{"id":"x","url":"`+up.URL+`"}]}`), 0o600)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	vb := &voicebox{cfg: cfg, upstream: &http.Client{}, guard: NewGuard(GuardConfig{URL: f.server(t).URL}, "")}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"is this nonsense?"}]}`))
	rec := httptest.NewRecorder()
	vb.handleChat(rec, req)

	var sent struct{ Messages []map[string]any }
	if err := json.Unmarshal(upstreamBody, &sent); err != nil {
		t.Fatalf("upstream body %q: %v", upstreamBody, err)
	}
	// The user's turn is the LAST message: voicebox's system prompt (prompt.go) now leads.
	if got := sent.Messages[len(sent.Messages)-1]["content"].(string); !strings.HasSuffix(got, defaultGuardNote) {
		t.Errorf("model did not receive the note: %q", got)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"object":"voicebox.event"`) || !strings.Contains(out, "guard:") {
		t.Errorf("page got no notice:\n%s", out)
	}
	if !strings.Contains(out, "That premise does not hold.") {
		t.Errorf("model reply not forwarded:\n%s", out)
	}
}

// Bryan's swipe typing: the model gets the corrected text, the [voice] marker survives, and the SAME corrected text
// comes back on later turns (and after a restart) so the prefix never changes.
func TestGuardFixesSpellingForTheModelAndReplaysIt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "guard-decisions.jsonl")
	f := &fakeGuard{}
	srv := f.server(t)
	m := msgs("user", "[voice] whats the capital of sweeden")
	r, err := NewGuard(GuardConfig{URL: srv.URL}, state).Apply(t.Context(), m)
	if err != nil || !r.Fresh || len(r.Fixes) != 1 || r.Fixes[0] != "sweeden→Sweden" {
		t.Fatalf("result %+v err=%v, want one fresh fix", r, err)
	}
	if got := content(m[0]); got != "[voice] whats the capital of Sweden" {
		t.Fatalf("model text %q", got)
	}
	f.fail = true // replay must not need the guard
	m2 := msgs("user", "[voice] whats the capital of sweeden", "assistant", "Stockholm.", "user", "and Norway?")
	_, _ = NewGuard(GuardConfig{URL: srv.URL}, state).Apply(t.Context(), m2)
	if got := content(m2[0]); got != "[voice] whats the capital of Sweden" {
		t.Errorf("after restart the correction was not replayed: %q", got)
	}
}

// The score is shown on every fresh decision, below the threshold too, so a near miss is visible on the page.
func TestGuardNoticeShowsScoreBelowThreshold(t *testing.T) {
	f := &fakeGuard{}
	vb := &voicebox{guard: NewGuard(GuardConfig{URL: f.server(t).URL}, "")}
	got := vb.guardRequest(t.Context(), map[string]any{"messages": msgs("user", "what is the capital of Norway?")})
	if got != "guard: p 0.10" {
		t.Errorf("notice %q, want the score alone", got)
	}
}

func TestGuardNoticeNamesTheFix(t *testing.T) {
	f := &fakeGuard{}
	vb := &voicebox{guard: NewGuard(GuardConfig{URL: f.server(t).URL}, "")}
	raw := map[string]any{"messages": msgs("user", "is sweeden nonsense?")}
	got := vb.guardRequest(t.Context(), raw)
	if !strings.Contains(got, "fixed sweeden→Sweden") || !strings.Contains(got, "note added") || !strings.Contains(got, "p 0.90") {
		t.Errorf("notice %q, want the fix and the note", got)
	}
	if again := vb.guardRequest(t.Context(), map[string]any{"messages": msgs("user", "is sweeden nonsense?")}); again != "" {
		t.Errorf("replay announced again: %q", again)
	}
}

// A guard shared over the LAN can require a token; voicebox must send it from VOICEBOX_GUARD_TOKEN, and send nothing
// when the variable is unset (the default, and how the Spark's own localhost guard runs).
func TestGuardSendsTokenOnlyWhenSet(t *testing.T) {
	var got []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"p": []float64{0.1}, "fixed": []string{"x"}})
	}))
	defer s.Close()
	g := NewGuard(GuardConfig{URL: s.URL}, "")

	t.Setenv("VOICEBOX_GUARD_TOKEN", "")
	_, _ = g.check(context.Background(), []string{"hello"})
	t.Setenv("VOICEBOX_GUARD_TOKEN", "s3cret")
	_, _ = g.check(context.Background(), []string{"hello"})

	if len(got) != 2 || got[0] != "" || got[1] != "Bearer s3cret" {
		t.Fatalf("Authorization headers sent = %q; want [\"\" \"Bearer s3cret\"]", got)
	}
}
