"""
Thin FastAPI server that mimics the Ollama /api/generate surface so Dispatch's
existing Go aiclass client can hit it without a new code path. Only the fields
Dispatch actually uses are honored: model (ignored — this server is single-
model), prompt, images (base64-encoded PNGs), options.num_predict, format.

Not a general-purpose Ollama clone — purpose-built for PaddleOCR-VL on CPU.
"""

from __future__ import annotations

import base64
import io
import json
import time
from typing import Any

import torch
from fastapi import FastAPI
from PIL import Image
from pydantic import BaseModel, Field
from transformers import AutoModelForCausalLM, AutoProcessor

MODEL_ID = "PaddlePaddle/PaddleOCR-VL"

# Load once at import time. Pre-downloaded during Docker build so this is a
# disk→RAM mmap, not a network pull. Float32 on CPU for accuracy; float16 on
# CPU is usually slower.
print(f"[paddle-cpu] loading {MODEL_ID}...", flush=True)
_t0 = time.time()
model = AutoModelForCausalLM.from_pretrained(
    MODEL_ID, trust_remote_code=True, torch_dtype=torch.float32
).eval()
processor = AutoProcessor.from_pretrained(MODEL_ID, trust_remote_code=True)
print(f"[paddle-cpu] model loaded in {time.time() - _t0:.1f}s", flush=True)


class GenerateRequest(BaseModel):
    model: str | None = None
    prompt: str = ""
    images: list[str] = Field(default_factory=list)
    stream: bool = False
    think: bool = False
    format: str | None = None
    options: dict[str, Any] = Field(default_factory=dict)


app = FastAPI(title="PaddleOCR-VL CPU server")


@app.get("/api/tags")
def tags():
    return {"models": [{"name": MODEL_ID, "size": 0, "size_vram": 0}]}


@app.get("/api/ps")
def ps():
    return {"models": [{"name": MODEL_ID, "size": 0, "size_vram": 0}]}


@app.post("/api/generate")
def generate(req: GenerateRequest) -> dict[str, Any]:
    imgs: list[Image.Image] = []
    for b64 in req.images:
        imgs.append(Image.open(io.BytesIO(base64.b64decode(b64))).convert("RGB"))

    # Build messages for the processor. PaddleOCR-VL expects an image + text
    # turn; we embed all supplied images in a single user turn so ours matches
    # the single-image pattern Dispatch uses today.
    content: list[dict[str, Any]] = []
    for img in imgs:
        content.append({"type": "image", "image": img})
    content.append({"type": "text", "text": req.prompt})
    messages = [{"role": "user", "content": content}]

    inputs = processor.apply_chat_template(
        messages,
        add_generation_prompt=True,
        tokenize=True,
        return_dict=True,
        return_tensors="pt",
    )

    max_new = int(req.options.get("num_predict", 1500))
    temperature = float(req.options.get("temperature", 0.0))
    do_sample = temperature > 0

    t0 = time.time()
    with torch.inference_mode():
        out = model.generate(
            **inputs,
            max_new_tokens=max_new,
            do_sample=do_sample,
            temperature=temperature if do_sample else 1.0,
        )
    elapsed_ns = int((time.time() - t0) * 1e9)

    # Strip the prompt tokens — generate() returns them prefixed to the new ones.
    input_len = inputs["input_ids"].shape[-1]
    new_tokens = out[0, input_len:]
    text = processor.decode(new_tokens, skip_special_tokens=True)

    # If the caller asked for JSON formatting, try to trim anything around a
    # JSON object (model sometimes narrates). Best-effort; caller coerces too.
    if req.format == "json":
        start = text.find("{")
        end = text.rfind("}")
        if 0 <= start < end:
            text = text[start : end + 1]

    return {
        "model": MODEL_ID,
        "response": text,
        "done": True,
        "total_duration": elapsed_ns,
        "eval_count": int(new_tokens.shape[-1]),
    }
