#!/usr/bin/env python3
"""Preflight guard: a decision model asks one question of each user message.

Liquid's d1-3B answers "Is this statement nonsense?" as a probability, in one forward pass and with no output tokens.
voicebox appends a short note to a message the guard scores at or above its threshold. Why this question and this
model: measured in gguf-quant-lab/experiments/liquid-d1 and laya-guard -- the plain question beat a long definitional
one on held-out data (AUROC 0.963 vs 0.907), fired on 0-5% of ordinary math, code and instruction prompts, and on
Ornith a gated note took BullshitBench 0.86 -> ~0.90 for ~0.03 on sound questions.

Step 1, before the question: SPELLING. A single typo multiplies d1's nonsense false fires about 7x on sound sentences
("Ice ockey is a sport..." 0.06 -> 0.82); correcting it first with the masked-LM checker (typo_check.py, ModernBERT,
~35 ms) undid 22 of 31 such flips. The correction is used only to ask the question -- the model never sees it, since
the payoff of showing it a correction note has not been measured yet.

Endpoints:
  GET  /health                          -> {"ok": true, "model": ...}
  POST /check  <- {"texts": [...]}      -> {"p": [...], "fixes": [[{"word","suggest"}...]...], "ms": ...}

Runs on the GPU in bf16 (~6.5 GB). The memory fraction is capped so a fault is a clean CUDA OOM rather than an
over-allocation on the GB10's unified memory.
"""

import json
import logging
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
from transformers import AutoModel

MODEL = os.environ.get("VOICEBOX_GUARD_MODEL", os.path.expanduser("~/models-prod/d1-3B"))
PORT = int(os.environ.get("VOICEBOX_GUARD_PORT", "8124"))
QUESTION = {"nonsense": {"type": "noul", "instructions": "Is this statement nonsense?"}}
MAX_TEXTS, MAX_CHARS = 64, 8000
# The spelling checker lives with the experiment that measured it; voicebox imports it rather than keeping a copy that
# could drift from the measured version.
TYPO_SRC = os.environ.get("VOICEBOX_TYPO_SRC", os.path.expanduser("~/gguf-quant-lab/experiments/laya-guard"))
TYPO_MODEL = os.environ.get("VOICEBOX_TYPO_MODEL", os.path.expanduser("~/models-prod/modernbert-base"))

log = logging.getLogger("guard")
_lock = threading.Lock()  # one batch on the GPU at a time

torch.cuda.set_per_process_memory_fraction(0.08)
model = AutoModel.from_pretrained(MODEL, trust_remote_code=True, dtype=torch.bfloat16).to("cuda").eval()
# Remote-code models on transformers 5.x can load with a zeroed rope buffer and still answer plausibly.
for name, buf in model.named_buffers():
    if name.endswith("inv_freq") and int((buf != 0).sum()) == 0:
        raise SystemExit(f"zeroed rope buffer {name}: {MODEL} did not load correctly")


speller = None
try:
    sys.path.insert(0, TYPO_SRC)
    import typo_check
    typo_check.MODEL = TYPO_MODEL
    speller = typo_check.TypoChecker(device="cuda")
except Exception as e:  # noqa: BLE001 -- the guard still works without step 1, just with more false fires
    log.warning("spelling step disabled: %s", e)


def corrected(text):
    """The text with each flagged word replaced by the checker's suggestion, and the fixes made."""
    if speller is None:
        return text, []
    fixes = sorted(speller.check(text), key=lambda f: f["at"], reverse=True)
    for f in fixes:
        text = text[:f["at"]] + f["suggest"] + text[f["at"] + len(f["word"]):]
    return text, [{"word": f["word"], "suggest": f["suggest"]} for f in reversed(fixes)]


def check(texts):
    with _lock, torch.inference_mode():
        fixed = [corrected(t[:MAX_CHARS]) for t in texts]
        out = model.system_one_batch([(t, QUESTION) for t, _ in fixed])
    return [round(float(o["answers"]["nonsense"]["noul"]), 4) for o in out], [f for _, f in fixed]


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            return self._json(200, {"ok": True, "model": MODEL, "spelling": speller is not None})
        self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/check":
            return self._json(404, {"error": "not found"})
        try:
            req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            texts = req["texts"]
            if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts) or len(texts) > MAX_TEXTS:
                return self._json(400, {"error": f"texts must be a list of at most {MAX_TEXTS} strings"})
        except (ValueError, KeyError) as e:
            return self._json(400, {"error": str(e)})
        t0 = time.time()
        p, fixes = check(texts) if texts else ([], [])
        ms = round((time.time() - t0) * 1000, 1)
        log.info("checked %d text(s) in %.1f ms, max p %.3f, fixes %s", len(texts), ms, max(p, default=0),
                 [f"{x['word']}->{x['suggest']}" for fx in fixes for x in fx])
        self._json(200, {"p": p, "fixes": fixes, "ms": ms})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    check(["warm-up"])
    log.info("guard on 127.0.0.1:%d (%s)", PORT, MODEL)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
