package main

// Tests for session storage, the SSE fan-out, and compaction planning.
//
// These gate `task deploy` (deploy -> check -> go test), so they are written to catch the
// failures that would actually hurt: losing a turn, leaking a path, replaying the wrong range
// after a reconnect, or compacting when the user is mid-sentence. Several of them encode a bug
// that was found by hand during development, so it cannot come back silently.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func TestAppendAssignsSequentialSeqs(t *testing.T) {
	st := newTestStore(t)
	s, err := st.Create("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(s.ID, "c1", Message{Role: "user", Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(s.ID, "c1", Message{Role: "assistant", Content: "two"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got.Messages))
	}
	for i, m := range got.Messages {
		if m.Seq != int64(i+1) {
			t.Errorf("message %d has seq %d, want %d", i, m.Seq, i+1)
		}
	}
	if got.NextSeq != 3 {
		t.Errorf("NextSeq = %d, want 3", got.NextSeq)
	}
}

// Append must ADD, never replace. The client posts its whole history each turn, so a
// replace-based implementation lets a device with a stale view silently delete a turn another
// device just added -- which is data loss with no error anywhere.
func TestAppendDoesNotReplaceHistory(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	st.Append(s.ID, "phone", Message{Role: "user", Content: "from phone"})
	st.Append(s.ID, "laptop", Message{Role: "user", Content: "from laptop"})
	got, _ := st.Get(s.ID)
	if len(got.Messages) != 2 {
		t.Fatalf("want both turns kept, got %d", len(got.Messages))
	}
	if got.Messages[0].Content != "from phone" {
		t.Errorf("first turn was overwritten: %q", got.Messages[0].Content)
	}
}

// A session id reaches the store from a URL path segment. Treating it as a filename without
// validation is a read of any file the process can open.
func TestSafeIDRejectsTraversal(t *testing.T) {
	bad := []string{
		"../etc/passwd", "..", "/absolute", "a/b", "a\\b", "with space",
		"UPPER", "dot.dot", "", strings.Repeat("a", 65),
	}
	for _, id := range bad {
		if safeID(id) {
			t.Errorf("safeID(%q) = true, want false", id)
		}
	}
	for _, id := range []string{"abc", "123", "1789-deadbeef", "a-b-c"} {
		if !safeID(id) {
			t.Errorf("safeID(%q) = false, want true", id)
		}
	}
}

func TestGetRejectsUnsafeID(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Get("../../etc/passwd"); err == nil {
		t.Fatal("Get accepted a traversal id")
	}
}

