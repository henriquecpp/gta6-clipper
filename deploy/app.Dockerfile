# syntax=docker/dockerfile:1
#
# Stages:
#   build        compiles the static Go binary (CGO_ENABLED=0)
#   integration  Go toolchain + ffmpeg + fonts, used by `task test:integration` / `task test:gpu`
#   runtime      final image: binary + ffmpeg (NVENC) + Streamlink + fonts, non-root

ARG GO_VERSION=1.27.4

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/clipper ./cmd/clipper

FROM golang:${GO_VERSION}-trixie AS integration
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg fonts-noto-core fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
CMD ["go", "test", "-race", "-tags", "integration", "./..."]

FROM python:3.13-slim-trixie AS runtime
ARG STREAMLINK_VERSION=8.6.3
# Debian's ffmpeg is built with ffnvcodec (h264_nvenc) and libass. The NVIDIA
# encode library itself is injected by the container toolkit when the
# `video` capability is requested (docker-compose.yml).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg fonts-noto-core fonts-dejavu-core ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "streamlink==${STREAMLINK_VERSION}"
RUN useradd --system --uid 10001 --create-home clipper \
 && mkdir -p /data /buffer /pending /output \
 && chown clipper:clipper /data /buffer /pending /output \
 && chmod 0700 /data
COPY --from=build /out/clipper /usr/local/bin/clipper
# Speech samples with TTS ground truth, used by `clipper gpucheck`.
COPY testdata/speech /usr/local/share/clipper/speech
USER clipper
WORKDIR /home/clipper
ENV NVIDIA_DRIVER_CAPABILITIES=video,compute,utility
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["clipper", "healthcheck"]
ENTRYPOINT ["clipper"]
CMD ["run"]
