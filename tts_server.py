#!/usr/bin/env python3
"""Piper text-to-speech as a small local HTTP service.

Why a service and not a subprocess per request: loading a voice takes ~2s and
synthesising a sentence takes ~0.1s. Paying the load on every request would make
speech twenty times slower than the model doing the work. Voices are loaded once
and kept.

Why Piper and not Kokoro: measured on this box, Piper runs 12-18x realtime on
the CPU (64-119 ms for a short reply), so it needs no GPU at all -- which matters
because the GPU here is usually full of llama-server, and the browser could not
get a WebGPU adapter even when it was free.

Speaking rate is applied at SYNTHESIS via length_scale, not by resampling the
result. Changing playback speed shifts pitch; changing length_scale does not.

Endpoints:
  GET  /voices          -> {"voices": [{"id","name","lang","quality"}...]}
  POST /tts             <- {"text","voice","rate"}   -> audio/wav
"""

import io
import json
import logging
import threading
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from piper import PiperVoice, SynthesisConfig

VOICE_DIR = Path(__file__).parent / "voices"
MAX_TEXT = 4000

log = logging.getLogger("tts")
_voices: dict[str, PiperVoice] = {}
_lock = threading.Lock()


def available() -> list[dict]:
    out = []
    for p in sorted(VOICE_DIR.glob("*.onnx")):
        vid = p.stem                      # e.g. en_US-lessac-medium
        lang, _, rest = vid.partition("-")
        name, _, quality = rest.partition("-")
        # num_speakers matters: en_US-libritts-high ships 904 of them, and a
        # service that never sends speaker_id renders speaker 0 forever --
        # 903 voices on disk that nothing can reach.
        n = 1
        try:
            n = int(json.loads((p.parent / (p.name + ".json")).read_text()).get("num_speakers", 1))
        except Exception:  # noqa: BLE001 - a missing config is not fatal
            pass
        out.append({"id": vid, "name": name or vid, "lang": lang,
                    "quality": quality, "speakers": n})
    return out


def get_voice(vid: str) -> PiperVoice:
    # Double-checked under a lock: two concurrent first-requests for the same
    # voice would otherwise both pay the 2s load and one would be discarded.
    v = _voices.get(vid)
    if v is not None:
        return v
    with _lock:
        v = _voices.get(vid)
        if v is None:
            path = VOICE_DIR / f"{vid}.onnx"
            if not path.exists():
                raise KeyError(vid)
            log.info("loading voice %s", vid)
            v = PiperVoice.load(str(path))
            _voices[vid] = v
    return v


def synth(text: str, vid: str, rate: float, volume: float = 1.0,
          speaker: int | None = None, noise_w: float | None = None,
          noise: float | None = None) -> bytes:
    voice = get_voice(vid)
    # length_scale is duration per phoneme, so it is the INVERSE of speed.
    #
    # noise_w_scale is phoneme-duration jitter. Every voice here ships 0.8, and
    # it is the parameter that reads as "robotic" when low -- flat, metronomic
    # timing. It is exposed rather than tuned because naturalness is a listening
    # judgement, not a number this file should decide.
    cfg = SynthesisConfig(
        length_scale=max(0.3, min(3.0, 1.0 / max(0.25, rate))),
        volume=max(0.1, min(1.0, volume)),
    )
    if speaker is not None:
        n = next((v["speakers"] for v in available() if v["id"] == vid), 1)
        sp = int(speaker)
        # A bad speaker id is the caller's mistake, not a server fault: say so
        # with the range rather than letting onnxruntime raise a 500.
        if sp < 0 or sp >= n:
            raise ValueError(f"speaker {sp} out of range for {vid}: 0..{n - 1}")
        cfg.speaker_id = sp
    if noise_w is not None:
        cfg.noise_w_scale = max(0.0, min(2.0, float(noise_w)))
    if noise is not None:
        cfg.noise_scale = max(0.0, min(2.0, float(noise)))
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        voice.synthesize_wav(text, w, syn_config=cfg)
    return buf.getvalue()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        log.info("%s", fmt % args)

    def _send(self, code, body: bytes, ctype="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/") in ("/voices", "/tts/voices"):
            self._send(200, json.dumps({"voices": available()}).encode())
        elif self.path.rstrip("/") == "/health":
            self._send(200, b'{"ok":true}')
        else:
            self._send(404, b'{"error":"not found"}')

    def do_POST(self):
        if self.path.rstrip("/") not in ("/tts", "/tts/speak"):
            self._send(404, b'{"error":"not found"}')
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
            req = json.loads(self.rfile.read(n) or b"{}")
            text = (req.get("text") or "").strip()[:MAX_TEXT]
            if not text:
                self._send(400, b'{"error":"no text"}')
                return
            vid = req.get("voice") or (available()[0]["id"] if available() else "")
            def optf(k):
                v = req.get(k)
                return None if v is None else float(v)
            wav = synth(text, vid, float(req.get("rate") or 1.0),
                        float(req.get("volume") or 1.0),
                        speaker=(None if req.get("speaker") is None else int(req["speaker"])),
                        noise_w=optf("noise_w"), noise=optf("noise_scale"))
            self._send(200, wav, "audio/wav")
        except ValueError as e:
            self._send(400, json.dumps({"error": str(e)}).encode())
        except KeyError as e:
            self._send(404, json.dumps({"error": f"no such voice {e}"}).encode())
        except Exception as e:  # noqa: BLE001 - report, never take the service down
            log.exception("tts failed")
            self._send(500, json.dumps({"error": str(e)}).encode())


def main():
    import argparse

    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8123)
    ap.add_argument("--preload", default="all",
                    help="comma-separated voice ids to load at start, or 'all' for every "
                         "installed voice (the default). Naming voices here would make the "
                         "unit file a second place to edit when one is added or removed; the "
                         "voices directory is the configuration.")
    a = ap.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    vs = available()
    log.info("piper tts on %s:%d — %d voices in %s", a.listen, a.port, len(vs), VOICE_DIR)
    for v in vs:
        log.info("  %s  (%s, %s%s)", v["id"], v["lang"], v["quality"],
                 f", {v['speakers']} speakers" if v["speakers"] > 1 else "")
    wanted = [v["id"] for v in vs] if a.preload.strip() == "all" \
        else [x.strip() for x in a.preload.split(",") if x.strip()]
    for vid in wanted:
        try:
            get_voice(vid)
        except Exception as e:  # noqa: BLE001
            log.warning("preload %s failed: %s", vid, e)
    ThreadingHTTPServer((a.listen, a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
