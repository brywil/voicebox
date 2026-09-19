package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

// The cap and the template answer DIFFERENT questions, and conflating them was
// the bug. chat_template_caps.supports_reasoning_effort says only that a
// top-level reasoning_effort field is honoured -- the MECHANISM. Which values it
// accepts lives in the template. Returning a hardcoded low/medium/high when the
// cap fired short-circuited the scan entirely, so on a llama.cpp build new
// enough to populate that cap, Ornith-1.5 lost its "none" and reasoning could
// not be turned off at all. Found by the Spark session, whose newer build sets
// the cap where this box's b10397 leaves it absent.
func TestCapDeclaresTheMechanismTemplateDeclaresTheLevels(t *testing.T) {
	tmpl := `{%- if _effort_raw in ('none', 'off') %}{%- set _initial_thinking = false %}
{%- elif _effort_raw in ('minimal', 'low') %}
{%- elif _effort_raw in ('high', 'xhigh') %}
{%- else %}{%- set _initial_effort = 'medium' %}`

	k := effortKnob{Kind: "reasoning_effort", Levels: effortLevels, Labels: effortLevels,
		Source: "llama.cpp chat_template_caps"}
	applyTemplateLevels(&k, tmpl)

	if k.Kind != "reasoning_effort" {
		t.Errorf("the cap's mechanism must survive: got %q", k.Kind)
	}
	if len(k.Levels) != 6 || k.Levels[0] != "none" {
		t.Fatalf("levels not taken from the template: %v", k.Levels)
	}
	if k.Labels[0] != "none" {
		t.Errorf("labels must track levels: %v", k.Labels)
	}
}

// With no template to read, the caller's assumed levels must stand rather than
// being blanked.
func TestNoTemplateLeavesTheAssumedLevelsAlone(t *testing.T) {
	k := effortKnob{Kind: "reasoning_effort", Levels: effortLevels, Labels: effortLevels}
	applyTemplateLevels(&k, "")
	if len(k.Levels) != 3 || k.Levels[0] != "low" {
		t.Errorf("clobbered the fallback: %v", k.Levels)
	}
}

// A boolean knob already carries its vocabulary; scanning would replace
// false/true with unrelated words that appear elsewhere in the template.
func TestBooleanKnobsKeepFalseTrue(t *testing.T) {
	k := effortKnob{Kind: "chat_template_kwargs", Variable: "enable_thinking",
		Levels: []string{"false", "true"}, Labels: []string{"off", "on"}}
	applyTemplateLevels(&k, `mentions 'none' and 'low' and 'high' for other reasons`)
	if len(k.Levels) != 2 || k.Levels[0] != "false" {
		t.Errorf("boolean knob was rewritten: %v", k.Levels)
	}
	if k.Labels[0] != "off" {
		t.Errorf("boolean labels lost: %v", k.Labels)
	}
}

// Guards the CALL SITE, not just the helper. The previous test exercised
// applyTemplateLevels directly, so deleting its call from the cap branch left
// the suite green — precisely the bug the Spark session hit. This stands up a
// /props that sets supports_reasoning_effort AND ships a template, which is the
// combination that short-circuited.
func TestDetectEffortUsesTemplateLevelsEvenWhenTheCapFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]any{"supports_reasoning_effort": true},
			"chat_template": `{%- if _effort_raw in ('none', 'off') %}{%- set _initial_thinking = false %}
{%- elif _effort_raw in ('minimal', 'low') %}{%- elif _effort_raw in ('high', 'xhigh') %}
{%- else %}{%- set _initial_effort = 'medium' %}`,
		})
	}))
	defer srv.Close()

	k := detectEffort(&Backend{ID: "t", URL: srv.URL}, "")
	if k.Kind != "reasoning_effort" {
		t.Fatalf("kind = %q, want reasoning_effort (the cap decides the mechanism)", k.Kind)
	}
	if len(k.Levels) != 6 || k.Levels[0] != "none" {
		t.Fatalf("levels = %v, want the template's six starting at none — the cap "+
			"must not short-circuit the template scan", k.Levels)
	}
}

// And the cap firing with NO template still yields the sane fallback.
func TestDetectEffortCapWithoutTemplateFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]any{"supports_reasoning_effort": true},
		})
	}))
	defer srv.Close()
	k := detectEffort(&Backend{ID: "t", URL: srv.URL}, "")
	if len(k.Levels) != 3 || k.Levels[0] != "low" {
		t.Errorf("levels = %v, want the low/medium/high fallback", k.Levels)
	}
}
