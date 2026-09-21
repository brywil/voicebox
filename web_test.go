package main

// Contract tests between the server and the single-page client.
//
// The client is one HTML file with no build step and no test runner, so these assert the few
// couplings that are INVISIBLE WHEN BROKEN. Every item here fails silently if it regresses:
// drop the session header and persistence simply stops with no error, forget the compaction
// fields and conversations quietly grow past the window, lose the overscroll rule and a
// mis-swipe throws the page away again. None of that shows up in a Go test of the server
// alone, and none of it shows up as a JavaScript error either.
//
// These are deliberately coarse -- they check that a coupling EXISTS, not how it is written --
// so ordinary refactoring does not break them.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func clientHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read client: %v", err)
	}
	return string(b)
}

func TestClientSendsSessionHeaders(t *testing.T) {
	s := clientHTML(t)
	for _, h := range []string{"X-Voicebox-Session", "X-Voicebox-Client"} {
		if !strings.Contains(s, h) {
			t.Errorf("client no longer sends %s -- the server will not persist or fan out "+
				"the turn, and nothing will report an error", h)
		}
	}
}

func TestClientUsesSessionAPI(t *testing.T) {
	s := clientHTML(t)
	for _, frag := range []string{
		"/api/sessions", // list, create, load
		"/events",       // the live stream
		"EventSource",   // must be the auto-reconnecting one, not a poll
		"since=",        // seeds the first connection so a replayed page is not resent it all
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("client is missing %q; live sync or resume will not work", frag)
		}
	}
}

// Compaction only takes effect if the client stops resending the turns the summary covers.
// A client that ignores compacted_through keeps sending everything, the conversation keeps
// growing, and compaction achieves nothing -- while appearing to work on the server side.
func TestClientHonoursCompaction(t *testing.T) {
	s := clientHTML(t)
	for _, frag := range []string{"compacted_through", "summary", "buildContext"} {
		if !strings.Contains(s, frag) {
			t.Errorf("client does not reference %q: compaction will have no effect on what "+
				"is actually sent to the model", frag)
		}
	}
	for _, kind := range []string{"compacting", "compact_failed"} {
		if !strings.Contains(s, kind) {
			t.Errorf("client does not handle the %q event; the user gets no indication of "+
				"why the next turn stalled", kind)
		}
	}
}

// The device that sent a turn also receives it back over the stream. Without origin-based
// suppression every message renders twice.
func TestClientSuppressesOwnEcho(t *testing.T) {
	s := clientHTML(t)
	if !strings.Contains(s, "CLIENT_ID") || !strings.Contains(s, "ev.origin") {
		t.Error("client does not compare event origin against its own id; its own turns will " +
			"render twice")
	}
}

// The original bug. overscroll-behavior on an inner scroller only prevents chaining out of
// that element; pull-to-refresh belongs to the ROOT scroller, so this rule has to be on html.
func TestClientDisablesPullToRefresh(t *testing.T) {
	s := clientHTML(t)
	norm := strings.Join(strings.Fields(s), " ")
	if !strings.Contains(norm, "html { overscroll-behavior-y:") &&
		!strings.Contains(norm, "html{overscroll-behavior-y:") {
		t.Error("no overscroll-behavior-y rule on html: a swipe starting outside #log will " +
			"still pull-to-refresh and discard the page")
	}
}

// localStorage is the right place for WHICH session is current and the wrong place for the
// conversation. If a transcript ever starts being written there, this experiment has been
// re-run the wrong way.
func TestClientDoesNotStoreTranscriptLocally(t *testing.T) {
	s := clientHTML(t)
	if strings.Contains(s, `localStorage.setItem("vb.history`) ||
		strings.Contains(s, "JSON.stringify(history)") {
		t.Error("the transcript is being written to localStorage; it belongs on the server, " +
			"which is what makes it survive a refresh AND follow you between devices")
	}
	if !strings.Contains(s, `localStorage.setItem("vb.session"`) {
		t.Error("the current session id is not persisted; every refresh will start a new " +
			"conversation, which is the bug this was meant to fix")
	}
}

// A syntax error in the client is invisible to `go build` and to every other test here: the
// page just fails to boot. Uses node when it is available and skips otherwise, so the gate is
// free on machines that have it and does not become a dependency on machines that do not.
func TestClientJavaScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping client syntax check")
	}
	s := clientHTML(t)
	const open = `<script type="module">`
	i := strings.Index(s, open)
	j := strings.LastIndex(s, "</script>")
	if i < 0 || j <= i {
		t.Fatal("could not find the module script in the client")
	}
	js := s[i+len(open) : j]

	f, err := os.CreateTemp(t.TempDir(), "client*.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(js); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, err := exec.Command(node, "--check", f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("client JavaScript does not parse:\n%s", out)
	}
}
