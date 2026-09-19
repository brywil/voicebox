package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Reasoning-budget discovery.
//
// There is no single way to ask a model how hard it should think, so this asks
// the SERVER what it supports rather than shipping a static list that is wrong
// for most models. Three answers are possible, in descending order of directness:
//
//   1. llama.cpp /props -> chat_template_caps.supports_reasoning_effort, a
//      straight declaration that the OpenAI-style reasoning_effort is honoured.
//   2. llama.cpp /props -> chat_template, scanned for the variable a family
//      spells its budget with. Muse-Glimmer reads reasoning_strength, gpt-oss
//      reasoning_effort, Qwen3 a boolean enable_thinking. These travel as
//      chat_template_kwargs, which llama.cpp json-parses per value, so "true"
//      arrives as a boolean rather than a string.
//   3. ollama /api/show -> capabilities contains "thinking". Ollama exposes no
//      template through its API, but it does declare the capability, and its
//      OpenAI endpoint honours reasoning_effort (measured: low produced 26
//      completion tokens against high's 37 on the same question).
//
// Detection is per backend AND per model: capability is a property of the
// loaded weights, not of the server.

type effortKnob struct {
	// Kind is how the value must be sent: "reasoning_effort" as a top-level
	// request field, or "chat_template_kwargs" as a template variable.
	Kind     string   `json:"kind"`
	Variable string   `json:"variable,omitempty"`
	Levels   []string `json:"levels,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Source   string   `json:"source"` // how it was discovered, for the tooltip
}

// templateKnobs are probed in order; the first whose variable appears in the
// template wins. Most specific first, so a template naming several resolves
// predictably. Lifted from goclaw, which detects the same thing for the same
// reason -- keep the two lists in step.
var templateKnobs = []effortKnob{
	{Variable: "reasoning_strength", Levels: []string{"low", "medium", "high"}, Labels: []string{"low", "medium", "high"}},
	{Variable: "reasoning_effort", Levels: []string{"low", "medium", "high"}, Labels: []string{"low", "medium", "high"}},
	{Variable: "thinking_budget", Levels: []string{"low", "medium", "high"}, Labels: []string{"low", "medium", "high"}},
	{Variable: "enable_thinking", Levels: []string{"false", "true"}, Labels: []string{"off", "on"}},
	{Variable: "thinking", Levels: []string{"false", "true"}, Labels: []string{"off", "on"}},
}

var effortLevels = []string{"low", "medium", "high"}

// effortVocab is the vocabulary a template might accept, cheapest first. Which
// of these a given model actually takes is read OUT of its chat template rather
// than assumed -- assuming low/medium/high cost Ornith-1.5 its "none", so a
// model that reasons for three thousand characters on a one-line question had no
// off switch, despite its template having one.
var effortVocab = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// applyTemplateLevels replaces a knob's assumed vocabulary with the one its
// template actually quotes, when that can be determined. The caller decides the
// KIND; only the template knows the LEVELS.
func applyTemplateLevels(k *effortKnob, tmpl string) {
	if strings.TrimSpace(tmpl) == "" {
		return
	}
	// Boolean knobs already carry their own two values.
	if len(k.Levels) == 2 && k.Levels[0] == "false" {
		return
	}
	if lv := levelsFromTemplate(tmpl); lv != nil {
		k.Levels, k.Labels = lv, lv
		k.Source += " (levels read from the template)"
	}
}

// levelsFromTemplate returns the vocabulary words the template actually quotes.
// Returns nil when it cannot tell, so the caller keeps its default.
func levelsFromTemplate(tmpl string) []string {
	var out []string
	for _, v := range effortVocab {
		if strings.Contains(tmpl, "'"+v+"'") || strings.Contains(tmpl, `"`+v+`"`) {
			out = append(out, v)
		}
	}
	// One word is not a vocabulary -- it is a coincidence, most likely the
	// template's own default appearing in an unrelated line.
	if len(out) < 2 {
		return nil
	}
	return out
}

func detectEffort(b *Backend, model string) effortKnob {
	none := effortKnob{Kind: "none", Source: "no reasoning control found"}
	cl := &http.Client{Timeout: 8 * time.Second}

	// --- llama.cpp ---
	if props, err := getJSON(cl, b, "/props", nil); err == nil {
		if caps, ok := props["chat_template_caps"].(map[string]any); ok {
			if v, _ := caps["supports_reasoning_effort"].(bool); v {
				k := effortKnob{Kind: "reasoning_effort", Levels: effortLevels,
					Labels: effortLevels, Source: "llama.cpp chat_template_caps"}
				// The cap answers only which MECHANISM is honoured -- a top-level
				// reasoning_effort field. It says nothing about which values that
				// field accepts, and returning a hardcoded low/medium/high here
				// short-circuited the template scan entirely: on a build that
				// populates this cap, Ornith-1.5 lost its "none" and there was no
				// way to turn reasoning off at all.
				tmpl, _ := props["chat_template"].(string)
				applyTemplateLevels(&k, tmpl)
				return k
			}
		}
		if tmpl, _ := props["chat_template"].(string); strings.TrimSpace(tmpl) != "" {
			for _, k := range templateKnobs {
				if strings.Contains(tmpl, k.Variable) {
					k.Kind = "chat_template_kwargs"
					k.Source = "found " + k.Variable + " in the chat template"
					applyTemplateLevels(&k, tmpl)
					return k
				}
			}
			return effortKnob{Kind: "none", Source: "chat template names no reasoning variable"}
		}
	}

	// --- ollama ---
	if model != "" {
		body, _ := json.Marshal(map[string]string{"model": model})
		if show, err := getJSON(cl, b, "/api/show", body); err == nil {
			if caps, ok := show["capabilities"].([]any); ok {
				for _, c := range caps {
					if s, _ := c.(string); s == "thinking" {
						return effortKnob{Kind: "reasoning_effort", Levels: effortLevels,
							Labels: effortLevels, Source: "ollama declares the thinking capability"}
					}
				}
				return effortKnob{Kind: "none", Source: "ollama does not list thinking for this model"}
			}
		}
	}
	return none
}

// getJSON calls an endpoint on a backend, POSTing when a body is supplied.
// Backend auth is applied the same way the proxy does it.
func getJSON(cl *http.Client, b *Backend, path string, body []byte) (map[string]any, error) {
	method := http.MethodGet
	var rdr io.Reader
	if body != nil {
		method, rdr = http.MethodPost, bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, strings.TrimRight(b.URL, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.APIKeyEnv != "" {
		if k := envOf(b.APIKeyEnv); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
