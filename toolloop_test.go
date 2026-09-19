package main

import (
	"encoding/json"
	"testing"
)

func delta(t *testing.T, js string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("bad test fixture %s: %v", js, err)
	}
	return m
}

// THE REGRESSION. The forwarding rule used to enumerate the fields it would pass
// through -- "content" and "reasoning" -- so a model streaming its thinking as
// "reasoning_content" had every one of those frames dropped. Nothing errored:
// they were received, parsed, matched nothing, and vanished. Reasoning simply
// stopped appearing once tools became the default and this path started running.
func TestReasoningIsForwardedWhateverTheFieldIsCalled(t *testing.T) {
	for _, js := range []string{
		`{"reasoning":"thinking out loud"}`,
		`{"reasoning_content":"thinking out loud"}`,
		`{"content":"","reasoning_content":"thinking out loud"}`,
		`{"role":"assistant","reasoning_content":"x"}`,
	} {
		if !hasUserVisibleDelta(delta(t, js)) {
			t.Errorf("dropped a delta carrying reasoning: %s", js)
		}
	}
}

// An unrecognised field must be forwarded, not withheld. A key this proxy has
// never heard of is far likelier to be output a newer model added than something
// that ought to be hidden -- and guessing wrong in that direction is invisible,
// which is what made the reasoning_content bug take a day to notice.
func TestUnknownFieldsAreForwardedNotSwallowed(t *testing.T) {
	for _, js := range []string{
		`{"thinking":"some future field"}`,
		`{"audio":{"data":"..."}}`,
		`{"content":"hi","some_2027_field":42}`,
	} {
		if !hasUserVisibleDelta(delta(t, js)) {
			t.Errorf("swallowed an unrecognised field: %s", js)
		}
	}
}

// The filter's ONLY job: withhold tool-call plumbing. Arguments arrive as
// partial JSON that parses only once concatenated, and a half-built function
// name rendered into the transcript is noise -- spoken aloud it is gibberish.
func TestPureToolCallDeltasAreWithheld(t *testing.T) {
	for _, js := range []string{
		`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"web_sea","arguments":""}}]}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"{\"qu"}}]}`,
		`{"role":"assistant","tool_calls":[{"index":0,"function":{"arguments":"ery\":"}}]}`,
		`{"role":"assistant"}`,
		`{}`,
	} {
		if hasUserVisibleDelta(delta(t, js)) {
			t.Errorf("forwarded pure tool-call plumbing: %s", js)
		}
	}
}

// Empty strings and nulls are not output. A model that sends {"content":""}
// alongside a tool call should not produce a visible frame.
func TestEmptyAndNullValuesDoNotCountAsOutput(t *testing.T) {
	for _, js := range []string{
		`{"content":""}`,
		`{"content":null}`,
		`{"reasoning":null,"content":""}`,
		`{"content":"","tool_calls":[{"index":0,"function":{"arguments":"x"}}]}`,
	} {
		if hasUserVisibleDelta(delta(t, js)) {
			t.Errorf("treated an empty value as output: %s", js)
		}
	}
}

// ...but a single space IS output: it is a real token the model emitted, and
// dropping it would silently corrupt spacing in the reply.
func TestWhitespaceContentIsOutput(t *testing.T) {
	if !hasUserVisibleDelta(delta(t, `{"content":" "}`)) {
		t.Error("dropped a space, which is a token the model actually emitted")
	}
}

func TestOpenAIToolsRendersACatalog(t *testing.T) {
	out := OpenAITools([]MCPTool{
		{Name: "web_search_free", Description: "search", InputSchema: map[string]any{"type": "object"}},
		{Name: "date_now", Description: "the date"}, // no schema: must be filled in
	})
	if len(out) != 2 {
		t.Fatalf("rendered %d tools, want 2", len(out))
	}
	fn, _ := out[1]["function"].(map[string]any)
	if fn["name"] != "date_now" {
		t.Errorf("wrong name: %v", fn["name"])
	}
	// A nil schema must become a valid empty object, or the request is rejected
	// by the backend rather than by us.
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("missing parameters for a tool with no schema: %#v", fn)
	}
}

func TestFirstLineOfTrimsForAChatMessage(t *testing.T) {
	if got := firstLineOf("found 10 results\nline two\nline three", 80); got != "found 10 results" {
		t.Errorf("got %q", got)
	}
	if got := firstLineOf("aaaaaaaaaa", 4); got != "aaaa…" {
		t.Errorf("got %q", got)
	}
}
