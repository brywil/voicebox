package main

// Executes real client functions, rather than asserting that certain strings appear in them.
//
// WHY THIS FILE EXISTS. web_test.go's TestClientHonoursCompaction greps index.html for
// "compacted_through" and passes if the string is present. A client that reads the field and
// then ignores it contains the string, so the test was GREEN while the exact failure its own
// comment described -- "a client that ignores compacted_through keeps sending everything,
// the conversation keeps growing, and compaction achieves nothing while appearing to work on
// the server side" -- was live in sessionLoad. Grepping for a symbol is a proxy for using it.
//
// So this pulls the real sessionLoad and buildContext out of the page, runs them under node
// against a stub of the server's actual JSON shape, and counts the messages that come out.
// That is the operation, not a proxy for it. Skips when node is absent so it does not become
// a build dependency.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractFn pulls one function out of the client source by balancing braces.
//
// It skips the parameter list first. Balancing braces from the declaration trips over a
// destructured default like ({ silent = false } = {}), whose closing brace looks like the end
// of the function body -- which silently truncates the extraction and produces a test that
// exercises a fragment.
func extractFn(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("could not find %q in the client", decl)
	}
	p := strings.Index(src[i:], "(")
	if p < 0 {
		t.Fatalf("no parameter list after %q", decl)
	}
	p += i
	depth := 0
	j := p
	for ; j < len(src); j++ {
		switch src[j] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	b := strings.Index(src[j:], "{")
	if b < 0 {
		t.Fatalf("no body after the parameter list of %q", decl)
	}
	b += j
	depth = 0
	for k := b; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : k+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting %q", decl)
	return ""
}

// TestClientDoesNotResendCompactedTurns is the real check behind compaction.
//
// The server returns the full recent window so the client can RENDER the scrollback, including
// turns the summary already covers. The client must render those and NOT resend them. Getting
// this wrong is silent and self-worsening: the server's estTokensFrom counts only messages
// above compacted_through, so it believes the context is small while the client is shipping
// summary-plus-everything, the hard threshold under-counts and never fires, and the backend
// quietly drops the front of the conversation.
func TestClientDoesNotResendCompactedTurns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping executing client test")
	}
	src := clientHTML(t)
	fns := extractFn(t, src, "async function sessionLoad") + "\n\n" +
		extractFn(t, src, "function buildContext")

	const total, covered = 10, 6
	var msgs []string
	for i := 1; i <= total; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"seq":%d,"role":%q,"content":"turn %d body"}`, i, role, i))
	}

	harness := fmt.Sprintf(`
// --- stubs for the page globals the extracted functions touch -----------------
let history = [], sessionId = null, sessionHead = 0;
let compactedThrough = 0, summaryText = "", sessionES = null;
const logEl = { innerHTML: "", appendChild(){}, set scrollTop(v){}, get scrollTop(){return 0} };
// buildContext labels turns from a model other than the one now selected, so it needs to know
// which that is. Every stored turn here carries no model, so nothing should be labelled.
const modelEl = { value: "model-a" };
const localStorage = { store:{}, setItem(k,v){this.store[k]=v}, getItem(k){return this.store[k]??null} };
function addMsg(){ return { parentNode:null }; }
function addReplay(){}
function stripTag(t){ return t.replace(/^\s*\[(voice|typed)\]\s*/i, ""); }
function status(){}
function showCompactionMarker(){ return { remove(){} }; }
function renderMessage(){}
function sessionSubscribe(){}
const SERVER = {
  id: "s1", title: "t", created: 0, updated: 0,
  head: %d, total: %d, truncated: false,
  summary: "FACTS - the port is 8080",
  compacted_through: %d,
  messages: [%s]
};
globalThis.fetch = async () => ({ ok: true, json: async () => SERVER });

%s

// --- the actual check ---------------------------------------------------------
await sessionLoad("s1", { silent: true });
const sent = buildContext();
const resent = sent.filter(m => m.role !== "system" &&
  SERVER.messages.some(s => s.seq <= SERVER.compacted_through && s.content === m.content));
console.log(JSON.stringify({
  sent: sent.length,
  resentCoveredTurns: resent.length,
  hasSummary: sent.some(m => m.role === "system" && m.content.includes("FACTS")),
}));
`, total, total, covered, strings.Join(msgs, ","), fns)

	dir := t.TempDir()
	f := filepath.Join(dir, "check.mjs")
	if err := os.WriteFile(f, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed to run:\n%s", out)
	}
	line := strings.TrimSpace(string(out))
	t.Logf("client produced: %s", line)

	if !strings.Contains(line, `"hasSummary":true`) {
		t.Error("the summary was not included in the context; earlier turns are simply lost")
	}
	if !strings.Contains(line, `"resentCoveredTurns":0`) {
		t.Errorf("the client RESENDS turns the summary already covers, so compaction has no "+
			"effect on what reaches the model -- and the server's token estimate, which "+
			"excludes them, will under-count and never trigger the hard limit.\ngot: %s", line)
	}
	// summary + the 4 turns above compacted_through
	if !strings.Contains(line, fmt.Sprintf(`"sent":%d`, 1+total-covered)) {
		t.Errorf("expected summary plus %d surviving turns; got: %s", total-covered, line)
	}
}

// TestClientActsOnDeletedEvent executes the live-stream handler against a deleted session.
//
// The server broadcasts kind:"deleted" and closes that session's subscribers, and a test on
// the server side can show the event goes out. That is not the fix. If the PAGE ignores it,
// the user-visible failure is exactly what it was before the event existed: the device keeps
// posting a session id the server no longer has, the hook declines every turn, and the only
// evidence is one line in the server log while the UI looks entirely normal.
//
// So this runs the real sessionSubscribe and sessionGone, feeds the handler a deleted event,
// and checks the page actually stops using the dead id.
func TestClientActsOnDeletedEvent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping executing client test")
	}
	src := clientHTML(t)
	fns := extractFn(t, src, "async function sessionGone") + "\n\n" +
		extractFn(t, src, "function sessionSubscribe")

	harness := `
