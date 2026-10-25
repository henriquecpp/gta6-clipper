#!/bin/sh
# Proves that whisper.cpp runs on the GPU with a kernel image for sm_120.
# Runs whisper-cli on the bundled sample and inspects the CUDA init log.
set -eu

MODEL="${WHISPER_MODEL:-/models/ggml-large-v3-turbo.bin}"
SAMPLE="${1:-/usr/local/share/whisper/jfk.wav}"

if [ ! -f "$MODEL" ]; then
  echo "FAIL: model not found at $MODEL (run: task whisper:model)" >&2
  exit 1
fi

echo "== nvidia-smi"
nvidia-smi --query-gpu=name,driver_version,compute_cap,memory.total --format=csv,noheader || true

echo "== whisper-cli on $SAMPLE"
LOG="$(mktemp)"
if ! whisper-cli -m "$MODEL" -f "$SAMPLE" -l auto 2>"$LOG" >"$LOG.out"; then
  cat "$LOG" >&2
  echo "FAIL: whisper-cli exited with error" >&2
  exit 1
fi
cat "$LOG.out"

if grep -qi "no kernel image" "$LOG"; then
  echo "FAIL: CUDA kernels not built for this GPU (sm_120)" >&2
  exit 1
fi
if ! grep -Eq "compute capability 12\.0" "$LOG"; then
  grep -i -E "cuda|gpu|backend" "$LOG" >&2 || true
  echo "FAIL: CUDA device with compute capability 12.0 not found in whisper log" >&2
  exit 1
fi
if ! grep -Eq "using CUDA[0-9]+ backend" "$LOG"; then
  grep -i -E "cuda|gpu|backend" "$LOG" >&2 || true
  echo "FAIL: whisper did not select the CUDA backend" >&2
  exit 1
fi
grep -E "compute capability|ARCHS|using CUDA" "$LOG" | sed "s/| WHISPER.*CUDA : /| CUDA : /" | head -n 5
echo "OK: whisper.cpp is using CUDA on an sm_120 device"
