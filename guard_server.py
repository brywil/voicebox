#!/usr/bin/env python3
"""Preflight guard: a decision model asks one question of each user message.

Liquid's d1-3B answers "Is this statement nonsense?" as a probability, in one forward pass and with no output tokens.
voicebox appends a short note to a message the guard scores at or above its threshold. Why this question and this
model: measured in gguf-quant-lab/experiments/liquid-d1 and laya-guard -- the plain question beat a long definitional
one on held-out data (AUROC 0.963 vs 0.907), fired on 0-5% of ordinary math, code and instruction prompts, and on
Ornith a gated note took BullshitBench 0.86 -> ~0.90 for ~0.03 on sound questions.

One request checks many texts as one batch, so a conversation's history after a restart costs one call, not one per
message.

Endpoints:
  GET  /health                          -> {"ok": true, "model": ...}
  POST /check  <- {"texts": [...]}      -> {"p": [...], "ms": ...}

Runs on the GPU in bf16 (~6.5 GB). The memory fraction is capped so a fault is a clean CUDA OOM rather than an
over-allocation on the GB10's unified memory.
"""

import json
import logging
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
from transformers import AutoModel

MODEL = os.environ.get("VOICEBOX_GUARD_MODEL", os.path.expanduser("~/models-prod/d1-3B"))
PORT = int(os.environ.get("VOICEBOX_GUARD_PORT", "8124"))
QUESTION = {"nonsense": {"type": "noul", "instructions": "Is this statement nonsense?"}}
MAX_TEXTS, MAX_CHARS = 64, 8000

log = logging.getLogger("guard")
_lock = threading.Lock()  # one batch on the GPU at a time

torch.cuda.set_per_process_memory_fraction(0.08)
model = AutoModel.from_pretrained(MODEL, trust_remote_code=True, dtype=torch.bfloat16).to("cuda").eval()
# Remote-code models on transformers 5.x can load with a zeroed rope buffer and still answer plausibly.
for name, buf in model.named_buffers():
    if name.endswith("inv_freq") and int((buf != 0).sum()) == 0:
        raise SystemExit(f"zeroed rope buffer {name}: {MODEL} did not load correctly")


def check(texts):
    with _lock, torch.inference_mode():
        out = model.system_one_batch([(t[:MAX_CHARS], QUESTION) for t in texts])
    return [round(float(o["answers"]["nonsense"]["noul"]), 4) for o in out]


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
            return self._json(200, {"ok": True, "model": MODEL})
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
        p = check(texts) if texts else []
        ms = round((time.time() - t0) * 1000, 1)
        log.info("checked %d text(s) in %.1f ms, max p %.3f", len(texts), ms, max(p, default=0))
        self._json(200, {"p": p, "ms": ms})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    check(["warm-up"])
    log.info("guard on 127.0.0.1:%d (%s)", PORT, MODEL)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
