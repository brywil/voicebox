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

```sh
cp config.example.json config.json     # edit backends to taste
go build -o voicebox .
./voicebox                             # http://localhost:8080
```

For ollama cloud, export the key named by `api_key_env` first:

```sh
export OLLAMA_API_KEY=...              # already in ~/.bashrc on the Xeon
```

`/api/backends` reports whether a key is *configured*, never the key itself.

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

WebGPU is used for speech recognition when available and falls back to CPU
automatically. At this size the fallback is genuinely fine — that is the whole
point of not putting a 4B multimodal model in the browser.

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
