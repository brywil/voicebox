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

// TestClientHasNoUseBeforeDeclaration catches the bug class that broke every message.
//
//	curThink = null;
//	thinking = "";
//	let full = "", thinking = "";
//
// The assignment on the second line precedes the `let` that declares the same name in the same
// block, which is a temporal dead zone violation: V8 throws "Cannot access 'thinking' before
// initialization" the moment the line runs. It sat at the top of send(), before the fetch, so
// every single turn failed and nothing ever reached the server -- the server log was empty,
// which is what made it look like anything other than a client bug.
//
// TestClientJavaScriptParses cannot catch this and never could: the code is syntactically
// valid, and node exits 0 parsing it. A TDZ violation is a RUNTIME error on a line that a parse
// never executes. So this walks the script's block scopes instead and reports any name assigned
// before it is declared in the same scope.
//
// The scanner self-checks at the end: it re-inserts the original bad line into a copy and
// requires that it is found. Without that, a scanner that quietly stops matching anything looks
// exactly like a clean file.
func TestClientHasNoUseBeforeDeclaration(t *testing.T) {
	src := moduleScript(t, clientHTML(t))
	if bad := useBeforeDeclaration(src); len(bad) > 0 {
		for _, b := range bad {
			t.Errorf("%s is assigned before the `let`/`const` that declares it in the same "+
				"scope: this throws \"Cannot access '%s' before initialization\" at runtime, "+
				"and the surrounding function stops there", b, b)
		}
	}

	// The scanner must actually be able to find one.
	broken := strings.Replace(src, "  curThink = null;\n  let full",
		"  curThink = null;\n  thinking = \"\";\n  let full", 1)
	if broken == src {
		t.Skip("send()'s preamble has moved; the self-check anchor needs updating")
	}
	if got := useBeforeDeclaration(broken); len(got) == 0 {
		t.Error("the scanner found nothing in a copy with the original bug re-inserted, so a " +
			"green result here means nothing")
	}
}

// moduleScript returns the contents of the page's <script type="module">.
func moduleScript(t *testing.T, html string) string {
	t.Helper()
	const open = `<script type="module">`
	i := strings.Index(html, open)
	j := strings.LastIndex(html, "</script>")
	if i < 0 || j <= i {
		t.Fatal("could not find the module script in the client")
	}
	return html[i+len(open) : j]
}

// useBeforeDeclaration reports names assigned before their let/const declaration in the SAME
// block scope. Scopes are identified by their opening brace, so sibling blocks and nested
// functions are separate and cannot produce a false positive.
func useBeforeDeclaration(src string) []string {
	src = blankLiterals(src)
	type item struct {
		scope, pos int
		name       string
	}
	var decls, assigns []item
	stack, next := []int{0}, 1

	ident := func(s string, k int) (string, int) {
		st := k
		for k < len(s) && (s[k] == '_' || s[k] == '$' ||
			s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z' ||
			(k > st && s[k] >= '0' && s[k] <= '9')) {
			k++
		}
		return s[st:k], k
	}
	isWord := func(s string, k int) bool {
		if k < 0 || k >= len(s) {
			return false
		}
		c := s[k]
		return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}

	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '{':
			stack = append(stack, next)
			next++
		case src[i] == '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case (strings.HasPrefix(src[i:], "let ") || strings.HasPrefix(src[i:], "const ")) && !isWord(src, i-1):
			// Every name in the declaration list, so `let a = 1, b = 2` records both.
			k := i + strings.IndexByte(src[i:], ' ') + 1
			for k < len(src) && src[k] != ';' && src[k] != '\n' {
				for k < len(src) && (src[k] == ' ' || src[k] == ',') {
					k++
				}
				name, end := ident(src, k)
				if name == "" {
					break
				}
				decls = append(decls, item{stack[len(stack)-1], i, name})
				// Skip this initialiser: its own `name =` is part of the declaration.
				depth := 0
				for end < len(src) && (depth > 0 || (src[end] != ',' && src[end] != ';' && src[end] != '\n')) {
					switch src[end] {
					case '(', '[', '{':
						depth++
					case ')', ']', '}':
						depth--
					}
					end++
				}
				k = end
			}
			i = k - 1
		case src[i] == '=' && i+1 < len(src) && src[i+1] != '=' && !isWord(src, i-1):
			if i > 0 && strings.ContainsRune("=!<>+-*/%&|^", rune(src[i-1])) {
				continue // comparison or compound assignment handled by the same rule below
			}
			k := i - 1
			for k >= 0 && (src[k] == ' ' || src[k] == '\t') {
				k--
			}
			end := k + 1
			for k >= 0 && isWord(src, k) {
				k--
			}
			if name := src[k+1 : end]; name != "" && !isWord(src, k) {
				assigns = append(assigns, item{stack[len(stack)-1], k + 1, name})
			}
		}
	}

	var out []string
	for _, a := range assigns {
		for _, d := range decls {
			if d.scope == a.scope && d.name == a.name && d.pos > a.pos {
				out = append(out, a.name)
				break
			}
		}
	}
	return out
}

// blankLiterals replaces comments, strings, template literals and regex literals with spaces,
// keeping every byte offset intact. Without it a brace inside a string or a `let` inside a
// comment would be read as code and the scope tracking would drift.
func blankLiterals(s string) string {
	b := []byte(s)
	blank := func(from, to int) {
		for i := from; i < to && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	prevCode := func(i int) byte {
		for i--; i >= 0; i-- {
			if b[i] != ' ' && b[i] != '\t' && b[i] != '\n' {
				return b[i]
			}
		}
		return 0
	}
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			j := i
			for j < len(b) && b[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			j := i + 2
			for j+1 < len(b) && !(b[j] == '*' && b[j+1] == '/') {
				j++
			}
			blank(i, j+2)
			i = j + 1
		case b[i] == '"' || b[i] == '\'' || b[i] == '`':
			q := b[i]
			j := i + 1
			for j < len(b) && b[j] != q {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			blank(i, j+1)
			i = j
		case b[i] == '/':
			// A regex literal, but only where a value cannot precede it -- otherwise this is
			// division. Getting it wrong the other way would blank real code.
			if p := prevCode(i); p == 0 || strings.ContainsRune("(,=:[!&|?{};+-*%<>~^", rune(p)) {
				j := i + 1
				for j < len(b) && b[j] != '/' && b[j] != '\n' {
					if b[j] == '\\' {
						j++
					}
					j++
				}
				if j < len(b) && b[j] == '/' {
					blank(i, j+1)
					i = j
				}
			}
		}
	}
	return string(b)
}
