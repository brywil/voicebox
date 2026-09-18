#!/usr/bin/env bash
# Start both halves of voicebox: the Piper speech service and the web/proxy server.
set -euo pipefail
cd "$(dirname "$0")"

# The ollama cloud key never reaches the browser; the proxy injects it. Taken
# from the environment, or from opencode's auth store if it is not exported.
if [ -z "${OLLAMA_API_KEY:-}" ] && [ -f "$HOME/.local/share/opencode/auth.json" ]; then
  OLLAMA_API_KEY="$(python3 -c "
import json,os
print(json.load(open(os.path.expanduser('~/.local/share/opencode/auth.json'))).get('ollama-cloud',{}).get('key',''))" 2>/dev/null || true)"
  export OLLAMA_API_KEY
fi

[ -f config.json ] || cp config.example.json config.json
[ -x voicebox ] || go build -o voicebox .

# Preload one voice so the first spoken reply is not paying a 2s model load.
./.venv/bin/python tts_server.py --preload en_US-lessac-medium &
TTS_PID=$!
# Kill by PID, never by command-line match: a pattern match also selects this
# script, which is how you kill the thing doing the killing.
trap 'kill $TTS_PID 2>/dev/null || true' EXIT INT TERM

./voicebox "$@"
