# Vision-Model Research Notes

Research captured 2026-04-20 while evaluating alternatives to `minicpm-v:latest` as the primary invoice-extraction model. Dispatch currently runs:

- **Primary**: `minicpm-v:latest` (~5.5 GB) on ai-02 (RTX 4070 Laptop, 8 GB VRAM) — fits pure-GPU, ~6-20s per verify
- **Fallback**: `gemma4:26b` (~18 GB) on ai-03 (CPU R730, 188 GB RAM) — triggers on primary error or zero-matches, ~2 min per verify

## Candidate 1: olmOCR 2 (Ai2 / Allen AI)

**What it is**: Fine-tune of Qwen2.5-VL-7B-Instruct trained on `olmOCR-mix-1025` with unit-test rewards for document OCR. Purpose-built for the same thing we care about — reading documents accurately.

**Specs**:
- 7B parameters
- 9.5 GB at Q8_0 (wouldn't fit pure-GPU on our 8 GB card; partial offload)
- Q4 GGUFs on HuggingFace ~5-6 GB (should fit pure-GPU)
- 125K token context window
- Available on Ollama as `richardyoung/olmocr2:7b-q8` (community, not official)

**Benchmark**: 82.4 on olmOCR-Bench. Beats Marker (76.1), MinerU (75.8), and general-purpose VLMs.

**Why it's interesting**: Drop-in replacement for minicpm in our current pipeline. Ollama-compatible, so no runtime changes. If accuracy is meaningfully better, we keep the two-stage architecture and just swap primary.

**Open questions we'd answer with an A/B**:
- Does it handle 3M-style edge cases (tiny credit memos) better than minicpm?
- Does it follow the JSON schema more reliably? (minicpm tends to emit prose preambles and dollar-signed numbers — we coerce around both, but cleaner output = less brittle code.)
- Q4 vs Q8 speed/accuracy tradeoff on 8 GB card.

**Test results 2026-04-20 — Q8 is too large for 8 GB VRAM:**
- On ai-01 (RX 580 / Vulkan): GPU reset watchdog killed runner mid-inference on all 3 samples at ~108-122s. Took out the X session too.
- On ai-02 (4070 Laptop / CUDA): `cudaMalloc failed: out of memory` at CLIP encoder allocation (~1.3 GB), even with no other models loaded. Model weights (6.1 GB) + KV (0.4 GB) + compute graph (0.9 GB) + CLIP (1.3 GB) = 8.7 GB, exceeds 7.9 GB usable. Failed in 3-17s.

**Conclusion**: Q8 doesn't fit either 8 GB card. To revisit: pull a Q4 GGUF variant (~5 GB weights) and manually import to Ollama, or move to hardware with ≥12 GB VRAM.

**Next step**: pull `richardyoung/olmocr2:7b-q8` on ai-02, run the same 3-sample A/B harness we ran for llava/minicpm:
```
./bin/verify-test -url http://<gpu-host-1>:11434 -po 1235547 -model richardyoung/olmocr2:7b-q8
./bin/verify-test -url http://<gpu-host-1>:11434 -po 1235306 -model richardyoung/olmocr2:7b-q8
./bin/verify-test -url http://<gpu-host-1>:11434 -po 1227498 -model richardyoung/olmocr2:7b-q8
```

If Q8 runs too slow (partial offload), try a Q4 GGUF pull.

## Candidate 2: PaddleOCR-VL 0.9B (Baidu)

**What it is**: Ultra-compact 0.9B-parameter vision-language model, purpose-built for document parsing. Released October 2025 by Baidu's PaddlePaddle team.

**Specs**:
- 0.9 billion parameters (≈10× smaller than our current primary)
- 3-4 GB VRAM with optimization
- Lightweight post-processing module emits structured Markdown + JSON (cross-page table merging, heading-hierarchy refinement)
- 109 languages

**Benchmark**: 94.5% on OmniDocBench v1.5 (with PaddleOCR-VL-1.5). #1 on the global leaderboard. Beats:
- GPT-4o
- Gemini 2.5 Pro
- Qwen2.5-VL-72B

That's a 0.9B model beating a 72B model on document parsing. If the numbers hold on real AP mail, it's a generational change.

**The catch**: requires PaddlePaddle runtime, not Ollama. Integration paths:
- Python service exposing HTTP API
- ONNX Runtime export
- C++ serving binary
- PaddleServing (their native deployment layer)

AMD has a ROCm guide for running it, which is promising for ai-01 (RX 580) if that box comes back online.

**Why it's worth the infra work**:
- Frees the GPU for other models (or lets us run 2-3 instances for concurrent load)
- Sub-second per-page inference likely (0.9B is tiny)
- Purpose-built for structured document output — should eliminate the JSON-prose problem we've been coercing around
- Would flip the tier structure: PaddleOCR-VL primary → minicpm-v secondary → gemma4:26b tertiary

**Integration plan (if we commit)**:
1. Stand up a Python service on ai-02 (or ai-01 w/ ROCm) with PaddleOCR-VL loaded
2. Wrap it in a small HTTP handler that matches our existing Ollama-like API surface (POST /api/verify with {image_b64, expected_lines})
3. Add a `paddlepaddle` backend type in `internal/aiclass` so the worker can call either Ollama or Paddle transparently
4. Promote to primary in `cmd/dispatch-worker/main.go`

**Risk**: the benchmark claim is extraordinary. Must validate on our invoices before ship. A 1-day spike is appropriate: build the Python service, wire it to verify-test, run the existing A/B, compare.

## Recommendation

**Short-term** (this week): olmOCR 2 A/B against minicpm. 30 min of work, uses existing tooling. If it's better, swap primary. Ship.

**Medium-term** (next 2 weeks): 1-day spike on PaddleOCR-VL. If the benchmark translates to our invoice types, it replaces both minicpm and gemma4:26b as primary — possibly eliminating the need for a fallback entirely. The infrastructure cost is real but the ceiling is much higher.

## Sources

- [olmOCR 2 blog — Ai2](https://allenai.org/blog/olmocr-2)
- [olmocr2 on Ollama](https://ollama.com/richardyoung/olmocr2)
- [olmOCR-2-7B-1025 on HuggingFace](https://huggingface.co/allenai/olmOCR-2-7B-1025)
- [PaddleOCR-VL arxiv paper](https://arxiv.org/abs/2510.14528)
- [PaddleOCR-VL 1.5 writeup — Towards AI](https://medium.com/@mustafa.gencc94/paddleocr-vl-1-5-a-deep-dive-into-the-0-9b-model-that-outperforms-gpt-4o-on-document-parsing-c93bac97ac1f)
- [AMD ROCm PaddleOCR-VL guide](https://www.amd.com/en/developer/resources/technical-articles/2026/unlocking-high-performance-document-parsing-of-paddleocr-vl-1-5-.html)
