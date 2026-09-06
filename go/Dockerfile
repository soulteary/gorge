# syntax=docker/dockerfile:1
#
# Build context is the "go/" directory, not the repository root:
#   docker build -t gorge-render go/
#   docker build -t gorge-diff --build-arg SERVICE=gorge-diff go/
#
# A single Dockerfile serves every binary under ./cmd; SERVICE selects which.

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

ARG SERVICE=gorge-render

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

FROM alpine:${ALPINE_VERSION}

ARG SERVICE=gorge-render
ARG PORT=8140

# busybox already provides the wget the healthcheck below needs.
RUN apk add --no-cache ca-certificates \
  && adduser -D -u 10001 gorge

COPY --from=builder /out/service /usr/local/bin/${SERVICE}

# ENTRYPOINT cannot expand build args, so expose the binary under a stable name.
RUN ln -s /usr/local/bin/${SERVICE} /usr/local/bin/gorge-service

# HEALTHCHECK runs through /bin/sh, so it reads this at container runtime.
ENV GORGE_HEALTHCHECK_PORT=${PORT}
EXPOSE ${PORT}

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${GORGE_HEALTHCHECK_PORT}/healthz" || exit 1

USER gorge

ENTRYPOINT ["gorge-service"]