// Since drives two different clients: a reconnecting one asks for everything after the last
// seq it saw, and a fresh one asks for a window of the most recent. Getting the boundary wrong
// silently duplicates or skips messages.
func TestSinceWindowing(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	for i := 1; i <= 10; i++ {
		st.Append(s.ID, "c", Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}

	msgs, head, err := st.Since(s.ID, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if head != 10 {
		t.Errorf("head = %d, want 10", head)
	}
	if len(msgs) != 4 || msgs[0].Seq != 7 || msgs[3].Seq != 10 {
		t.Fatalf("since=6 gave %d msgs starting at seq %d; want 4 starting at 7",
			len(msgs), msgs[0].Seq)
	}

	// limit returns the LAST n, not the first n -- a device joining a long conversation wants
	// the recent end of it.
	msgs, _, _ = st.Since(s.ID, 0, 3)
	if len(msgs) != 3 || msgs[0].Seq != 8 {
		t.Fatalf("limit=3 gave %d msgs starting at seq %d; want 3 starting at 8",
			len(msgs), msgs[0].Seq)
	}

	// since beyond the head is the steady state for a connected client: nothing new.
	msgs, _, _ = st.Since(s.ID, 10, 0)
	if len(msgs) != 0 {
		t.Errorf("since=head returned %d messages, want 0", len(msgs))
	}
}

func TestSubscribeReceivesAppend(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	ch, release := st.Subscribe(s.ID)
	defer release()

	st.Append(s.ID, "deviceA", Message{Role: "user", Content: "hello"})

	select {
	case ev := <-ch:
		if ev.Kind != "commit" {
			t.Errorf("Kind = %q, want commit", ev.Kind)
		}
		if ev.Origin != "deviceA" {
			t.Errorf("Origin = %q; without it the sending device cannot drop its own echo", ev.Origin)
		}
		if len(ev.Msgs) != 1 || ev.Msgs[0].Seq != 1 {
			t.Errorf("event carried %d msgs, want 1 with seq 1", len(ev.Msgs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received nothing")
	}
}

// A wedged or paused device must not be able to stall the request that is persisting a turn.
// Dropping is recoverable (the client reconnects and asks for everything since its last seq);
// blocking the write is not.
func TestBroadcastDropsRatherThanBlocks(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	_, release := st.Subscribe(s.ID) // never drained
	defer release()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ { // far past the 16-deep buffer
			st.Append(s.ID, "c", Message{Role: "user", Content: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Append blocked on a subscriber that was not reading")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	ch, release := st.Subscribe(s.ID)
	release()
	if _, open := <-ch; open {
		t.Error("channel still open after release")
	}
}

func TestDeriveTitleStripsVoiceTagAndTruncates(t *testing.T) {
	got := deriveTitle([]Message{{Role: "assistant", Content: "ignored"},
		{Role: "user", Content: "[voice] what is the capital of Australia"}})
	if strings.Contains(got, "[voice]") {
		t.Errorf("title kept the input marker: %q", got)
	}
	if got != "what is the capital of Australia" {
		t.Errorf("title = %q", got)
	}
	long := deriveTitle([]Message{{Role: "user", Content: strings.Repeat("ab", 80)}})
	if len([]rune(long)) > 50 {
		t.Errorf("title not truncated: %d runes", len([]rune(long)))
	}
}

func TestListSortsByUpdatedDescending(t *testing.T) {
	st := newTestStore(t)
	a, _ := st.Create("first")
	time.Sleep(2 * time.Millisecond)
	b, _ := st.Create("second")
	time.Sleep(2 * time.Millisecond)
	st.Append(a.ID, "c", Message{Role: "user", Content: "bump a"})

	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	if list[0].ID != a.ID {
		t.Errorf("most recently updated session is not first: got %s, want %s", list[0].ID, a.ID)
	}
	_ = b
}

// The picker reads every session file. One corrupt file must not take the whole list with it.
func TestListSkipsCorruptFile(t *testing.T) {
	st := newTestStore(t)
	good, _ := st.Create("good")
	os.WriteFile(st.path("1700000000-corrupt"), []byte("{not json"), 0o600)
	list, err := st.List()
	if err != nil {
		t.Fatalf("List failed because of one bad file: %v", err)
	}
	if len(list) != 1 || list[0].ID != good.ID {
		t.Errorf("want only the good session, got %+v", list)
	}
}

// --- compaction planning ---------------------------------------------------------------

func TestEstTokensFromExcludesCompacted(t *testing.T) {
	s := &Session{
		Summary:          "short summary",
		CompactedThrough: 2,
		Messages: []Message{
			{Seq: 1, Content: strings.Repeat("x", 700)},
			{Seq: 2, Content: strings.Repeat("x", 700)},
			{Seq: 3, Content: strings.Repeat("x", 700)},
		},
	}
	n := s.estTokensFrom(s.CompactedThrough)
	// ~200 tokens for the one uncompacted message, plus the summary; nowhere near the ~600
	// that counting all three would give.
	if n > 300 {
		t.Errorf("estTokensFrom counted compacted messages: got %d", n)
	}
	if n < 150 {
		t.Errorf("estTokensFrom ignored the uncompacted message: got %d", n)
	}
}

// The soft threshold must sit well below the hard one. A narrow band leaves few idle moments to
// catch before the hard limit forces a compaction into the user's way, which is the entire
// thing the band exists to avoid.
func TestCompactBandIsWide(t *testing.T) {
	h := &voicebox{cfg: &Config{Default: "b", Backends: []*Backend{{ID: "b", URL: "http://127.0.0.1:1"}}}}
	soft, hard := h.softFrac("b"), h.hardFrac("b")
	if !(soft > 0 && soft < hard && hard < 1) {
		t.Fatalf("expected 0 < soft < hard < 1, got soft=%v hard=%v", soft, hard)
	}
	if hard-soft < 0.3 {
		t.Errorf("band is only %.2f wide; too narrow to catch idle moments", hard-soft)
	}
}

func TestCompactFractionsHonourConfig(t *testing.T) {
	h := &voicebox{cfg: &Config{Default: "b", Backends: []*Backend{
		{ID: "b", URL: "http://127.0.0.1:1", CompactAt: 0.9, CompactIdleAt: 0.2}}}}
	if got := h.hardFrac("b"); got != 0.9 {
		t.Errorf("hardFrac = %v, want 0.9", got)
	}
	if got := h.softFrac("b"); got != 0.2 {
		t.Errorf("softFrac = %v, want 0.2", got)
	}
}

// An unreachable or non-llama.cpp backend must yield "unknown", which disables compaction,
// rather than a guessed window. Compacting against a guess is worse than not compacting.
func TestUnknownContextDisablesCompaction(t *testing.T) {
	h := &voicebox{cfg: &Config{Default: "b", Backends: []*Backend{
		{ID: "b", URL: "http://127.0.0.1:1"}}}} // nothing listening
	if got := h.compactContext("b"); got != 0 {
		t.Errorf("compactContext on an unreachable backend = %d, want 0", got)
	}
}

// /props lives at the ROOT. A backend URL points at the OpenAI-compatible surface, so a naive
// base+"/props" becomes ".../v1/props" and 404s -- and the resulting 0 silently disables
// compaction rather than erroring.
func TestProbeContextStripsV1Suffix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"default_generation_settings": map[string]any{"n_ctx": 2048},
			"total_slots":                 4,
		})
	}))
	defer srv.Close()

	if n := probeContext(srv.URL + "/v1"); n != 2048 {
		t.Errorf("probeContext = %d, want 2048 (path hit was %q)", n, gotPath)
	}
	if gotPath != "/props" {
		t.Errorf("probed %q, want /props at the root", gotPath)
	}
}

// MEASURED on llama-server: --ctx-size 8192 --parallel 4 reports
// default_generation_settings.n_ctx = 2048, i.e. ALREADY divided per slot. Dividing again by
// total_slots would give 512 and compact four times too aggressively -- and over-compaction is
// silent, it just looks like a model with a poor memory.
func TestProbeContextDoesNotDivideBySlotsAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"default_generation_settings": map[string]any{"n_ctx": 2048},
			"total_slots":                 4,
		})
	}))
	defer srv.Close()
	if n := probeContext(srv.URL); n != 2048 {
		t.Errorf("probeContext = %d, want 2048 per-slot value used as-is", n)
	}
}

