# syntax=docker/dockerfile:1
#
# whisper.cpp HTTP server compiled with CUDA for Blackwell (sm_120).
# Blackwell requires CUDA >= 12.8; building without CMAKE_CUDA_ARCHITECTURES=120
# fails at runtime with "no kernel image is available" (CLAUDE.md §20).

ARG CUDA_VERSION=12.8.1
ARG UBUNTU_VERSION=24.04

FROM nvidia/cuda:${CUDA_VERSION}-devel-ubuntu${UBUNTU_VERSION} AS build
ARG WHISPER_VERSION=v1.9.5
ARG CUDA_ARCHS=120
RUN apt-get update \
 && apt-get install -y --no-install-recommends git cmake build-essential ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch "${WHISPER_VERSION}" https://github.com/ggml-org/whisper.cpp.git .
# Static ggml so the runtime image only needs the CUDA runtime libraries.
RUN cmake -B build \
      -DCMAKE_BUILD_TYPE=Release \
      -DGGML_CUDA=1 \
      -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHS}" \
      -DBUILD_SHARED_LIBS=OFF \
      -DWHISPER_BUILD_TESTS=OFF \
 && cmake --build build -j"$(nproc)" --config Release --target whisper-server whisper-cli

FROM nvidia/cuda:${CUDA_VERSION}-runtime-ubuntu${UBUNTU_VERSION}
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates libgomp1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/build/bin/whisper-server /src/build/bin/whisper-cli /usr/local/bin/
COPY --from=build /src/models/download-ggml-model.sh /usr/local/bin/download-ggml-model.sh
COPY --from=build /src/samples/jfk.wav /usr/local/share/whisper/jfk.wav
COPY deploy/whisper/gpu-check.sh /usr/local/bin/gpu-check
RUN chmod 0755 /usr/local/bin/download-ggml-model.sh /usr/local/bin/gpu-check \
 && useradd --system --uid 10001 --create-home whisper \
 && mkdir -p /models && chown whisper /models
USER whisper
WORKDIR /home/whisper
EXPOSE 8080
ENV WHISPER_MODEL=/models/ggml-large-v3-turbo.bin
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
  CMD curl -fsS http://localhost:8080/health || exit 1
ENTRYPOINT ["whisper-server"]
# --dtw improves word timestamps (see docs/decisions/0012). Flash attention is
# disabled because whisper.cpp does not support DTW together with it.
CMD ["--host", "0.0.0.0", "--port", "8080", "-m", "/models/ggml-large-v3-turbo.bin", "-l", "auto", "--dtw", "large.v3.turbo", "-nfa"]
