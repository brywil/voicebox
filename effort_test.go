package main

import "testing"

// Assuming the vocabulary was the bug. Ornith-1.5's template accepts none, off,
// minimal, low, medium, high, xhigh and more -- and 'none' sets
// _initial_thinking = false, a real off switch. Offering only low/medium/high
// left a model that reasons for thousands of characters with no way to stop.
func TestLevelsAreReadFromTheTemplateNotAssumed(t *testing.T) {
	tmpl := `{%- if _effort_raw in ('none', 'off') %}{%- set _initial_thinking = false %}
{%- elif _effort_raw in ('minimal', 'low') %}{%- set _initial_effort = 'low' %}
{%- elif _effort_raw in ('high', 'xhigh', 'max') %}{%- set _initial_effort = 'xhigh' %}
{%- else %}{%- set _initial_effort = 'medium' %}`
	got := levelsFromTemplate(tmpl)
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q want %q (order is cheapest-first)", i, got[i], want[i])
		}
	}
	if got[0] != "none" {
		t.Error("no off switch offered for a template that has one")
	}
}

// A template naming one vocabulary word is a coincidence, not a vocabulary --
// most likely its own default appearing in an unrelated line. Keep the caller's
// default rather than offering a one-item dropdown.
func TestASingleVocabularyWordIsNotAVocabulary(t *testing.T) {
	if got := levelsFromTemplate(`{%- set _default = 'medium' %}`); got != nil {
		t.Errorf("inferred %v from one word", got)
	}
	if got := levelsFromTemplate(`no vocabulary at all here`); got != nil {
		t.Errorf("inferred %v from nothing", got)
	}
}
