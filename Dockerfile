# Builds an EPP image that is the stock llm-d-router endpoint picker plus the
# request-tracer plugin compiled in (cmd/epp-with-tracer). It mirrors upstream's
# Dockerfile.epp so the result is a drop-in replacement for the stock epp image.
#
# The llm-d-router source is pulled as a Go module dependency (go mod download);
# this repo only contributes cmd/ and requesttracer/. To build against a fork or
# a local router checkout, add a `replace` to go.mod before building (see README).

# BASE_IMAGE can be overridden, e.g.:
#   --build-arg BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-micro:9.7
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot

# Go build stage. go 1.26.6 to satisfy llm-d-router v0.10.0's go directive.
FROM --platform=${BUILDPLATFORM} golang:1.27.1 AS go-builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Module manifests first so the dependency-download layer caches independently
# of source changes.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# This module's only source.
COPY cmd/          cmd/
COPY requesttracer/ requesttracer/

# Precompile without version flags so commit-only changes reuse the expensive
# compile work from the Docker layer cache.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -o /dev/null ./cmd/epp-with-tracer

# Build with the llm-d-router version metadata (the embedded version reflects the
# router the binary is built against, matching stock epp images).
ARG COMMIT_SHA=unknown
ARG BUILD_REF
ARG LDFLAGS="-s -w"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="${LDFLAGS} -X github.com/llm-d/llm-d-router/version.CommitSHA=${COMMIT_SHA} -X github.com/llm-d/llm-d-router/version.BuildRef=${BUILD_REF}" \
    -o bin/epp ./cmd/epp-with-tracer

# Runtime stage
FROM ${BASE_IMAGE}

WORKDIR /

COPY --from=go-builder /workspace/bin/epp /app/epp

USER 65532:65532

# gRPC, health and metrics ports (same as stock epp)
EXPOSE 9002
EXPOSE 9003
EXPOSE 9090
# KV-Events ZMQ SUB socket
EXPOSE 5557

ENTRYPOINT ["/app/epp"]
