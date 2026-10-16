package main

import (
	"strings"
	"time"
)

// The system prompt voicebox sends on every chat request, built here rather than in the page so every
// client (the page, StoneChat, a car) gets the same one.
//
// Kept deliberately short and literal: the models behind voicebox are small, and a small model follows a
// few numbered rules far better than pages of guidance. Before voicebox sent this, most requests carried no
// system prompt at all, and the model wrote for a screen (lists, bold, code blocks) that speakableText()
// then had to strip for the speaker.

const spokenRules = `You are voicebox, a voice assistant. Your reply will be read aloud by text-to-speech. The listener is often driving and cannot look at a screen.

Rules:
1. Talk in plain sentences. Never use markdown: no lists, bullets, headings, bold, tables, or code blocks.
2. Answer first, in two to five short sentences, then stop. Offer more detail instead of giving it unasked.
3. Write numbers and symbols the way a person says them: "about two and a half gigabytes", "twenty percent".
4. Never read out code, file paths, links, or long IDs. Say what they are instead.
5. Ask at most one question, and only at the end.
6. If you used a tool, say so in a few words, like "I searched and found...". If you are not sure, say so. Never make up facts or results.`

const textRules = `You are voicebox, a chat assistant.

Rules:
1. Answer first and keep replies short.
2. If you used a tool, say so in a few words. If you are not sure, say so. Never make up facts or results.`

// voiceInputRule is the page's measured preamble, moved here unchanged. Only [voice] is tagged and the note is
// terse on purpose: a longer version that also tagged [typed] made granite 1b echo the marker into its replies
// and start commenting on (then rephrasing) the user's wording for the rest of the conversation. Saying less
// gives a weak model less to imitate.
const voiceInputRule = "A message beginning with [voice] was dictated by speech recognition and may " +
	"contain misheard words. Answer it normally. Never write [voice] yourself and " +
	"never remark on how the message was entered or worded."

const dictationRule = "The user may be dictating, so some words may be misheard. Answer what they most " +
	"likely meant. Never remark on how a message was worded."

// systemPrompt returns the prompt for one request. spoken: the reply will be read aloud. voiceInput: at
// least one user message was dictated (the page tags those "[voice] ").
func systemPrompt(spoken, voiceInput bool, now time.Time) string {
	var b strings.Builder
	if spoken {
		b.WriteString(spokenRules)
	} else {
		b.WriteString(textRules)
	}
	if voiceInput {
		b.WriteString("\n\n" + voiceInputRule)
	} else if spoken {
		// Gboard voice typing arrives as ordinary typed text, so voicebox cannot see dictation; a reply
		// that will be heard is the best evidence the question was spoken too. No tag is named here,
		// so there is nothing for a small model to echo.
		b.WriteString("\n\n" + dictationRule)
	}
	b.WriteString("\n\nCurrent local time: " + now.Format("Monday, January 2, 2006, 3:04 PM") + ".")
	return b.String()
}

// applySystemPrompt puts voicebox's prompt at the front of raw["messages"]. Any system messages the client
// sent (the page's compaction summary) are folded into it, because several system messages, or one that is
// not first, break some chat templates.
func applySystemPrompt(raw map[string]any, spoken bool, now time.Time) {
	msgs, _ := raw["messages"].([]any)
	var notes []string
	rest := make([]any, 0, len(msgs)+1)
	voiceInput := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			rest = append(rest, m)
			continue
		}
		if mm["role"] == "system" {
			if s, ok := mm["content"].(string); ok && strings.TrimSpace(s) != "" {
				notes = append(notes, strings.TrimSpace(s))
			}
			continue
		}
		if mm["role"] == "user" {
			if s, ok := mm["content"].(string); ok && strings.HasPrefix(s, "[voice] ") {
				voiceInput = true
			}
		}
		rest = append(rest, mm)
	}
	sys := systemPrompt(spoken, voiceInput, now)
	if len(notes) > 0 {
		sys += "\n\n" + strings.Join(notes, "\n\n")
	}
	raw["messages"] = append([]any{map[string]any{"role": "system", "content": sys}}, rest...)
}
