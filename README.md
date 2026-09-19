# voicebox

A browser voice interface for your own LLM servers. Hold a button, talk, hear the
answer back. Speech recognition runs in the browser; the model runs wherever you
point it — llama-server, local ollama, or ollama cloud.

## Why there is a server at all

The page could in principle call llama-server directly. Three browser rules stop
that, and a same-origin proxy is the only thing that clears all three at once:

- **The microphone needs a secure context** — `https://`, or `localhost`. A bare
  LAN IP is neither, and `getUserMedia` simply refuses.
- **An https page may not call an http backend.** So "just put TLS on the page"
  is a dead end while llama-server speaks plain http.
- **Cross-origin calls need CORS.** llama-server happens to send it; a cloud
  endpoint on another host still costs a preflight.

Serving the page and the API from one origin removes all three. It also keeps
the ollama cloud API key on the server side instead of in a browser tab, where
any extension can read it.

## Run

Installed as two systemd user units — `voicebox-tts` (Piper) and `voicebox`
(page + proxy), the second a soft dependency of the first so the page still
works without speech:

```sh
cp systemd/*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now voicebox-tts voicebox
```

`OLLAMA_API_KEY` comes from `~/.config/voicebox/voicebox.env` (mode 600), read by
the unit and injected upstream by the proxy, so it never reaches a browser tab.

`./run.sh` does the same thing in the foreground for development. First time
only:

```sh
python3 -m venv .venv && ./.venv/bin/pip install piper-tts
./.venv/bin/python -m piper.download_voices en_US-lessac-medium --data-dir voices
```

For ollama cloud, export the key named by `api_key_env` first:

```sh
export OLLAMA_API_KEY=...              # already in ~/.bashrc on the Xeon
```

`/api/backends` reports whether a key is *configured*, never the key itself.

## On a phone

Served over the tailnet with a real certificate:

```sh
sudo tailscale set --operator=$USER      # once, so serve needs no root after
tailscale serve --bg 8080
```