// --- the chat-response sniffer ----------------------------------------------------------

// The assistant turn is reassembled from SSE frames passing through to the browser. Frames
// split across Write boundaries are the normal case on a streaming response, so the buffer must
// carry the remainder rather than dropping it.
func TestSessionTeeReassemblesSplitFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := &sessionTee{ResponseWriter: rec}
	full := ""
	for _, c := range []string{"Hel", "lo ", "wor", "ld"} {
		frame := `data: {"choices":[{"delta":{"content":"` + c + `"}}]}` + "\n\n"
		full += c
		// split each frame down the middle, so no single Write contains a whole one
		h := len(frame) / 2
		tee.Write([]byte(frame[:h]))
		tee.Write([]byte(frame[h:]))
	}
	tee.Write([]byte("data: [DONE]\n\n"))
	if got := tee.text.String(); got != full {
		t.Errorf("reassembled %q, want %q", got, full)
	}
	if rec.Body.String() == "" {
		t.Error("tee swallowed the response instead of passing it through")
	}
}

// Reasoning deltas must NOT be stored: they are not spoken, not shown by default, and putting
// the model's scratch work into the transcript means replaying it as context next turn.
func TestSessionTeeIgnoresReasoningField(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := &sessionTee{ResponseWriter: rec}
	tee.Write([]byte(`data: {"choices":[{"delta":{"reasoning":"thinking hard"}}]}` + "\n\n"))
	tee.Write([]byte(`data: {"choices":[{"delta":{"content":"answer"}}]}` + "\n\n"))
	if got := tee.text.String(); got != "answer" {
		t.Errorf("captured %q, want just %q", got, "answer")
	}
}

// Only the LAST user message is persisted per request: the client posts its whole history, so
// taking all of them would duplicate the entire conversation on every turn.
func TestLastUserMessage(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"second"}]}`)
	got, ok := lastUserMessage(body)
	if !ok || got != "second" {
		t.Errorf("lastUserMessage = %q, %v; want \"second\", true", got, ok)
	}
	if _, ok := lastUserMessage([]byte(`{"messages":[{"role":"system","content":"s"}]}`)); ok {
		t.Error("reported a user message where there is none")
	}
	if _, ok := lastUserMessage([]byte("not json")); ok {
		t.Error("reported a user message from unparseable input")
	}
}

func TestSetSummaryBroadcastsCompact(t *testing.T) {
	st := newTestStore(t)
	s, _ := st.Create("")
	st.Append(s.ID, "c", Message{Role: "user", Content: "a"})
	ch, release := st.Subscribe(s.ID)
	defer release()

	if err := st.SetSummary(s.ID, "NOTES", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != "compact" {
			t.Errorf("Kind = %q, want compact -- clients must re-read compacted_through", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no compact event broadcast")
	}
	got, _ := st.Get(s.ID)
	if got.Summary != "NOTES" || got.CompactedThrough != 1 {
		t.Errorf("summary=%q through=%d", got.Summary, got.CompactedThrough)
	}
	// Compaction changes what is SENT, never what is stored.
	if len(got.Messages) != 1 {
		t.Errorf("compaction deleted messages: %d remain", len(got.Messages))
	}
}