// --- stubs for the page globals the extracted functions touch -----------------
let sessionId = "dead-one", sessionES = null, sessionHead = 0;
let history = [], compactedThrough = 0, summaryText = "", compactingEl = null;
const CLIENT_ID = "deviceA";
const localStorage = {
  store: { "vb.session": "dead-one" },
  setItem(k, v) { this.store[k] = v; },
  getItem(k) { return this.store[k] ?? null; },
  removeItem(k) { delete this.store[k]; },
};
let newCalls = 0, markers = [];
async function sessionNew() { newCalls++; sessionId = "fresh-one"; localStorage.setItem("vb.session", sessionId); }
async function sessionRefreshList() {}
function showCompactionMarker(text) { markers.push(text); return { remove() {} }; }
function status() {}
function renderMessage() {}
class FakeES {
  constructor(url) { this.url = url; this.readyState = 1; FakeES.last = this; }
  close() { this.readyState = 2; this.closed = true; }
}
FakeES.CLOSED = 2;
globalThis.EventSource = FakeES;
globalThis.fetch = async () => ({ status: 404, ok: false, json: async () => ({}) });

` + fns + `

// --- the actual check ---------------------------------------------------------
sessionSubscribe();
const opened = FakeES.last;
opened.onmessage({ data: JSON.stringify({ session: "dead-one", kind: "deleted" }) });
await new Promise((r) => setTimeout(r, 50));
console.log(JSON.stringify({
  startedNewSession: newCalls,
  stillUsingDeadId: sessionId === "dead-one",
  deadIdStillStored: localStorage.getItem("vb.session") === "dead-one",
  closedTheDeadStream: opened.closed === true,
  toldTheUser: markers.some((m) => /deleted/i.test(m)),
}));
`

	dir := t.TempDir()
	f := filepath.Join(dir, "deleted.mjs")
	if err := os.WriteFile(f, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed to run:\n%s", out)
	}
	line := strings.TrimSpace(string(out))
	t.Logf("client produced: %s", line)

	for _, want := range []struct{ frag, why string }{
		{`"stillUsingDeadId":false`,
			"the page kept the deleted session id: every following turn is posted to a session " +
				"the server no longer has, declined, and lost with only a log line to show for it"},
		{`"deadIdStillStored":false`,
			"the deleted id is still in localStorage, so the next refresh resumes it again"},
		{`"startedNewSession":1`,
			"no replacement session was started, so nothing said from here on is saved"},
		{`"closedTheDeadStream":true`,
			"the dead EventSource was left open"},
		{`"toldTheUser":true`,
			"the conversation vanished from under the user with nothing on screen to say so"},
	} {
		if !strings.Contains(line, want.frag) {
			t.Errorf("%s\ngot: %s", want.why, line)
		}
	}
}

// TestClientLabelsTurnsFromAnotherModel executes buildContext across a model switch.
//
// The picker can change the model mid-conversation while earlier turns stay verbatim in the
// context. Unlabelled, the incoming model reads its predecessor's words as its own first-person
// history -- openclaw-go 5104a5a, where a model went in circles over which model it was after
// three backend swaps in one evening. The label must go on turns from ANOTHER model only: a
// model reading its own words should see exactly what it saw before and pay nothing.
//
// Also checks the `model` field itself never leaves the page. It is bookkeeping, and an
// unrecognised field on a message is ignored by some backends and rejected by others.
func TestClientLabelsTurnsFromAnotherModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping executing client test")
	}
	fns := extractFn(t, clientHTML(t), "function buildContext")

	harness := `
let summaryText = "";
const modelEl = { value: "model-now" };
const history = [
  { role: "user", content: "who are you" },
  { role: "assistant", content: "the small one", model: "model-before" },
  { role: "user", content: "and now" },
  { role: "assistant", content: "the large one", model: "model-now" },
];

` + fns + `

const sent = buildContext();
const byContent = (frag) => sent.find((m) => m.content.includes(frag));
console.log(JSON.stringify({
  foreignLabelled: /^\[written by model-before/.test(byContent("the small one").content),
  ownTurnUntouched: byContent("the large one").content === "the large one",
  userTurnsUntouched: sent.filter((m) => m.role === "user")
                          .every((m) => !m.content.startsWith("[written by")),
  leaksModelField: sent.some((m) => "model" in m),
}));
`
	dir := t.TempDir()
	f := filepath.Join(dir, "attribution.mjs")
	if err := os.WriteFile(f, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed to run:\n%s", out)
	}
	line := strings.TrimSpace(string(out))
	t.Logf("client produced: %s", line)

	for _, want := range []struct{ frag, why string }{
		{`"foreignLabelled":true`,
			"a turn from the previous model is unlabelled, so the current model reads it as its own"},
		{`"ownTurnUntouched":true`,
			"the model's OWN turns were labelled too, which changes what it sees for no reason"},
		{`"userTurnsUntouched":true`, "a user turn was attributed to a model"},
		{`"leaksModelField":false`,
			"the bookkeeping `model` field is being sent upstream on each message"},
	} {
		if !strings.Contains(line, want.frag) {
			t.Errorf("%s\ngot: %s", want.why, line)
		}
	}
}