→ `https://<machine>.<your-tailnet>.ts.net` (tailnet only, Let's Encrypt).
`tailscale serve status` prints the exact name.

The certificate is the point, not the convenience: **the microphone needs a
secure context**, and a LAN IP over http is not one. A self-signed cert does not
help either. `tailscale serve` also puts the page and `/v1`, `/tts`, `/voices`
on one origin, so nothing needs CORS or a second host.

The architecture pays off here: Piper is server-side, so the phone downloads no
voice model, and choosing **Chrome (on-device)** for speech-in means no Whisper
download either. The phone fetches essentially just the page.

Touch sizing is gated on `(pointer: coarse)`, not a width breakpoint — what
decides it is a fingertip, not a small window. Press-and-hold is a mouse idiom,
so a coarse pointer gets tap-to-start/tap-to-stop instead: a finger that drifts
off the button would otherwise cancel the take, and the synthesised mouse events
Android fires after a touch re-enter the same handlers.

Pick a backend that answers quickly. `qwen38-27b` on the Xeon measured **69 s to
first token** (27B on a 16 GB card, heavily CPU-offloaded), which reads as a
broken page rather than a slow one.

## The microphone and the LAN

Serving on `:8080` makes the page reachable from the LAN, but **the mic only
works from a secure context**. So:

| you browse from | page URL | mic |
|---|---|---|
| the machine running voicebox | `http://localhost:8080` | ✅ works |
| another machine on the LAN | `http://<lan-ip>:8080` | ❌ blocked |

Typing still works everywhere. To get the mic from another machine, either put
TLS in front (Caddy, or `tailscale serve`, which issues a real cert), or start
Chrome there with the origin whitelisted:

```sh
google-chrome \
  --unsafely-treat-insecure-origin-as-secure=http://<lan-ip>:8080 \
  --user-data-dir=/tmp/vb-profile
```

That flag needs its own profile dir, and it is a testing aid, not a deployment.

## What runs where

| piece | where | cost |
|---|---|---|
| speech → text | browser, `whisper-tiny.en` via transformers.js | ~40 MB, cached after first run |
| the model | your server | 0 bytes in the browser |
| text → speech | browser, built-in `speechSynthesis` | 0 bytes |

WebGPU is used for speech recognition when an adapter is actually obtainable,
and falls back to CPU otherwise. The check calls `requestAdapter()` rather than
testing that `navigator.gpu` exists — the object is present on any modern Chrome
even where no adapter can be had, and asking for `webgpu` anyway throws from
inside onnxruntime in a parallel `Promise.all` whose sibling rejections escape
any try/catch the caller writes.

The CPU path is threaded. voicebox serves the page with `Cross-Origin-Opener-
Policy: same-origin` and `Cross-Origin-Embedder-Policy: credentialless`, which
makes the page cross-origin isolated, which is what unlocks `SharedArrayBuffer`
and therefore multi-threaded WASM. Without those headers onnxruntime runs on one
thread and is slow enough to look hung rather than slow.

COEP is `credentialless`, not `require-corp`: require-corp demands a CORP header
on every cross-origin resource, and neither the jsdelivr CDN nor the HuggingFace
model files send one, so it would break the downloads it is meant to accelerate.
Verified isolated with the CDN still importing (`crossOriginIsolated=true`,
`SharedArrayBuffer=true`, 963 exports imported).

At this size the CPU fallback is genuinely fine — that is the whole point of not
putting a 4B multimodal model in the browser.

## Tools

Off by default; the switch appears only when a tool server is configured. With
it on, the model can search the web, save and recall memories, and ask the date
— the six tools on the restricted mymcp instance (see `docs/MYMCP.md`).

**The loop runs in the proxy, never the browser.** mymcp is loopback-bound, so a
page could not reach it at all, and the bearer token must not exist in a document
anything on the tailnet can read. `/v1/chat/completions` is therefore *handled*
rather than forwarded when tools are in play: ask the model, receive tool calls,
run them, feed the results back, ask again. Everything else on `/v1` still passes
straight through, and with tools off the request is proxied exactly as before.

The browser still sees one continuous SSE stream. Content and reasoning deltas
pass through untouched — the page's parser needs no special case — and tool
activity is added as extra frames tagged `voicebox.event` that the model never
sees. Tool-call deltas are accumulated rather than forwarded: arguments arrive as
partial JSON that only parses once concatenated, and a half-built function name
spoken aloud would be gibberish.

**Calls are announced in the aside voice.** Silence is far worse in audio than on
screen — thirty seconds of nothing during a search is indistinguishable from a
hang when there is no spinner. Failures are spoken too; successes are not, since
the answer that follows is the report.

`max_rounds` (default 4) bounds the loop. A model that keeps calling tools is a
real failure mode and an expensive one here, since every round is latency heard
as silence. Hitting the bound is announced rather than passed off as a finished
answer.

## Voice vs typed

Dictated messages are prefixed `[voice]`, behind a one-line system note. It
helps the model read past recogniser artefacts: homophones, mangled proper nouns,
missing punctuation.

Measured working on a capable model — `[voice] whats the capital of sweeden` and
glm-5.3-flash's own reasoning read "the voice transcription shows 'sweeden' which
is a misspelling ... a typical speech-to-text artifact", then answered plainly.

**Only voice is tagged, and the note is terse, because the obvious richer version
measurably broke a small model.** Tagging `[typed]` as well and explaining both
cases made granite-3.1-1b echo the marker into its own replies ("[typed] As of
2023...") and, once it commented on a message's phrasing, keep doing so for the
rest of the conversation — escalating to suggesting rephrasings instead of
answering. Absence of a tag carries "typed" perfectly well, and a weak model has
less to imitate. The lean version also recovered from a history already poisoned
with that behaviour, where the verbose one did not.

An echoed marker is stripped from replies anyway, and the tag never appears on
screen — you know how you entered it.

## A note on model size

If answers are confidently wrong, check which backend is selected before
suspecting the plumbing. granite-3.1-1b-a400m is a nano model with 400M active
parameters; asked for the bicycle land speed record it named, across runs, four
different people who never held it. That is the model, not the prompt and not
the transcription.

## Speech engines

| engine | where the audio goes | download | page touches the mic? |
|---|---|---|---|
| **keyboard (Gboard)** | nowhere — the keyboard's own recogniser | none | **no** |
| Chrome, on-device | nowhere — stays on the machine | Chrome's language pack | yes |
| Chrome, cloud | Google | none | yes |
| Whisper tiny/base/small | nowhere | 40 / 80 / 250 MB | yes |

**Keyboard dictation** uses Gboard's own recogniser — on-device, already tuned
for that phone. But note what it cannot be: **a page cannot raise the keyboard's
dictation.** `x-webkit-speech` was removed years ago and nothing replaced it, so
the flow is inherently two taps — open the field, then tap the keyboard's mic.
In this mode the talk button is hidden entirely and the text field takes the
row, because a large button whose only power is focusing a field you could tap
yourself is worse than no button.

**For one tap, use `Chrome (on-device)` instead.** On Android that is Google's
recogniser — the same family behind Gboard's voice typing — reached through the
Web Speech API, so the quality is comparable and the page can start and stop it
directly.

It also avoids a conflict rather than managing one. **The microphone is held only
while actually recording**, and not at all in keyboard mode. An earlier version
acquired it at load and never released the stream, which on Android blocks every
other consumer — the keyboard reported "another device is using the microphone"
purely because this page had taken it. Dropping the reference is not enough; the
tracks must be stopped, which is what hands the device back.

Chrome's recogniser is the same Google speech stack behind Gboard's voice typing;
[Chrome 139 added an on-device mode](https://developer.mozilla.org/en-US/docs/Web/API/SpeechRecognition/available_static)
so audio need not leave the machine. The engine dropdown says which mode is
actually active, because that difference decides where your voice goes.

## Voices and speed

Three engines, and the default is the server one:

| engine | runs | download | speed |
|---|---|---|---|
| **Piper (server)** | on this machine's CPU | none in the browser | **12–18x realtime** |
| system | browser / OS | none | instant |
| Kokoro | in the browser | 86 MB | ~1x on WASM, too slow to use |

**Piper is the default because the browser could not get a WebGPU adapter here**
even with 14 GB free on the card, which left Kokoro on single-threaded WASM and
slower than playback. Piper needs no GPU at all — measured on this box it does
12–18x realtime on the CPU, 64–119 ms for a short reply, so the GPU stays free
for llama-server. It is also the only engine that will work from a phone, since
the phone would have to download and run the model otherwise.

Voices are loaded once and kept: the first request for a voice pays ~2s, every
one after is ~0.2s for a short sentence. Add more with

```sh
./.venv/bin/python -m piper.download_voices en_GB-cori-high --data-dir voices
```

Speed is 0.5x–2.5x. For Piper it is applied at **synthesis** (`length_scale`),
so faster speech is genuinely faster rather than pitch-shifted the way changing
playback rate would make it. For the system voice it applies per utterance;
Kokoro uses playback rate.

Note for anyone reading the kokoro-js docs: `list_voices()` only calls
`console.table()` and returns **undefined**. The data is the `voices` getter.

## The interface

The controls live behind `☰`. There are thirteen of them now — server, model,
thinking level, input tagging, recogniser, auto-send, speech engine, voice,
speed, speak-replies, speak-thinking, aside voice, clear — and a single bar of
them had stopped being scannable and did not fit a narrow window at all.

What stays visible is a crumb line reading `server · model · voice · thinking`,
because once the selects are hidden there is otherwise no way to tell which
model just answered you. Esc closes the panel if it is open, and stops the
speech if it is not.

Rows that do not apply are hidden rather than disabled: the thinking-level row
only appears when the model has such a control, the aside voice only when
reading the reasoning aloud is switched on.

## What gets spoken

Markdown is written to be read, not spoken. Piper says "asterisk asterisk" for
`**bold**`, spells URLs out character by character, and reads a fenced code block
line by line including the backticks. The text is therefore cleaned on the way to
the synthesiser only — the transcript on screen keeps its formatting.

Code blocks become "(code block)", links keep their text and lose their URL,
tables are dropped outright. Things that merely look like markup are left alone:
`2 * 3 * 4` and `snake_case_name` survive intact, which is most of why the
patterns are fussier than they first appear.

**Interrupting.** A reasoning model can produce minutes of speech. `⏹` or **Esc**
stops it dead and clears the queue; starting to talk does too, so barging in
works. Without that the only options were waiting it out or reloading.

**Thinking aloud.** With "speak thinking" on, the model's reasoning is read in a
*different voice*, quieter (volume 0.65, measured: peak amplitude 32767 → 21298)
and slightly faster. It is an aside, and it should sound like one — otherwise a
long chain of thought is indistinguishable from the answer and you cannot hear
where the reply starts. When the answer does begin, any unspoken reasoning is
dropped rather than queued in front of it.

## Thinking level

The dropdown is **discovered, not assumed**. There is no single way to ask a
model how hard to think, so the server interrogates the backend and reports what
that model actually honours. Three answers, most direct first:

| source | mechanism | seen on |
|---|---|---|
| llama.cpp `/props` → `chat_template_caps.supports_reasoning_effort` | `reasoning_effort` | models declaring it |
| llama.cpp `/props` → `chat_template` scanned for a knob variable | `chat_template_kwargs` | granite → `enable_thinking` |
| ollama `/api/show` → `capabilities` contains `thinking` | `reasoning_effort` | glm-5.3-flash |

Families spell it differently — Muse-Glimmer `reasoning_strength`, gpt-oss
`reasoning_effort`, Qwen3 a boolean `enable_thinking` — and most models have no
such knob, so a fixed list would be wrong more often than right. The control is
hidden entirely when nothing is supported, and re-asked whenever the model
changes, because the capability belongs to the weights rather than the server.

**The value must be the JSON type the template expects.** llama.cpp validates and
rejects a string standing in for a boolean:

```
invalid type for "enable_thinking" (expected boolean, got string)
```

so `true`/`false` levels are converted to real booleans while `low`/`medium`/
`high` stay strings.

Both mechanisms verified end to end. `enable_thinking=true` on granite returned
*empty content* within an 80-token cap — it spent the whole budget thinking.
On ollama cloud, three reps per level separate cleanly with no overlap:

| | reasoning chars | completion tokens |
|---|---|---|
| low | 89, 47, 47 | 136, 115, 111 |
| high | 119, 128, 119 | 242, 181, 232 |

A single pair had shown no difference at all, which is why this was repped
rather than quoted from one run.

## Reasoning models

Most of what ollama cloud serves streams a separate `reasoning` field alongside
`content`. voicebox shows reasoning collapsed and **never speaks it** — listening
to a model deliberate for thirty seconds before it answers is unusable. Only
`content` is read aloud.

## Config

```json
{
  "listen": ":8080",
  "default": "xeon-llama",
  "backends": [
    { "id": "xeon-llama", "label": "Xeon llama-server", "url": "http://127.0.0.1:8085" },
    { "id": "ollama-cloud", "label": "Ollama Cloud", "url": "https://ollama.com",
      "api_key_env": "OLLAMA_API_KEY" }
  ]
}
```

The browser picks a backend per request via an `X-Voicebox-Backend` header, so
switching servers mid-conversation costs nothing and needs no restart.

## Verified

- streaming is real, not buffered: first byte 11 ms against 2.4 s total
- ollama cloud reaches through the proxy with the key injected server-side
- reasoning/content split measured against `glm-5.3-flash` (83 chars reasoning,
  `proxy works` as content)
