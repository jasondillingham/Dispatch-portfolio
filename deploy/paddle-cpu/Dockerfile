# PaddleOCR-VL CPU inference server.
#
# Bundles: Python 3.11, PyTorch (CPU build), transformers + dependencies,
# and the PaddleOCR-VL-1.5 weights. Serves an Ollama-compatible /api/generate
# endpoint so Dispatch's existing aiclass.Client can hit it without a new
# client type — just another URL in the round-robin pool.
#
# Size on disk: ~5 GB (torch + transformers + model weights).
# Memory when running: ~3-5 GB resident depending on concurrent requests.
# Meant to be stacked: run N of these on different cpusets + ports.

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HF_HOME=/opt/hf-cache \
    TRANSFORMERS_CACHE=/opt/hf-cache \
    OMP_NUM_THREADS=4
# Runtime-only env: pass HF_HUB_OFFLINE=1 via `docker run -e` so the image
# can still pull weights at build time while forcing offline mode once
# deployed behind the --internal-style firewall.

WORKDIR /app

# PyTorch CPU build (much smaller than the CUDA one, ~200MB vs 2+GB).
# torchvision is a runtime dep of PaddleOCR-VL's image processor, not just
# a dev/training thing — its check_imports() fails at load without it.
RUN pip install --no-cache-dir \
        torch==2.5.1 torchvision==0.20.1 --index-url https://download.pytorch.org/whl/cpu

# Transformers + image/server deps.
# transformers pinned to 4.55.0: PaddleOCR-VL's custom modeling code was
# written against a narrow window — it imports `use_kernel_forward_from_hub`
# (~4.50+), `transformers.masking_utils.create_causal_mask` (~4.53+),
# AND ROPE_INIT_FUNCTIONS['default'] (removed in the ~4.56 rope refactor).
# 4.55.0 is the highest version that has all three.
RUN pip install --no-cache-dir \
        "transformers==4.56.0" \
        accelerate \
        sentencepiece \
        protobuf \
        pillow \
        fastapi \
        uvicorn[standard] \
        einops \
        safetensors

# Pre-pull the model at build time so the container starts fast. This bakes
# ~2 GB into the image but means every container launch is zero-delay instead
# of waiting on HF download. Accept trust_remote_code since PaddleOCR-VL ships
# custom model files.
RUN python -c "\
from transformers import AutoModelForCausalLM, AutoProcessor; \
import torch; \
AutoModelForCausalLM.from_pretrained('PaddlePaddle/PaddleOCR-VL', trust_remote_code=True, torch_dtype=torch.float32); \
AutoProcessor.from_pretrained('PaddlePaddle/PaddleOCR-VL', trust_remote_code=True)"

# Patch the custom modeling file: Paddle's upstream code calls
# `create_causal_mask(inputs_embeds=...)` but every released transformers
# signature takes `input_embeds` (no s). Rename that one kwarg ONLY in the
# create_causal_mask() call — the inner self.model() call a few lines down
# uses `inputs_embeds=` correctly and must NOT be touched.
RUN find /opt/hf-cache/modules/transformers_modules -name modeling_paddleocr_vl.py \
    -exec sed -i '/create_causal_mask(/,/^\s*)/ s/^\(\s*\)inputs_embeds=inputs_embeds,$/\1input_embeds=inputs_embeds,/' {} \; \
    && echo "=== verifying patch ===" \
    && grep -n "input_embeds=inputs_embeds\|inputs_embeds=inputs_embeds" $(find /opt/hf-cache/modules/transformers_modules -name modeling_paddleocr_vl.py | head -1)

COPY server.py /app/server.py

EXPOSE 11434

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "11434", "--workers", "1"]
