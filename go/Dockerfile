# syntax=docker/dockerfile:1
#
# Build context is the "go/" directory, not the repository root:
#   docker build -t gorge-render go/
#   docker build -t gorge-diff --build-arg SERVICE=gorge-diff go/
#
# A single Dockerfile serves every binary under ./cmd; SERVICE selects which.

ARG GO_VERSION=1.27
# The official golang image only publishes Alpine variants for the two most
# recent Alpine releases, so the builder tracks its own Alpine version
# (currently 3.24 — golang:1.27 has no -alpine3.22 tag). The final runtime image
# below still pins ALPINE_VERSION independently.
ARG BUILDER_ALPINE_VERSION=3.24
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION}-alpine${BUILDER_ALPINE_VERSION} AS builder

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
#
# /var/lib/gorge/files exists in every image although only gorge-file-storage's
# local disk backend writes it, and it exists in the *image* rather than being
# created at runtime for one reason: Docker initialises an empty named volume
# from the image's content at the mount point, ownership included. Without it
# the volume is created root-owned and the unprivileged user below cannot write
# a single file into it — which shows up as every upload failing on a service
# whose healthcheck is green.
RUN apk add --no-cache ca-certificates \
  && adduser -D -u 10001 gorge \
  && install -d -o gorge -g gorge -m 0755 /var/lib/gorge/files

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
